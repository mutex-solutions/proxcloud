#!/usr/bin/env python3
"""Seed what an operator needs before customers can use a Proxmox provider, through the
admin API: platform configuration + region, flavor catalog, billing configuration, public
price plan + rules, provider default quota. Idempotent: existing documents are updated,
never duplicated. Register the provider first (admin console or POST /admin/service).

    deploy/seed/seed-operator.py --provider "My Proxmox"

Environment: PROXCLOUD_HOST (localhost; API :8080, Keycloak :8090), or PROXCLOUD_API and
PROXCLOUD_TOKEN_URL in full, plus PROXCLOUD_ADMIN_USER / PROXCLOUD_ADMIN_PASSWORD (admin/admin,
the dev realm's operator) and PROXCLOUD_ADMIN_CLIENT (proxcloud-admin).
"""
import argparse, json, os, pathlib, sys, urllib.error, urllib.parse, urllib.request

HERE = pathlib.Path(__file__).resolve().parent
HOST = os.environ.get("PROXCLOUD_HOST", "localhost")
API = os.environ.get("PROXCLOUD_API", f"http://{HOST}:8080/api/v1")
TOKEN_URL = os.environ.get(
    "PROXCLOUD_TOKEN_URL", f"http://{HOST}:8090/realms/proxcloud/protocol/openid-connect/token"
)
# Resource types the platform bills today; the seed file also prices managed services. The sandbox rules are
# placeholders (their names say so) until a sandbox rate is signed.
RULE_TYPES = {"instance", "instance_traffic", "volume", "floating_ip", "image", "bucket", "share", "server_protection",
              "server_snapshot", "sandbox", "sandbox_snapshot"}
DEFAULT_QUOTA = {"servers": 20, "vcpus": 80, "ramMb": 102400, "diskGb": 2000, "networks": 10, "floatingIps": 5}


def token():
    form = urllib.parse.urlencode({
        "grant_type": "password",
        "client_id": os.environ.get("PROXCLOUD_ADMIN_CLIENT", "proxcloud-admin"),
        "username": os.environ.get("PROXCLOUD_ADMIN_USER", "admin"),
        "password": os.environ.get("PROXCLOUD_ADMIN_PASSWORD", "admin"),
    }).encode()
    return json.load(urllib.request.urlopen(TOKEN_URL, form))["access_token"]


class Admin:
    def __init__(self, tok):
        self.tok = tok

    def __call__(self, method, path, body=None, ok_404=False):
        req = urllib.request.Request(
            API + "/admin" + path, method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": "Bearer " + self.tok, "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as res:
                raw = res.read()
        except urllib.error.HTTPError as err:
            if ok_404 and err.code == 404:
                return None
            sys.exit(f"{method} {path}: {err.code} {err.read().decode()[:400]}")
        return json.loads(raw).get("data") if raw else None


def load(name):
    return json.loads((HERE / name).read_text())


def upsert_by_name(admin, path, name, doc):
    """PUT over the document called name in the list at path, POST it when absent."""
    found = next((d for d in admin("GET", path) or [] if d.get("name") == name), None)
    if found:
        return admin("PUT", f"{path}/{found['id']}", doc)
    return admin("POST", path, doc)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--provider", required=True, help="name of the registered Proxmox provider")
    args = ap.parse_args()
    admin = Admin(token())

    svc = next((s for s in admin("GET", "/service") or [] if s.get("name") == args.provider), None)
    if not svc:
        sys.exit(f'provider "{args.provider}" is not registered')
    cfg = svc.get("config") or {}
    region = cfg.get("region") or next(iter(cfg.get("regions") or {}), "")

    plat = load("platform-configuration.json")
    plat["regions"] = [{"serviceId": svc["id"], "region": region, "order": 0}]
    saved = upsert_by_name(admin, "/platform-configuration", plat["name"], plat)
    print(f"platform configuration {saved['id']} region {region}")

    for fl in load("flavors.json"):
        admin("PUT", f"/flavors/{fl['slug']}", fl)
    print("flavors:", ", ".join(f["slug"] for f in admin("GET", "/flavors")))

    bill = load("billing-configuration.json")
    current = admin("GET", "/billing/configuration/current", ok_404=True)
    saved = admin("PUT", f"/billing/configuration/{current['id']}", {**current, **bill}) if current \
        else admin("POST", "/billing/configuration", bill)
    print(f"billing configuration {saved['id']} ({saved.get('baseCurrency')})")

    seed = load("price-plan-seed.json")
    plan = upsert_by_name(admin, "/price-plan", seed["plan"]["name"], seed["plan"])
    existing = {r["name"]: r["id"] for r in admin("GET", f"/price-plan/{plan['id']}/rule") or []}
    rules = [r for r in seed["rules"] if r["resourceType"] in RULE_TYPES]
    for rule in rules:
        rule = {**rule, "pricePlanId": plan["id"]}
        if rule["name"] in existing:
            admin("PUT", f"/price-plan/rule/{existing[rule['name']]}", rule)
        else:
            admin("POST", "/price-plan/rule", rule)
    print(f"price plan {plan['id']}: {len(rules)} rules")

    admin("PUT", f"/service/{svc['id']}/quota", DEFAULT_QUOTA)
    print("default quota:", DEFAULT_QUOTA)


if __name__ == "__main__":
    main()
