#!/usr/bin/env python3
"""Local-only deployment regression matrix: no cluster, Docker daemon, or Go build.

Run after `helm dependency build deploy/charts/proxcloud`:
  python3 deploy/tests/test_durable_jobs.py
Requires helm, docker compose, PyYAML, Jinja2 and ansible-playbook on PATH.
"""
import base64
import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import jinja2
import yaml

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "deploy/charts/proxcloud"
INSTALLER = ROOT / "installer"
DEFAULTS = yaml.safe_load((INSTALLER / "playbooks/group_vars/all/defaults.yml").read_text())["pc_defaults"]


def run(args, *, env=None):
    result = subprocess.run(args, cwd=ROOT, env=env, capture_output=True, text=True, timeout=120)
    return result


def render(values=None, *, upgrade=False, values_file=None, error=None):
    with tempfile.TemporaryDirectory(prefix="pc-jobs-chart-") as directory:
        path = Path(directory) / "values.yaml"
        path.write_text(yaml.safe_dump(values or {}))
        args = ["helm", "template", "regression", str(CHART), "-n", "pc-test",
                "--set", "api.encryptionKey=validation-only", "--set", "postgresql.auth.password=validation-only",
                "-f", str(path)]
        if upgrade:
            args.append("--is-upgrade")
        if values_file:
            args.extend(["-f", str(CHART / values_file)])
        result = run(args)
        if error:
            assert result.returncode != 0, result.stdout
            assert error in result.stderr, result.stderr
            return []
        assert result.returncode == 0, result.stderr
        return [doc for doc in yaml.safe_load_all(result.stdout) if doc]


def api_env(documents):
    api = next(doc for doc in documents if doc["kind"] == "Deployment" and doc["metadata"]["name"].endswith("-api"))
    return {entry["name"]: entry for entry in api["spec"]["template"]["spec"]["containers"][0]["env"]}


def api_secret(documents):
    return next(doc for doc in documents if doc["kind"] == "Secret" and doc["metadata"]["name"].endswith("-api"))["stringData"]


def migration(mode="retain"):
    return {"mode": mode, "evidence": "ops-ticket/verified-queue-and-snapshot", "producersStopped": True,
            "consumersStopped": True, "drainedOrReconciled": True, "retentionPrepared": mode == "cutover"}


