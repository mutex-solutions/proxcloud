#!/usr/bin/env python3
"""Production Docker Compose install (deploy/compose): contract tests, standard library only.

  python3 deploy/tests/test_compose_production.py

No container is started and no daemon is needed. `helm` renders the chart templates the realm and seed
files come from (required when CI is set, skipped otherwise when absent); `docker compose config` checks
the rendered model when the Compose plugin is installed.

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


def read_env(directory):
    pairs = {}
    for line in (directory / ".env").read_text().splitlines():
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
        shutil.copytree(COMPOSE, self.dir, ignore=shutil.ignore_patterns(".env", "cf-token", "operator-password", "backups"))

    def run(self, *args, env=None):
        """Run the script without a terminal. PROXCLOUD_* and CF_* of the caller never leak in."""
        clean = {k: v for k, v in os.environ.items() if not k.startswith(("PROXCLOUD_", "CF_", "COMPOSE_"))}
        clean.update(env or {})
        return subprocess.run(["bash", str(self.dir / SCRIPT), *args], env=clean, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True, timeout=60)

    def install(self, *args, env=None):
        return self.run("install", "--dry-run", "--project-name", "pc-compose-test", *args, env=env)


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
                self.assertEqual(stat.S_IMODE((install.dir / ".env").stat().st_mode), 0o600)
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
                # The realms first, then everything else.
                self.assertLess(result.stdout.index("+ docker compose -p pc-compose-test up -d keycloak-config"),
                                result.stdout.index("+ docker compose -p pc-compose-test up -d --remove-orphans"))
                self.assertIn("ENCRYPTION_KEY", result.stderr)  # the back-it-up warning
                token = install.dir / "cf-token"
                self.assertEqual(token.exists(), mode == "acme-dns-cloudflare")
                if token.exists():
                    self.assertEqual(stat.S_IMODE(token.stat().st_mode), 0o600)
                    self.assertNotIn("render-only-token", result.stdout + result.stderr)
                self.check_compose_model(install, mode, values)

    def check_compose_model(self, install, mode, values):
        if not shutil.which("docker") or subprocess.run(["docker", "compose", "version"], capture_output=True).returncode:
            return  # no Compose plugin: the .env checks above still ran
        # .env selects the TLS profile; "setup" adds the realm import the script runs by name.
        result = subprocess.run(["docker", "compose", "--profile", mode, "--profile", "setup", "config", "--format", "json"],
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

    def test_rerun_keeps_secrets_and_applies_changed_settings(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        before = read_env(install.dir)
        # A dry run creates no operator, so the email is still asked for; the rest comes from .env.
        again = install.install("--admin-host", "ops.example.com", "--https-port", "18443", "--http-port", "18080",
                                "--version", "v9.8.7", "--operator-email", "ops@example.com")
        self.assertEqual(again.returncode, 0, again.stderr)
        after = read_env(install.dir)
        for name in SECRETS:
            self.assertEqual(after[name], before[name])
        self.assertNotIn("ENCRYPTION_KEY", again.stderr)  # warned once, when it was generated
        self.assertEqual(after["ADMIN_HOST"], "ops.example.com")
        self.assertEqual(after["APP_HOST"], "app.cloud.example.com")  # kept from .env without --domain
        self.assertEqual((after["HTTPS_PORT"], after["URL_PORT"], after["PROXCLOUD_VERSION"]), ("18443", ":18443", "v9.8.7"))
        lines = (install.dir / ".env").read_text().splitlines()
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
        ]
        for args, message in cases:
            with self.subTest(args=args):
                result = Install(self).install(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertNotIn("+ docker", result.stdout)

    def test_changing_the_tls_mode_removes_the_previous_traefik_first(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        result = install.install("--tls", "acme-http", "--acme-email", "acme@example.com", "--http-port", "8080",
                                 "--operator-email", "ops@example.com")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("public port 80", result.stderr)  # HTTP-01 behind a forwarded port: warned, not refused
        self.assertLess(result.stdout.index("rm -sf traefik-internal"), result.stdout.index("up -d"))
        self.assertEqual(read_env(install.dir)["COMPOSE_PROFILES"], "acme-http")

    def test_project_name_is_fixed_after_the_first_install(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
        result = install.run("install", "--dry-run", "--project-name", "other")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot change", result.stderr)

    def test_other_commands_need_an_install_and_print_their_docker_commands(self):
        install = Install(self)
        self.assertIn("install first", install.run("status", "--dry-run").stderr)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)

        self.assertIn("+ docker compose -p pc-compose-test ps", install.run("status", "--dry-run").stdout)
        self.assertIn("pg_dumpall", install.run("backup", "--dry-run").stdout)

        self.assertIn("usage:", install.run("upgrade", "--dry-run").stderr)
        self.assertIn("invalid version", install.run("upgrade", "0.6.0", "--dry-run").stderr)
        upgrade = install.run("upgrade", "v0.6.0", "--dry-run")
        self.assertEqual(upgrade.returncode, 0, upgrade.stderr)
        self.assertLess(upgrade.stdout.index("pg_dumpall"), upgrade.stdout.index("pull"))  # backup first
        self.assertLess(upgrade.stdout.index("pull"), upgrade.stdout.index("up -d keycloak-config"))
        self.assertEqual(read_env(install.dir)["PROXCLOUD_VERSION"], "v0.6.0")

    def test_uninstall_asks_and_deletes_volumes_only_with_purge(self):
        install = Install(self)
        self.assertEqual(install.install(*BASE, "--tls", "internal").returncode, 0)
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


if __name__ == "__main__":
    unittest.main()
