"""CI smoke: register a digest without Kaniko, prepare, create, exec/files and cleanup.

Run on an approved self-hosted runner inside the VPN, never a public GitHub runner.
Required: E2B_API_KEY, SANDBOX_IMAGE (immutable linux/amd64 manifest), optional E2B_DOMAIN.
For a operator-approved private source: REGISTRY_USERNAME and REGISTRY_PASSWORD.
No credential values are printed. Registration does not delete the CI source image.
"""

import json
import os
import time
import urllib.error
import urllib.request
import uuid

from e2b import Sandbox

API = os.environ.get("E2B_API_URL", "https://api.example.org/e2b").rstrip("/")
KEY = os.environ["E2B_API_KEY"]
IMAGE = os.environ["SANDBOX_IMAGE"]
NAME = "ci-digest-" + uuid.uuid4().hex[:12]
DEADLINE = time.monotonic() + 1800


def api(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        API + path,
        data=data,
        method=method,
        headers={"X-API-Key": KEY, "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=45) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def wait_until(action, done):
    while time.monotonic() < DEADLINE:
        value = action()
        if done(value):
            return value
        time.sleep(3)
    raise TimeoutError("sandbox CI verification exceeded its overall budget")


def cleanup_template(template_id, names, *, created):
    """Remove only this run's names when registration reused a shared artifact.

    Concurrent CI jobs can receive the same template/build. One job must not delete the
    artifact or primary name while another is still using it; a reused job owns aliases only.
    """
    try:
        if created:
            current = api("GET", f"/templates/{template_id}")
            foreign_names = set(current.get("names", [])) - set(names)
            if not foreign_names:
                api("DELETE", f"/templates/{template_id}")
                return
            # Another run has adopted the same artifact. Keep the primary record rather
            # than invalidating its in-flight create; remove only aliases owned here.
            primary = current.get("names", [None])[0]
            names = [name for name in names if name != primary]
        if names:
            api("DELETE", f"/templates/{template_id}/aliases", {"aliases": names})
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise


def main():
    if "@sha256:" not in IMAGE:
        raise ValueError("SANDBOX_IMAGE must be an immutable platform manifest digest")
    request = {"source": IMAGE, "name": NAME, "cpuCount": 1, "memoryMB": 2048}
    user, password = os.getenv("REGISTRY_USERNAME"), os.getenv("REGISTRY_PASSWORD")
    if bool(user) != bool(password):
        raise ValueError("supply both registry credentials or neither")
    if user:
        request["registry"] = {"username": user, "password": password}
    imported = api("POST", "/templates/import", request)
    template_id, build_id = imported["templateID"], imported["buildID"]
    sandbox = None
    alias = NAME + "-alias"
    try:
        def status():
            value = api("GET", f"/templates/{template_id}/builds/{build_id}/status")
            if value["status"] == "error":
                # Do not echo arbitrary worker output into CI logs.
                raise RuntimeError("digest verification failed; inspect project template status")
            return value

        wait_until(status, lambda value: value["status"] == "ready")
        repeated = api("POST", "/templates/import", dict(request, name=alias))
        assert repeated["templateID"] == template_id and repeated["buildID"] == build_id
        assert repeated["reused"], "same artifact/settings must not start another import"
        print("ok: immutable digest registration and alias reuse")
        coverage = api("POST", f"/templates/{template_id}/prepare")
        if coverage.get("enabled"):
            wait_until(
                lambda: api("GET", f"/templates/{template_id}/prepare"),
                lambda value: value.get("ready") is True,
            )
            print("ok: observed image-cache coverage")
        else:
            print("note: warming disabled; create uses bounded cold-start wait")
        sandbox = Sandbox.create(alias, timeout=180, request_timeout=930)
        result = sandbox.commands.run("printf '%s' \"$E2B_SANDBOX_ID\"")
        assert result.stdout == sandbox.sandbox_id
        sandbox.files.write("/tmp/proxcloud-ci-digest.txt", "digest-import-ok")
        assert sandbox.files.read("/tmp/proxcloud-ci-digest.txt") == "digest-import-ok"
        print("ok: usable sandbox, authoritative ID, exec and files")
    finally:
        if sandbox is not None:
            sandbox.kill()
        cleanup_template(template_id, [NAME, alias], created=not imported.get("reused", False))


if __name__ == "__main__":
    main()