class ChartTests(unittest.TestCase):
    def test_default_broker_absent_primary_only(self):
        docs = render()
        env = api_env(docs)
        self.assertFalse(any(doc["kind"] == "RabbitmqCluster" for doc in docs))
        self.assertFalse(any("rabbitmq" in doc["metadata"]["name"] for doc in docs))
        self.assertFalse(any(name.startswith("PROXCLOUD_RABBITMQ_") for name in env))
        self.assertNotIn("PROXCLOUD_JOBS_RABBIT_FANOUT", env)
        self.assertNotIn("PROXCLOUD_JOBS_SCHEDULER_ENABLED", env)
        self.assertNotIn("PROXCLOUD_JOBS_DEBUG_TRIGGERS", env)
        self.assertNotIn("PROXCLOUD_DB_READ_URL", env)
        self.assertEqual(env["PROXCLOUD_JOBS_WORKER_CONCURRENCY"]["value"], "2")
        self.assertEqual(env["PROXCLOUD_JOBS_POLL_INTERVAL"]["value"], "1s")
        self.assertEqual(env["PROXCLOUD_JOBS_LEASE_DURATION"]["value"], "1m")
        secret = api_secret(docs)
        self.assertIn("@regression-proxcloud-pg-rw:", secret["db-url"])
        self.assertNotIn("db-read-url", secret)
        self.assertNotIn("rabbitmq-password", secret)

    def test_dev_and_quickstart_broker_absent(self):
        for filename in ("values-dev.yaml", "values-quickstart.yaml"):
            with self.subTest(filename=filename):
                docs = render({"keycloakConfigCli": {"adminPassword": "validation-only"}}, values_file=filename)
                self.assertFalse(any("rabbitmq" in doc["metadata"]["name"] for doc in docs))
                self.assertNotIn("PROXCLOUD_DB_READ_URL", api_env(docs))

    def test_existing_secret_missing_optional_read_key_is_compatible(self):
        docs = render({"api": {"existingSecret": "old-api-secret"}})
        env = api_env(docs)
        self.assertEqual(env["PROXCLOUD_DB_URL"]["valueFrom"]["secretKeyRef"], {"name": "old-api-secret", "key": "db-url"})
        self.assertEqual(env["PROXCLOUD_DB_READ_URL"]["valueFrom"]["secretKeyRef"],
                         {"name": "old-api-secret", "key": "db-read-url", "optional": True})
        self.assertFalse(any(name.startswith("PROXCLOUD_RABBITMQ_") for name in env))

    def test_explicit_cnpg_reader_does_not_change_primary_or_seed(self):
        docs = render({"cnpg": {"instances": 2, "statusReadReplica": True}})
        secret = api_secret(docs)
        self.assertIn("@regression-proxcloud-pg-rw:", secret["db-url"])
        self.assertIn("@regression-proxcloud-pg-ro:", secret["db-read-url"])
        seed = next(doc for doc in docs if doc["kind"] == "Job" and doc["metadata"]["name"].endswith("-seed"))
        self.assertEqual([e["name"] for e in seed["spec"]["template"]["spec"]["containers"][0]["env"]], ["PROXCLOUD_DB_URL"])
        render({"cnpg": {"statusReadReplica": True}}, error="instances>=2")

    def test_external_primary_and_reader_secrets_without_inline_credentials(self):
        docs = render({"cnpg": {"enabled": False}, "externalPostgresql": {"existingSecret": "primary", "secretKey": "writer",
                       "readExistingSecret": "replica", "readSecretKey": "reader"},
                       "externalRabbitmq": {"host": "obsolete", "existingSecret": "obsolete"}})
        env = api_env(docs)
        self.assertEqual(env["PROXCLOUD_DB_URL"]["valueFrom"]["secretKeyRef"], {"name": "primary", "key": "writer"})
        self.assertEqual(env["PROXCLOUD_DB_READ_URL"]["valueFrom"]["secretKeyRef"], {"name": "replica", "key": "reader"})
        self.assertNotIn("db-url", api_secret(docs))
        self.assertNotIn("rabbitmq-password", api_secret(docs))

    def test_worker_values_and_concurrency_bounds(self):
        env = api_env(render({"api": {"jobs": {"workerConcurrency": 16, "pollInterval": "100ms", "leaseDuration": "10m"}}}))
        self.assertEqual(env["PROXCLOUD_JOBS_WORKER_CONCURRENCY"]["value"], "16")
        self.assertEqual(env["PROXCLOUD_JOBS_POLL_INTERVAL"]["value"], "100ms")
        self.assertEqual(env["PROXCLOUD_JOBS_LEASE_DURATION"]["value"], "10m")
        for value in (0, 17):
            render({"api": {"jobs": {"workerConcurrency": value}}}, error="1..16")

    def test_legacy_retained_cr_annotations_and_exact_spec(self):
        spec = {"replicas": 3, "image": "rabbitmq:3-management", "persistence": {"storage": "20Gi"},
                "rabbitmq": {"additionalConfig": "cluster_partition_handling = pause_minority"}}
        docs = render({"jobsMigration": migration(), "rabbitmqCluster": {"enabled": True, "name": "old-broker", "retainedSpec": spec}}, upgrade=True)
        cr = next(doc for doc in docs if doc["kind"] == "RabbitmqCluster")
        self.assertEqual(cr["metadata"]["name"], "old-broker")
        self.assertEqual(cr["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertEqual(cr["metadata"]["annotations"]["argocd.argoproj.io/sync-options"], "Prune=false,Delete=false")
        self.assertEqual(cr["spec"], spec)
        self.assertFalse(any(name.startswith("PROXCLOUD_RABBITMQ_") for name in api_env(docs)))

    def test_old_subchart_is_explicit_retention_only(self):
        render({"rabbitmq": {"enabled": True, "auth": {"password": "validation-only"}}}, error="retention-only")
        docs = render({"jobsMigration": migration(), "rabbitmq": {"enabled": True, "auth": {"password": "validation-only"}}}, upgrade=True)
        self.assertTrue(any(doc["kind"] == "StatefulSet" and "rabbitmq" in doc["metadata"]["name"] for doc in docs))
        self.assertNotIn("rabbitmq-password", api_secret(docs))

    def test_upgrades_fail_without_explicit_evidenced_migration_choice(self):
        render(upgrade=True, error="existing releases require")
        render({"rabbitmqCluster": {"enabled": True}}, error="retention-only")
        render({"jobsMigration": {"mode": "retain"}, "rabbitmqCluster": {"enabled": True}}, error="NOT a drain proof")
        incomplete = migration("cutover")
        incomplete["retentionPrepared"] = False
        render({"jobsMigration": incomplete}, upgrade=True, error="retentionPrepared")
        render({"jobsMigration": migration()}, upgrade=True, error="must keep")
        docs = render({"jobsMigration": migration("cutover")}, upgrade=True)
        self.assertFalse(any(doc["kind"] == "RabbitmqCluster" for doc in docs))

    def test_true_legacy_flag_never_silently_cleared(self):
        render({"api": {"jobs": {"rabbitFanout": True}}}, error="migration error")
        render({"api": {"applicationYml": "proxcloud:\n  jobs:\n    rabbit-fanout: true\n"}}, error="migration error")
        render({"api": {"extraEnv": [{"name": "PROXCLOUD_JOBS_RABBIT_FANOUT", "value": "true"}]}}, error="must be cleared")


class ComposeTests(unittest.TestCase):
    def test_default_and_explicit_legacy_profile(self):
        clean = {k: v for k, v in os.environ.items() if not k.startswith(("PROXCLOUD_", "COMPOSE_"))}
        clean["COMPOSE_DISABLE_ENV_FILE"] = "1"
        args = ["docker", "compose", "-f", str(ROOT / "docker-compose.yml")]
        result = run(args + ["config", "--format", "json"], env=clean)
        self.assertEqual(result.returncode, 0, result.stderr)
        config = json.loads(result.stdout)
        self.assertNotIn("rabbitmq", config["services"])
        api = config["services"]["api"]
        self.assertNotIn("rabbitmq", api["depends_on"])
        self.assertEqual(api["environment"]["PROXCLOUD_DB_READ_URL"], "")
        self.assertEqual(api["environment"]["PROXCLOUD_JOBS_SCHEDULER_ENABLED"], "false")
        self.assertEqual(api["environment"]["PROXCLOUD_JOBS_DEBUG_TRIGGERS"], "false")
        self.assertFalse(any(k.startswith("PROXCLOUD_RABBITMQ_") for k in api["environment"]))
        result = run(args + ["--profile", "legacy-rabbitmq", "config", "--format", "json"], env=clean)
        self.assertEqual(result.returncode, 0, result.stderr)
        broker = json.loads(result.stdout)["services"]["rabbitmq"]
        self.assertEqual(broker["image"], "rabbitmq:3-management")
        self.assertNotIn("volumes", broker)  # preserve historical image-owned anonymous data volume


class InstallerTests(unittest.TestCase):
    def template(self, filename, config=None, brokers=None):
        env = jinja2.Environment(loader=jinja2.FileSystemLoader(INSTALLER / "playbooks/templates"), undefined=jinja2.StrictUndefined)
        env.filters.update(to_json=json.dumps, b64encode=lambda text: base64.b64encode(str(text).encode()).decode())
        variables = dict(pc=config or copy.deepcopy(DEFAULTS), pc_chart_repo="registry.test/charts", pc_chart_version="0.0.0-test",
                         pc_image_prefix="registry.test/proxcloud", pc_image_tag="test", pc_url_app="https://app.example.org",
                         pc_url_api="https://api.example.org", pc_url_admin="https://admin.example.org", pc_url_id="https://id.example.org",
                         pc_domain="example.org", pc_ver={"keycloak": "26.8.2"}, pc_legacy_brokers=brokers or [],
                         pc_db_password="validation-only", pc_encryption_key="validation-only", pc_sso_client_secret="validation-only",
                         pc_keycloak_db_password="validation-only", pc_keycloak_admin_password="validation-only")
        return [doc for doc in yaml.safe_load_all(env.get_template(filename).render(**variables)) if doc]

    def test_fresh_application_no_broker_and_optional_replica_secret(self):
        app = self.template("app-proxcloud.yml.j2")[0]
        values = app["spec"]["source"]["helm"]["valuesObject"]
        self.assertFalse(values["rabbitmqCluster"]["enabled"])
        self.assertFalse(values["rabbitmq"]["enabled"])
        self.assertFalse(values["api"]["jobs"]["schedulerEnabled"])
        self.assertFalse(values["api"]["jobs"]["debugTriggers"])
        self.assertEqual(values["api"]["jobs"]["workerConcurrency"], 2)
        docs = self.template("secrets.yml.j2")
        secret = next(doc for doc in docs if doc["metadata"]["name"] == "proxcloud-secrets")
        self.assertNotIn("db-read-url", secret["data"])
        self.assertIn("@proxcloud-pg-rw:", base64.b64decode(secret["data"]["db-url"]).decode())
        config = copy.deepcopy(DEFAULTS)
        config["database"] = {"instances": 2, "status_read_replica": True}
        docs = self.template("secrets.yml.j2", config)
        secret = next(doc for doc in docs if doc["metadata"]["name"] == "proxcloud-secrets")
        self.assertIn("@proxcloud-pg-ro:", base64.b64decode(secret["data"]["db-read-url"]).decode())
        self.assertIn("@proxcloud-pg-rw:", base64.b64decode(secret["data"]["db-url"]).decode())

    def gate(self, *, app=None, brokers=None, decision=None, forbidden=False, settings=None, release="v0.4.0", statefulsets=None, rabbit_crd=True):
        if not shutil.which("ansible-playbook"):
            self.fail("ansible-playbook required on PATH for migration gate regression")
        play = yaml.safe_load((INSTALLER / "playbooks/40-mgmt-platform.yml").read_text())[0]
        tasks = []
        for task in play["tasks"]:
            if task["name"] == "Install ArgoCD through the k3s helm controller":
                break
            tasks.append(task)
        config = copy.deepcopy(DEFAULTS)
        if decision:
            config["jobs_migration"].update(decision)
        for key, value in (settings or {}).items():
            config[key].update(value)
        with tempfile.TemporaryDirectory(prefix="pc-jobs-installer-") as directory:
            path = Path(directory)
            mock = path / "k3s"
            mock.write_text("""#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
if os.environ.get('MOCK_FORBIDDEN') == '1':
    print('Forbidden: cannot read CRDs', file=sys.stderr)
    sys.exit(1)
if 'crds' in args:
    names = ['applications.argoproj.io']
    if os.environ['MOCK_RABBIT_CRD'] == '1':
        names.append('rabbitmqclusters.rabbitmq.com')
    print(json.dumps({'items': [{'metadata': {'name': name}} for name in names]}))
elif 'application' in args:
    print(os.environ['MOCK_APP'])
elif 'rabbitmqclusters' in args:
    print(json.dumps({'items': json.loads(os.environ['MOCK_BROKERS'])}))
elif 'statefulsets' in args:
    print(json.dumps({'items': json.loads(os.environ['MOCK_STATEFULSETS'])}))
else:
    print('UNEXPECTED MUTATION OR COMMAND: ' + repr(args), file=sys.stderr)
    sys.exit(99)
""")
            mock.chmod(0o755)
            fixture = [{"name": "Migration gate only (mocked reads)", "hosts": "localhost", "gather_facts": False,
                        "vars": {"pc": config, "ns": "proxcloud", "pc_ver": {"proxcloud": release}}, "tasks": tasks}]
            playbook = path / "gate.yaml"
            playbook.write_text(yaml.safe_dump(fixture, sort_keys=False))
            env = dict(os.environ, PATH=str(path) + os.pathsep + os.environ["PATH"], MOCK_APP=json.dumps(app) if app else "",
                       MOCK_BROKERS=json.dumps(brokers or []), MOCK_FORBIDDEN="1" if forbidden else "0",
                       MOCK_RABBIT_CRD="1" if rabbit_crd else "0", MOCK_STATEFULSETS=json.dumps(statefulsets or []),
                       ANSIBLE_CONFIG=str(INSTALLER / "ansible.cfg"), ANSIBLE_LOCAL_TEMP=str(path / "ansible-local"))
            return run(["ansible-playbook", "-i", "localhost,", "-c", "local", str(playbook)], env=env)

    def test_invalid_config_refused_before_discovery_or_any_apply(self):
        cases = [
            ({"database": {"status_read_replica": True, "instances": 1}}, "instances >= 2"),
            ({"jobs": {"worker_concurrency": 0}}, "worker_concurrency"),
            ({"jobs": {"worker_concurrency": 17}}, "worker_concurrency"),
            ({"jobs": {"worker_concurrency": True}}, "worker_concurrency"),
            ({"jobs": {"poll_interval": "99ms"}}, "poll_interval"),
            ({"jobs": {"poll_interval": "61s"}}, "poll_interval"),
            ({"jobs": {"lease_duration": "9s"}}, "lease_duration"),
            ({"jobs": {"lease_duration": "11m"}}, "lease_duration"),
            ({"jobs": {"poll_interval": "bad"}}, "poll_interval"),
        ]
        for settings, error in cases:
            with self.subTest(settings=settings):
                # Forbidden fake discovery proves invalid config failed BEFORE even the first read,
                # not merely after a Secret/Application mutation or a later Helm render.
                result = self.gate(settings=settings, forbidden=True)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn(error, result.stdout)
                self.assertNotIn("Discover existing platform", result.stdout)
                self.assertNotIn("UNEXPECTED MUTATION", result.stdout)
        result = self.gate(settings={"images": {"build": False}}, release="v0.3.29", forbidden=True)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("published broker-free release", result.stdout)
        self.assertNotIn("Discover existing platform", result.stdout)

    def test_migration_gate_fresh_pass_existing_without_proof_fails(self):
        result = self.gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        result = self.gate(app={"spec": {}})
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("drain/reconcile", result.stdout)

    def test_migration_gate_rejects_unprotected_cr_and_forbidden_discovery(self):
        decision = {"mode": "retain", "evidence": "ops-ticket", "producers_stopped": True, "consumers_stopped": True,
                    "drained_or_reconciled": True}
        result = self.gate(brokers=[{"metadata": {"name": "proxcloud-rabbitmq"}}], decision=decision)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("LIVE RabbitmqCluster", result.stdout)
        result = self.gate(forbidden=True)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Forbidden", result.stdout)

    def test_migration_gate_retains_exact_existing_cr_and_cert_manager(self):
        annotations = {"helm.sh/resource-policy": "keep", "argocd.argoproj.io/sync-options": "Prune=false,Delete=false"}
        broker = {"metadata": {"name": "proxcloud-rabbitmq", "annotations": annotations},
                  "spec": {"replicas": 3, "persistence": {"storage": "20Gi"}}}
        decision = {"mode": "retain", "evidence": "ops-ticket", "producers_stopped": True, "consumers_stopped": True,
                    "drained_or_reconciled": True}
        result = self.gate(app={"spec": {}}, brokers=[broker], decision=decision)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        config = copy.deepcopy(DEFAULTS)
        config["jobs_migration"].update(decision)
        values = self.template("app-proxcloud.yml.j2", config, [broker])[0]["spec"]["source"]["helm"]["valuesObject"]
        self.assertEqual(values["rabbitmqCluster"]["retainedSpec"], broker["spec"])
        operators = (INSTALLER / "playbooks/templates/apps-operators.yml.j2").read_text()
        self.assertIn("cert-manager", operators)
        stage = (INSTALLER / "playbooks/40-mgmt-platform.yml").read_text()
        self.assertNotIn("rabbit_operator_url", stage)
        self.assertNotIn("Install the RabbitMQ", stage)

    def test_ambiguous_argo_sources_and_broker_without_crd_fail_before_apply(self):
        sources = [
            {"source": {"helm": {"values": "rabbitmq:\\n  enabled: true"}}},
            {"source": {"helm": {"parameters": [{"name": "rabbitmq.enabled", "value": "true"}]}}},
            {"source": {"helm": {"valueFiles": ["old.yaml"]}}},
            {"sources": [{"helm": {"valuesObject": {"rabbitmq": {"enabled": True}}}}]},
        ]
        for spec in sources:
            with self.subTest(spec=spec):
                result = self.gate(app={"spec": spec}, rabbit_crd=False)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("separate manual broker retention review", " ".join(result.stdout.split()))
                self.assertNotIn("Require an explicit and evidenced", result.stdout)
                self.assertNotIn("UNEXPECTED MUTATION", result.stdout)
        result = self.gate(statefulsets=[{"metadata": {"name": "old-broker", "labels": {"app.kubernetes.io/name": "rabbitmq"}}}], rabbit_crd=False)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("not owned by a discovered retained", result.stdout)
        self.assertNotIn("UNEXPECTED MUTATION", result.stdout)

    def test_migration_gate_cutover_and_old_subchart_rejection(self):
        decision = {"mode": "cutover", "evidence": "ops-ticket/no-legacy", "producers_stopped": True,
                    "consumers_stopped": True, "drained_or_reconciled": True, "retention_prepared": True}
        result = self.gate(app={"spec": {}}, decision=decision)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        app = {"spec": {"source": {"helm": {"valuesObject": {"rabbitmq": {"enabled": True}}}}}}
        result = self.gate(app=app, decision=decision)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Bitnami", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
