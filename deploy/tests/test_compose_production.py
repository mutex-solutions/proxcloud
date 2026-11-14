#!/usr/bin/env python3
"""Production Docker Compose install (deploy/compose): contract tests, standard library only.

  python3 deploy/tests/test_compose_production.py

No container is started and no daemon is needed: the script runs with --dry-run, or against a fake
`docker` that records its calls. `helm` renders the chart templates the realm and seed files come from,
`docker compose config` checks the rendered model and `shellcheck` reads the script: all three are
required when CI is set, and skipped otherwise when absent.

After a change to the chart's realm or seed templates, regenerate the committed copies:

  UPDATE_COMPOSE_GENERATED=1 python3 deploy/tests/test_compose_production.py
"""
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "deploy/charts/proxcloud"
COMPOSE = ROOT / "deploy/compose"
SCRIPT = "proxcloud-up.sh"
TLS_MODES = ("internal", "acme-http", "acme-dns-cloudflare")
SECRETS = ("PG_PASSWORD", "KEYCLOAK_DB_PASSWORD", "KC_ADMIN_PASSWORD", "SSO_CLIENT_SECRET", "ENCRYPTION_KEY")

# The chart values the Compose install stands for: two realms (customers in "clients", operators in
# "master"), per-organization SSO on, PKCE required. The console URLs stay placeholders that
# keycloak-config-cli substitutes from its environment at import (compose.yaml sets them).
CHART_VALUES = {
    "keycloakConfigCli": {"enabled": True, "adminPassword": "render-only"},
    "auth": {
        "main": {"issuer": "https://id.invalid/realms/clients"},
        "admin": {"issuer": "https://id.invalid/realms/master"},
        "adminApi": {"issuer": "https://id.invalid/realms/master"},
        "sso": {"enabled": True, "clientSecret": "render-only"},
    },
    "api": {"selfUrls": {"ui": "$(env:PROXCLOUD_SELF_UI_BASE_URL)",
                         "admin": "$(env:PROXCLOUD_SELF_ADMIN_BASE_URL)"}},
}
# committed file -> (chart template, ConfigMap key)
GENERATED = {
    "keycloak-config/clients.json": ("templates/keycloak/config-cli.yaml", "clients.json"),
    "keycloak-config/master.json": ("templates/keycloak/config-cli.yaml", "master.json"),
    "seed/seed.sql": ("templates/api/seed-job.yaml", "seed.sql"),
}


def render_from_chart():
    """The generated files, rendered by helm from the chart's own templates.

    A copy of the chart without its subchart dependencies renders offline: these two templates
    read only the chart's values and files.
    """
    with tempfile.TemporaryDirectory(prefix="pc-compose-chart-") as tmp:
        chart = Path(tmp) / "proxcloud"
        chart.mkdir()
        (chart / "Chart.yaml").write_text((CHART / "Chart.yaml").read_text().split("\ndependencies:")[0] + "\n")
        shutil.copy(CHART / "values.yaml", chart)
        shutil.copytree(CHART / "files", chart / "files")
        templates = sorted({template for template, _ in GENERATED.values()})
        for template in ["templates/_helpers.tpl", *templates]:
            (chart / template).parent.mkdir(parents=True, exist_ok=True)
            shutil.copy(CHART / template, chart / template)
        values = Path(tmp) / "values.json"  # JSON is YAML
        values.write_text(json.dumps(CHART_VALUES))
        rendered = {}
        for template in templates:
            result = subprocess.run(["helm", "template", "pc", str(chart), "-f", str(values), "--show-only", template],
                                    capture_output=True, text=True, timeout=120)
            assert result.returncode == 0, result.stderr
            rendered[template] = result.stdout
    files = {}
    for path, (template, key) in GENERATED.items():
        # A ConfigMap block scalar: "  <key>: |" then its lines, indented by four.
        block = re.search(r"^  %s: \|\n((?:    .*\n|\n)*)" % re.escape(key), rendered[template], re.M)
        assert block, "%s not rendered by %s" % (key, template)
        files[path] = "".join(line[4:] if line.strip() else "\n" for line in block.group(1).splitlines(True))
    return files


DRY = ".env.dry-run"  # what a dry run writes in place of .env


def read_env(directory, name=DRY):
    pairs = {}
    for line in (directory / name).read_text().splitlines():
        if line and not line.startswith("#"):
            key, _, value = line.partition("=")
            pairs[key] = value
    return pairs


class Install:
    """A throwaway copy of deploy/compose: the script only ever writes next to itself."""

    def __init__(self, test):
        tmp = tempfile.TemporaryDirectory(prefix="pc-compose-test-")
        test.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name) / "compose"
        shutil.copytree(COMPOSE, self.dir, ignore=shutil.ignore_patterns(".env*", "cf-token", "operator-password", "backups"))
        self.log = Path(tmp.name) / "docker.log"
        self.bin = Path(tmp.name) / "bin"
        self.bin.mkdir()
        for name, body in (("docker", FAKE_DOCKER), ("curl", "#!/bin/sh\nprintf 200\n")):
            (self.bin / name).write_text(body)
            (self.bin / name).chmod(0o755)

    def run(self, *args, env=None):
        """Run the script without a terminal. PROXCLOUD_* and CF_* of the caller never leak in."""
        clean = {k: v for k, v in os.environ.items() if not k.startswith(("PROXCLOUD_", "CF_", "COMPOSE_"))}
        clean.update(env or {})
        return subprocess.run(["bash", str(self.dir / SCRIPT), *args], env=clean, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True, timeout=60)

    def install(self, *args, env=None):
        return self.run("install", "--dry-run", "--project-name", "pc-compose-test", *args, env=env)

    def adopt(self):
        """Make the last dry run's preview the install's .env, as the real run would have written it."""
        shutil.copy(self.dir / DRY, self.dir / ".env")

    def real(self, *args, env=None):
        """Run for real against the fake docker: everything but the containers happens."""
        return self.run(*args, env={"PATH": "%s:%s" % (self.bin, os.environ["PATH"]), "FAKE_LOG": str(self.log),
                                    **(env or {})})

    def calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []


# Every call is logged with the two variables a test exports to prove they do not reach Compose.
FAKE_DOCKER = r"""#!/bin/sh
echo "PROXCLOUD_VERSION=${PROXCLOUD_VERSION-} HTTP_PORT=${HTTP_PORT-} : $*" >> "$FAKE_LOG"
case "$*" in
  "version --format"*) echo 27.5.1 ;;
  "compose version --short") echo 2.32.4 ;;
  "ps -a --filter"*) [ -z "${FAKE_OTHER_DIR-}" ] || echo "$FAKE_OTHER_DIR" ;;
  "volume inspect"*) [ -n "${FAKE_VOLUME-}" ] ;;
  *" ps -q"*|*" ps -aq "*) echo c0ffee ;;
  inspect*Health*) echo healthy ;;
  inspect*) echo exited:0 ;;
  *" exec -T keycloak "*) cat > /dev/null; echo created ;;
esac
"""


BASE = ("--domain", "cloud.example.com", "--operator-email", "ops@example.com")


class GeneratedFromChartTests(unittest.TestCase):
    def test_realm_and_seed_files_equal_the_chart_render(self):
        if not shutil.which("helm"):
            if os.environ.get("CI"):
                self.fail("helm is required in CI, or this test proves nothing")
            self.skipTest("helm not installed")
        rendered = render_from_chart()
        if os.environ.get("UPDATE_COMPOSE_GENERATED"):
            for path, content in rendered.items():
                (COMPOSE / path).parent.mkdir(parents=True, exist_ok=True)
                (COMPOSE / path).write_text(content)
        for path, content in rendered.items():
            with self.subTest(path=path):
                self.assertEqual((COMPOSE / path).read_text(), content,
                                 "stale: run UPDATE_COMPOSE_GENERATED=1 python3 deploy/tests/test_compose_production.py")

    def test_realm_files_carry_no_site_value(self):
        clients = json.loads((COMPOSE / "keycloak-config/clients.json").read_text())
        master = json.loads((COMPOSE / "keycloak-config/master.json").read_text())
        self.assertEqual((clients["realm"], master["realm"]), ("clients", "master"))
        by_id = {c["clientId"]: c for realm in (clients, master) for c in realm["clients"]}
        self.assertEqual(by_id["proxcloud-ui"]["redirectUris"], ["$(env:PROXCLOUD_SELF_UI_BASE_URL)/*"])
        self.assertEqual(by_id["proxcloud-admin"]["redirectUris"], ["$(env:PROXCLOUD_SELF_ADMIN_BASE_URL)/*"])
        self.assertEqual(by_id["proxcloud-idp-admin"]["secret"], "$(env:PROXCLOUD_SSO_KC_CLIENT_SECRET)")
        # Both token endpoints are public and allow the password grant.
        self.assertTrue(clients["bruteForceProtected"] and master["bruteForceProtected"])
        # Every variable the import substitutes is one compose.yaml hands to keycloak-config-cli.
        used = set(re.findall(r"\$\(env:([A-Z_]+)\)", json.dumps([clients, master])))
        compose = (COMPOSE / "compose.yaml").read_text()
        for name in used:
            self.assertRegex(compose, r"(?m)^      %s: " % name)


class ComposeFileTests(unittest.TestCase):
    def setUp(self):
        self.text = (COMPOSE / "compose.yaml").read_text()

    def test_images_are_pinned_and_proxcloud_images_follow_the_release_variable(self):
        images = re.findall(r"(?m)^\s+image: (.+)$", self.text)
        self.assertEqual(len(images), 8)  # one Traefik (shared by the three TLS modes) + seven services
        for image in images:
            with self.subTest(image=image):
                if "/proxcloud-" in image:
                    self.assertRegex(image, r"^ghcr\.io/mutex-solutions/proxcloud-(api|web|admin):\$\{PROXCLOUD_VERSION(:\?[^}]*)?\}$")
                else:
                    self.assertRegex(image, r":v?\d+[\w.-]*$")
                    self.assertNotIn("latest", image)
        self.assertEqual({i for i in images if "/proxcloud-" not in i}, {
            "traefik:v3.7.18", "postgres:17-alpine", "quay.io/keycloak/keycloak:26.8.2",
            "docker.io/adorsys/keycloak-config-cli:6.5.1-26.5.5"})

    def test_no_default_credentials(self):
        for name in SECRETS:
            with self.subTest(secret=name):
                uses = re.findall(r"\$\{%s[^}]*\}" % name, self.text)
                self.assertTrue(uses)
                self.assertTrue(any(":?" in use for use in uses), "the first use must be required")
                for use in uses:
                    self.assertNotRegex(use, r"^\$\{%s:?-" % name)  # no fallback value
        self.assertNotRegex(self.text, r"(?i)password: (?!\$\{)\S")
        self.assertNotIn("name:", self.text.split("services:")[0])  # the project name comes from .env

    def test_no_password_is_a_command_argument(self):
        self.assertNotRegex(self.text, r"(?m)^\s+(entrypoint|command): .*(PASSWORD|DB_URL)")
        script = (COMPOSE / SCRIPT).read_text()
        self.assertNotRegex(script, r"--(new-)?password")
        self.assertIn("pg_isready -h 127.0.0.1", self.text)  # not the socket-only server of the first start


class ScriptLintTests(unittest.TestCase):
    def test_script_parses_and_is_shellcheck_clean(self):
        scripts = [str(COMPOSE / SCRIPT), str(COMPOSE / "postgres-init/10-keycloak.sh")]
        self.assertEqual(subprocess.run(["bash", "-n", scripts[0]], capture_output=True).returncode, 0)
        if not shutil.which("shellcheck"):
            if os.environ.get("CI"):
                self.fail("shellcheck is required in CI")
            self.skipTest("shellcheck not installed")
        result = subprocess.run(["shellcheck", *scripts], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout)


class ScriptDryRunTests(unittest.TestCase):
    def test_each_tls_mode_renders_env_and_compose_model(self):
        for mode in TLS_MODES:
            with self.subTest(mode=mode):
                install = Install(self)
                extra, env = [], {}
                if mode != "internal":
                    extra += ["--acme-email", "acme@example.com"]
                if mode == "acme-dns-cloudflare":
                    env["CF_DNS_API_TOKEN"] = "render-only-token"
                result = install.install(*BASE, "--tls", mode, *extra, env=env)
                self.assertEqual(result.returncode, 0, result.stderr)
                values = read_env(install.dir)
                self.assertEqual(stat.S_IMODE((install.dir / DRY).stat().st_mode), 0o600)
                self.assertFalse((install.dir / ".env").exists())  # a dry run writes the preview only
                self.assertEqual(values["COMPOSE_PROFILES"], mode)
                self.assertEqual(values["COMPOSE_PROJECT_NAME"], "pc-compose-test")
                self.assertEqual([values[k] for k in ("APP_HOST", "ADMIN_HOST", "API_HOST", "AUTH_HOST")],
                                 ["app.cloud.example.com", "admin.cloud.example.com", "api.cloud.example.com",
                                  "id.cloud.example.com"])
                self.assertEqual((values["HTTP_PORT"], values["HTTPS_PORT"], values["URL_PORT"]), ("80", "443", ""))
                secrets = [values[name] for name in SECRETS]
                self.assertEqual(len(set(secrets)), len(SECRETS))
                for secret in secrets:
                    self.assertRegex(secret, r"^[0-9a-f]{48,64}$")
                    self.assertNotIn(secret, result.stdout + result.stderr)
                self.assertNotIn("OPERATOR_EMAIL", values)  # recorded only once the operator exists
                # The realms first; then the Traefik of the other two modes goes, then everything else.
                others = " ".join("traefik-" + other for other in TLS_MODES if other != mode)
                steps = [result.stdout.index("+ docker compose -p pc-compose-test " + step) for step in
                         ("up -d keycloak-config", "rm -sf " + others, "up -d --remove-orphans")]
                self.assertEqual(steps, sorted(steps))
                self.assertFalse((install.dir / "cf-token").exists())
                self.assertNotIn("render-only-token", result.stdout + result.stderr)
                self.check_compose_model(install, mode, values)

    def check_compose_model(self, install, mode, values):
        if not shutil.which("docker") or subprocess.run(["docker", "compose", "version"], capture_output=True).returncode:
            if os.environ.get("CI"):
                self.fail("the Docker Compose plugin is required in CI")
            return  # no Compose plugin: the .env checks above still ran
        # .env selects the TLS profile; "setup" adds the realm import the script runs by name.
        result = subprocess.run(["docker", "compose", "--env-file", DRY, "--profile", mode, "--profile", "setup",
                                 "config", "--format", "json"],
                                cwd=install.dir, capture_output=True, text=True, timeout=60,
                                env={k: v for k, v in os.environ.items() if not k.startswith(("COMPOSE_", "PROXCLOUD_"))})
        self.assertEqual(result.returncode, 0, result.stderr)
        model = json.loads(result.stdout)
        self.assertEqual(model["name"], "pc-compose-test")
        services = model["services"]
        self.assertEqual(set(services), {"traefik-" + mode, "postgres", "keycloak", "keycloak-config", "seed",
                                         "api", "web", "admin"})
        traefik = services["traefik-" + mode]
        command = " ".join(traefik["command"])
        self.assertEqual(sorted((p["published"], p["target"]) for p in traefik["ports"]), [("443", 443), ("80", 80)])
        self.assertIn("Label(`com.docker.compose.project`,`pc-compose-test`)", command)
        self.assertEqual("certresolver=le" in command, mode != "internal")
        self.assertEqual("acme.email=acme@example.com" in command, mode != "internal")
        self.assertEqual("httpchallenge.entrypoint=web" in command, mode == "acme-http")
        self.assertEqual("dnschallenge.provider=cloudflare" in command, mode == "acme-dns-cloudflare")
        self.assertEqual("secrets" in traefik, mode == "acme-dns-cloudflare")
        for name in ("api", "web", "admin", "seed"):
            self.assertTrue(services[name]["image"].endswith(":" + values["PROXCLOUD_VERSION"]))
        api = services["api"]["environment"]
        self.assertEqual(api["PROXCLOUD_SELF_UI_BASE_URL"], "https://app.cloud.example.com")
        self.assertEqual(api["AUTH_MAIN_OAUTH2_ISSUER_URI"], "https://id.cloud.example.com/realms/clients")
        self.assertEqual(api["AUTH_ADMIN_OAUTH2_ISSUER_URI"], "https://id.cloud.example.com/realms/master")
        self.assertEqual(api["PROXCLOUD_ENCRYPTION_DEFAULT_KEY"], values["ENCRYPTION_KEY"])
        self.assertEqual(services["keycloak"]["environment"]["KC_HOSTNAME"], "https://id.cloud.example.com")
        config = services["keycloak-config"]["environment"]
        self.assertEqual(config["PROXCLOUD_SELF_ADMIN_BASE_URL"], "https://admin.cloud.example.com")
        self.assertEqual(config["PROXCLOUD_SSO_KC_CLIENT_SECRET"], api["PROXCLOUD_SSO_KC_CLIENT_SECRET"])
        self.assertEqual(services["admin"]["labels"]["traefik.http.routers.admin.rule"], "Host(`admin.cloud.example.com`)")
        for name, service in services.items():
            self.assertEqual(service["logging"]["options"]["max-size"], "10m", name)

    def test_rerun_keeps_secrets_and_applies_changed_settings(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        install.adopt()
        before = read_env(install.dir)
        # A dry run creates no operator, so the email is still asked for; the rest comes from .env.
        again = install.install("--admin-host", "ops.example.com", "--https-port", "18443", "--http-port", "18080",
                                "--version", "v9.8.7", "--operator-email", "ops@example.com")
        self.assertEqual(again.returncode, 0, again.stderr)
        after = read_env(install.dir)
        for name in SECRETS:
            self.assertEqual(after[name], before[name])
        self.assertEqual(read_env(install.dir, ".env"), before)  # the dry run left the install's .env alone
        self.assertEqual(after["ADMIN_HOST"], "ops.example.com")
        self.assertEqual(after["APP_HOST"], "app.cloud.example.com")  # kept from .env without --domain
        self.assertEqual((after["HTTPS_PORT"], after["URL_PORT"], after["PROXCLOUD_VERSION"]), ("18443", ":18443", "v9.8.7"))
        lines = (install.dir / DRY).read_text().splitlines()
        keys = [line.split("=")[0] for line in lines if line and not line.startswith("#")]
        self.assertEqual(len(keys), len(set(keys)))

    def test_environment_and_single_hostnames_work_without_flags(self):
        install = Install(self)
        result = install.install(env={
            "PROXCLOUD_APP_HOST": "Portal.Example.com", "PROXCLOUD_ADMIN_HOST": "ops.example.com",
            "PROXCLOUD_API_HOST": "api.example.com", "PROXCLOUD_AUTH_HOST": "login.example.com",
            "PROXCLOUD_TLS_MODE": "acme-http", "PROXCLOUD_ACME_EMAIL": "acme@example.com",
            "PROXCLOUD_OPERATOR_EMAIL": "ops@example.com"})
        self.assertEqual(result.returncode, 0, result.stderr)
        values = read_env(install.dir)
        self.assertEqual((values["APP_HOST"], values["AUTH_HOST"], values["ACME_EMAIL"]),
                         ("portal.example.com", "login.example.com", "acme@example.com"))

    def test_missing_or_invalid_input_fails_without_prompting(self):
        cases = [
            ((), "missing: --tls --app-host --admin-host --api-host --auth-host --operator-email"),
            ((*BASE, "--tls", "acme-http"), "missing: --acme-email"),
            ((*BASE, "--tls", "acme-dns-cloudflare", "--acme-email", "a@example.com"), "--cloudflare-token-file"),
            ((*BASE, "--tls", "letsencrypt"), "invalid TLS mode"),
            ((*BASE, "--tls", "internal", "--app-host", "bad host"), "invalid hostname"),
            ((*BASE, "--tls", "internal", "--app-host", "localhost"), "invalid hostname"),
            ((*BASE, "--tls", "internal", "--api-host", "app.cloud.example.com"), "must differ"),
            ((*BASE, "--tls", "internal", "--https-port", "70000"), "invalid --https-port"),
            ((*BASE, "--tls", "internal", "--version", "latest"), "invalid version"),
            (("--domain", "cloud.example.com", "--tls", "internal", "--operator-email", "nobody"), "invalid operator email"),
            ((*BASE, "--tls", "internal", "--bogus", "x"), "unknown option"),
            ((*BASE, "--tls"), "--tls needs a value"),
            ((*BASE, "--tls", "internal", "--purge"), "--purge belongs to uninstall"),
            ((*BASE, "--tls", "internal", "--cloudflare-token-file", "/dev/null"), "belongs to --tls acme-dns-cloudflare"),
        ]
        for args, message in cases:
            with self.subTest(args=args):
                install = Install(self)
                result = install.install(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertNotIn("+ docker", result.stdout)
                self.assertFalse((install.dir / DRY).exists())  # nothing is written before validation passes

    def test_changing_the_tls_mode_removes_the_previous_traefik_after_the_realm_import(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        install.adopt()
        result = install.install("--tls", "acme-http", "--acme-email", "acme@example.com", "--http-port", "8080",
                                 "--operator-email", "ops@example.com")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("public port 80", result.stderr)  # HTTP-01 behind a forwarded port: warned, not refused
        # A failed import must leave the old Traefik serving: it goes after the import, before the new one.
        steps = [result.stdout.index(step) for step in
                 ("up -d keycloak-config", "rm -sf traefik-internal traefik-acme-dns-cloudflare", "up -d --remove-orphans")]
        self.assertEqual(steps, sorted(steps))
        self.assertEqual(read_env(install.dir)["COMPOSE_PROFILES"], "acme-http")
        # The dry run did not record the new mode: the real run still has the old Traefik to remove.
        self.assertEqual(read_env(install.dir, ".env")["COMPOSE_PROFILES"], "internal")

    def test_project_name_defaults_away_from_the_development_stack_and_is_fixed_afterwards(self):
        install = Install(self)
        self.assertEqual(install.run("install", "--dry-run", *BASE, "--tls", "internal").returncode, 0)
        self.assertEqual(read_env(install.dir)["COMPOSE_PROJECT_NAME"], "proxcloud-prod")
        install.adopt()
        result = install.run("install", "--dry-run", "--project-name", "other")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot change", result.stderr)

    def test_other_commands_need_an_install_and_print_their_docker_commands(self):
        install = Install(self)
        self.assertIn("install first", install.run("status", "--dry-run").stderr)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        install.adopt()
        ignored = install.run("status", "--domain", "other.example.com", "--dry-run")
        self.assertNotEqual(ignored.returncode, 0)
        self.assertIn("status does not take --domain", ignored.stderr)

        self.assertIn("+ docker compose -p pc-compose-test ps", install.run("status", "--dry-run").stdout)
        self.assertIn("pg_dumpall", install.run("backup", "--dry-run").stdout)

        self.assertIn("usage:", install.run("upgrade", "--dry-run").stderr)
        self.assertIn("invalid version", install.run("upgrade", "0.6.0", "--dry-run").stderr)
        upgrade = install.run("upgrade", "v0.6.0", "--dry-run")
        self.assertEqual(upgrade.returncode, 0, upgrade.stderr)
        self.assertLess(upgrade.stdout.index("pg_dumpall"), upgrade.stdout.index("pull"))  # backup first
        self.assertLess(upgrade.stdout.index("pull"), upgrade.stdout.index("up -d keycloak-config"))
        self.assertEqual(read_env(install.dir)["PROXCLOUD_VERSION"], "v0.6.0")
        # The install's own .env keeps its release: the next plain install must not upgrade without a backup.
        self.assertNotEqual(read_env(install.dir, ".env")["PROXCLOUD_VERSION"], "v0.6.0")

    def test_uninstall_asks_and_deletes_volumes_only_with_purge(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        install.adopt()
        refused = install.run("uninstall", "--dry-run")  # no terminal, no --yes
        self.assertNotEqual(refused.returncode, 0)
        self.assertNotIn("+ docker", refused.stdout)
        refused = install.run("uninstall", "--purge", "--dry-run")
        self.assertNotEqual(refused.returncode, 0)
        self.assertNotIn("+ docker", refused.stdout)

        kept = install.run("uninstall", "--yes", "--dry-run")
        self.assertEqual(kept.returncode, 0, kept.stderr)
        self.assertIn("+ docker compose -p pc-compose-test --profile setup down --remove-orphans", kept.stdout)
        self.assertNotIn(" -v", kept.stdout)
        purged = install.run("uninstall", "--purge", "--yes", "--dry-run")
        self.assertIn("+ docker compose -p pc-compose-test --profile setup down -v", purged.stdout)
        self.assertTrue((install.dir / ".env").exists())  # secrets are never deleted by the script


class ScriptRealRunTests(unittest.TestCase):
    """The script run for real, against a fake docker (Install.real)."""

    ARGS = ("install", "--project-name", "pc-compose-test", "--domain", "cloud.example.com", "--tls", "internal",
            "--operator-email", "Ops@Example.com")

    def installed(self):
        install = Install(self)
        result = install.real(*self.ARGS)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("ENCRYPTION_KEY", result.stderr)  # the back-it-up warning, once
        return install

    def test_a_failed_first_run_does_not_disarm_the_project_guard(self):
        install = Install(self)
        failed = install.real(*self.ARGS[:-4], "--tls", "bogus", "--operator-email", "ops@example.com")
        self.assertIn("invalid TLS mode", failed.stderr)
        self.assertFalse((install.dir / ".env").exists())
        # The second run still sees the containers another directory created under this project name.
        taken = install.real(*self.ARGS, env={"FAKE_OTHER_DIR": "/srv/other-checkout"})
        self.assertNotEqual(taken.returncode, 0)
        self.assertIn("already has containers, created from '/srv/other-checkout'", taken.stderr)
        # A database volume without the .env it was created with is not adopted either.
        orphan = install.real(*self.ARGS, env={"FAKE_VOLUME": "1"})
        self.assertNotEqual(orphan.returncode, 0)
        self.assertIn("Restore your .env", orphan.stderr)
        self.assertFalse((install.dir / ".env").exists())
        self.assertFalse(any(" up -d" in call for call in install.calls()))
        # With its .env in place the same volume is this install's own.
        self.assertEqual(install.real(*self.ARGS).returncode, 0)
        self.assertEqual(install.real("install", env={"FAKE_VOLUME": "1"}).returncode, 0)

    def test_the_development_stack_project_name_is_refused(self):
        install = Install(self)  # .../compose under a temporary directory: make that its "repository root"
        (install.dir.parent.parent / "docker-compose.yml").write_text("services: {}\n")
        args = [install.dir.parent.parent.name if a == "pc-compose-test" else a for a in self.ARGS]
        result = install.real(*args)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("development stack", result.stderr)

    def test_install_writes_env_and_creates_the_operator_once(self):
        install = self.installed()
        values = read_env(install.dir, ".env")
        self.assertEqual(stat.S_IMODE((install.dir / ".env").stat().st_mode), 0o600)
        self.assertEqual(values["OPERATOR_EMAIL"], "ops@example.com")  # lower case, as Keycloak stores it
        self.assertEqual(stat.S_IMODE((install.dir / "operator-password").stat().st_mode), 0o600)
        again = install.real("install")
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertNotIn("ENCRYPTION_KEY", again.stderr)
        self.assertEqual(read_env(install.dir, ".env"), values)
        self.assertEqual(sum(" exec -T keycloak " in call for call in install.calls()), 1)
        # No password in any docker command line.
        for name in SECRETS:
            self.assertNotIn(values[name], install.log.read_text())
        self.assertNotIn((install.dir / "operator-password").read_text().strip(), install.log.read_text())

    def test_purge_forgets_the_operator_and_the_next_install_creates_it_again(self):
        install = self.installed()
        self.assertEqual(install.real("uninstall", "--purge", "--yes").returncode, 0)
        self.assertNotIn("OPERATOR_EMAIL", read_env(install.dir, ".env"))
        self.assertFalse((install.dir / "operator-password").exists())
        again = install.real("install", "--operator-email", "ops@example.com")
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertEqual(sum(" exec -T keycloak " in call for call in install.calls()), 2)
        self.assertTrue((install.dir / "operator-password").exists())

    def test_the_shell_environment_does_not_reach_compose(self):
        install = self.installed()
        installed = read_env(install.dir, ".env")["PROXCLOUD_VERSION"]
        result = install.real("upgrade", "v9.9.9", env={"PROXCLOUD_VERSION": "v0.0.1", "HTTP_PORT": "1"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(read_env(install.dir, ".env")["PROXCLOUD_VERSION"], "v9.9.9")
        compose = [call for call in install.calls() if " : compose -p pc-compose-test " in call]
        pulls = [call for call in compose if call.endswith(" pull")]
        self.assertEqual(pulls, ["PROXCLOUD_VERSION=v9.9.9 HTTP_PORT= : compose -p pc-compose-test pull"])
        for call in compose:
            if call not in pulls:
                self.assertTrue(call.startswith("PROXCLOUD_VERSION= HTTP_PORT= : "), call)  # .env decides
        self.assertNotEqual(installed, "v9.9.9")

    def test_a_finished_install_changes_release_only_through_upgrade(self):
        install = self.installed()
        before = len(install.calls())
        for env, args in (({}, ("--version", "v9.9.9")), ({"PROXCLOUD_VERSION": "v9.9.9"}, ())):
            result = install.real("install", *args, env=env)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("proxcloud-up.sh upgrade v9.9.9", result.stderr)
        self.assertFalse(any(" up -d" in call for call in install.calls()[before:]))
        self.assertNotEqual(read_env(install.dir, ".env")["PROXCLOUD_VERSION"], "v9.9.9")

    def test_the_cloudflare_token_is_written_by_a_real_run_only_and_never_shown(self):
        install = Install(self)
        (install.bin / "getent").write_text("#!/bin/sh\n[ \"$2\" != api.cloud.example.com ]\n")  # one name unresolved
        (install.bin / "getent").chmod(0o755)
        args = [*self.ARGS[:-4], "--tls", "acme-dns-cloudflare", "--acme-email", "acme@example.com",
                "--operator-email", "ops@example.com"]
        result = install.real(*args, env={"CF_DNS_API_TOKEN": "render-only-token"})
        self.assertEqual(result.returncode, 0, result.stderr)
        token = install.dir / "cf-token"
        self.assertEqual((token.read_text(), stat.S_IMODE(token.stat().st_mode)), ("render-only-token", 0o600))
        self.assertNotIn("render-only-token", result.stdout + result.stderr + install.log.read_text())
        self.assertIn("api.cloud.example.com does not resolve", result.stderr)
        self.assertNotIn("app.cloud.example.com does not resolve", result.stderr)

    def test_an_unreachable_front_door_is_a_warning_and_the_operator_is_still_created(self):
        install = Install(self)
        (install.bin / "curl").write_text("#!/bin/sh\nprintf 000\n")
        (install.bin / "sleep").write_text("#!/bin/sh\n")
        (install.bin / "sleep").chmod(0o755)
        (install.dir / SCRIPT).write_text((install.dir / SCRIPT).read_text().replace(
            'wait_for 120 "the API through Traefik"', 'wait_for 0 "the API through Traefik"'))
        result = install.real(*self.ARGS)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("no answer from the API", result.stderr)
        self.assertEqual(read_env(install.dir, ".env")["OPERATOR_EMAIL"], "ops@example.com")


if __name__ == "__main__":
    unittest.main()
