#!/usr/bin/env python3
"""Compare the pins of deploy/platform/platform-bom.yaml with what the registries serve (stdlib only).

Anonymous and read-only: no credentials, no secrets. One line per item, exit 1 on any failure.

What is checked, per kind of pin:
  oci    a chart in a bare OCI repo ({repo, name, version, digest}), an image (<repo>:<tag>@<digest>) or a VM image
         ({package, tag, digest}, under ghcr.io/mutex-solutions/): an anonymous pull token, then HEAD of the tag's
         manifest; its Docker-Content-Digest header must equal the pin. The manifest and its layers are not downloaded.
  index  a chart in an https:// repo: <repo>/index.yaml is fetched and the `digest` of the entry with that exact name
         and version must equal the pin. The .tgz itself is not downloaded.
  url    a download ({url, sha256}): the file is downloaded (at most --max-download-mb) and its sha256 must equal the
         pin.
A `pending` digest has nothing to compare and is reported as "skip"; with --release it fails, and so do a `0.0.0+`
version and a `-dev` release. The `release` line must be one plain version (quotes allowed), or the run fails.
Redirects: a manifest request is never redirected; a download follows https redirects only.

The BOM is read without a YAML library (the CI scripts are stdlib only): each pin is one line in one of the shapes
above, and a line that holds a pin in any other shape fails the run, so a pin cannot be skipped by being unreadable.
platformlife.Validate (Go) stays the judge of the BOM's schema.
"""
import argparse
import hashlib
import http.client
import json
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BOM = "deploy/platform/platform-bom.yaml"
VM_IMAGE_REPO = "ghcr.io/mutex-solutions"  # platformlife.VMImage: ghcr.io/mutex-solutions/<package>
ACCEPT = ", ".join((
    "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json"))
ATTEMPTS = 3
TIMEOUT = 30
MAX_SMALL = 64 << 20  # tokens and chart indexes (the largest index today is about 10 MiB)

_V = r'"?([^",{}\s]+)"?'
_CHART = re.compile(r"\{repo: %s, name: %s, version: %s, digest: %s\}" % (_V, _V, _V, _V))
_VM = re.compile(r"\{package: %s, tag: %s, digest: %s\}" % (_V, _V, _V))
_URL = re.compile(r"\{url: %s, sha256: %s\}" % (_V, _V))
_IMAGE = re.compile(r'^\s+\w+:\s+"?([^"@\s]+):([^"@\s:/]+)@([^"\s]+)"?\s*$')
_PIN = re.compile(r"digest:|\bsha256:\s|@(?:sha256:|pending)")  # every way a line can hold a pin
_RELEASE = re.compile(r"^release:(.*)$", re.M)
_VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?")
_COMMENT = re.compile(r"(^|\s)#.*$")  # no pin holds " #"


class VerifyError(Exception):
    pass


def parse_bom(text):
    """Return (release, items). An item is a dict: kind oci|index|url, ref, digest, version, line."""
    release = [_COMMENT.sub("", value).strip() for value in _RELEASE.findall(text)]
    if len(release) != 1:
        raise VerifyError("the BOM has %d release lines, want one" % len(release))
    release = release[0]
    if release[:1] in "'\"" and release[-1:] == release[:1]:
        release = release[1:-1]
    # Anything else (a YAML tag, an anchor, a half quote) could read as a -dev release in Go and not here.
    if not _VERSION.fullmatch(release):
        raise VerifyError("the BOM's release %r is not a plain version" % release)
    items = []
    for n, raw in enumerate(text.splitlines(), 1):
        line = _COMMENT.sub("", raw)
        found = []
        for repo, name, version, digest in _CHART.findall(line):
            if "://" in repo and not repo.startswith("https://"):
                raise VerifyError("%s line %d: repo %s is neither a bare OCI repository nor https://" % (BOM, n, repo))
            if repo.startswith("https://"):
                found.append({"kind": "index", "ref": "%s %s %s" % (repo, name, version), "repo": repo, "name": name})
            else:
                # Helm stores a version's "+" as "_" in the OCI tag.
                found.append({"kind": "oci", "ref": "%s/%s:%s" % (repo, name, version.replace("+", "_"))})
            found[-1].update(version=version, digest=digest)
        for package, tag, digest in _VM.findall(line):
            found.append({"kind": "oci", "ref": "%s/%s:%s" % (VM_IMAGE_REPO, package, tag), "version": tag, "digest": digest})
        for url, sha in _URL.findall(line):
            found.append({"kind": "url", "ref": url, "version": "", "digest": sha})
        m_image = _IMAGE.match(line)
        if m_image and not found:
            repo, tag, digest = m_image.groups()
            found.append({"kind": "oci", "ref": "%s:%s" % (repo, tag), "version": tag, "digest": digest})
        if len(found) != len(_PIN.findall(line)):
            raise VerifyError("%s line %d: a pin in a shape this script does not read" % (BOM, n))
        for item in found:
            item["line"] = n
        items.extend(found)
    if not items:
        raise VerifyError("the BOM has no pins")
    return release, items


class Redirects(urllib.request.HTTPRedirectHandler):
    """Follow https redirects of plain downloads only (github.com release files need them; their bytes are hashed).
    A manifest request is never redirected: another host would state the digest and receive the pull token."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if req.get_method() == "HEAD" or req.has_header("Authorization"):
            raise VerifyError("%s redirects to %s: a manifest request is not redirected" % (req.full_url, newurl))
        if not newurl.startswith("https://"):
            raise VerifyError("%s redirects to %s, which is not https://" % (req.full_url, newurl))
        return super().redirect_request(req, fp, code, msg, headers, newurl)


OPENER = urllib.request.build_opener(Redirects)


def fetch(opener, url, headers=None, method="GET", limit=MAX_SMALL, digest=None):
    """One request with bounded retries. Returns (status, headers, body); with digest (a hashlib object) the body is
    hashed instead of kept. A 4xx other than 429 is returned to the caller, not retried."""
    if not url.startswith("https://"):
        raise VerifyError("%s is not https://" % url)
    last = None
    for attempt in range(ATTEMPTS):
        if attempt:
            time.sleep(2 ** attempt)
        try:
            # A generic client name: charts.jetstack.io answers 403 to urllib's default User-Agent.
            req = urllib.request.Request(url, headers=dict(headers or {}, **{"User-Agent": "bom-verify"}), method=method)
            with opener(req, timeout=TIMEOUT) as resp:
                size, chunks = 0, []
                hasher = digest.copy() if digest else None
                while method != "HEAD":
                    chunk = resp.read(1 << 20)
                    if not chunk:
                        break
                    size += len(chunk)
                    if size > limit:
                        raise VerifyError("%s is larger than %d bytes" % (url, limit))
                    hasher.update(chunk) if hasher else chunks.append(chunk)
                body = hasher.hexdigest() if hasher else b"".join(chunks)
                return resp.status, resp.headers, body
        except urllib.error.HTTPError as err:
            err.close()
            if err.code < 500 and err.code != 429:
                return err.code, err.headers, b""
            last = "HTTP %d" % err.code
        except (urllib.error.URLError, OSError, http.client.HTTPException) as err:  # timeouts, resets, cut bodies
            last = str(getattr(err, "reason", err)) or repr(err)
    raise VerifyError("%s: %s after %d attempts" % (url, last, ATTEMPTS))


def oci_digest(opener, ref):
    """The Docker-Content-Digest of <host>/<path>:<tag>, with an anonymous pull token when the registry asks for one."""
    name, _, tag = ref.rpartition(":")
    host, _, path = name.partition("/")
    url = "https://%s/v2/%s/manifests/%s" % (host, path, tag)
    headers = {"Accept": ACCEPT}
    status, resp, _ = fetch(opener, url, headers, "HEAD")
    if status == 401:
        params = dict(re.findall(r'(\w+)="([^"]*)"', resp.get("WWW-Authenticate") or ""))
        if "realm" not in params:
            raise VerifyError("%s: 401 without a Bearer realm" % url)
        query = urllib.parse.urlencode({k: params[k] for k in ("service", "scope") if k in params})
        status, _, body = fetch(opener, params["realm"] + "?" + query)
        try:
            token = "" + json.loads(body)["token"]  # TypeError unless a string
        except (ValueError, KeyError, TypeError):
            raise VerifyError("%s: no anonymous token (HTTP %d)" % (params["realm"], status))
        headers["Authorization"] = "Bearer " + token
        status, resp, _ = fetch(opener, url, headers, "HEAD")
    if status != 200:
        raise VerifyError("HTTP %d for the manifest" % status)
    got = resp.get("Docker-Content-Digest")
    if not got:
        raise VerifyError("the registry sent no Docker-Content-Digest")
    return got


def index_digest(index, name, version):
    """The digest of name at exactly version in a Helm index.yaml, as sha256:<hex>.

    Helm writes one fixed shape, which this reads by indentation: under `entries:`, `<name>:` then one list item per
    version whose own keys (`version:`, `digest:`) sit two columns right of its dash. Deeper keys (a dependency's
    version) are ignored."""
    lines = index.splitlines()
    try:
        start = lines.index("entries:") + 1
    except ValueError:
        raise VerifyError("index.yaml has no entries")
    key = re.compile(r'^(\s+)["\']?%s["\']?:\s*$' % re.escape(name))
    depth = None  # the indentation of the chart names: that of the first line under entries
    for i in range(start, len(lines)):
        if lines[i] and not lines[i].startswith((" ", "-")):
            break  # the next top-level key
        if depth is None and lines[i].strip():
            depth = len(lines[i]) - len(lines[i].lstrip())
        m = key.match(lines[i])
        if not m or len(m.group(1)) != depth:
            continue  # a deeper line is not a chart, whatever it says
        entry, found = {}, []
        dash = None
        for line in lines[i + 1:]:
            indent = len(line) - len(line.lstrip())
            if not line.strip():
                continue
            if dash is None:
                dash = indent
            if indent < dash or (indent == dash and not line.lstrip().startswith("- ")):
                break  # the next chart
            if indent == dash:
                entry = {}
                found.append(entry)
                line = " " * (dash + 2) + line.lstrip()[2:]
                indent = dash + 2
            kv = re.match(r'^\s*(version|digest):\s*["\']?([^"\'\s]+)["\']?\s*$', line)
            if kv and indent == dash + 2:
                entry[kv.group(1)] = kv.group(2)
        digests = [e.get("digest") for e in found if e.get("version") == version]
        if len(digests) != 1 or not digests[0]:
            raise VerifyError("index.yaml has %d entries with a digest for %s %s" % (len([d for d in digests if d]), name, version))
        return digests[0] if digests[0].startswith("sha256:") else "sha256:" + digests[0]
    raise VerifyError("index.yaml has no chart %s" % name)


def resolve(opener, item, max_download):
    """What the registry serves for one item, in the form of its pin."""
    if item["kind"] == "oci":
        return oci_digest(opener, item["ref"])
    if item["kind"] == "index":
        url = item["repo"].rstrip("/") + "/index.yaml"
        status, _, body = fetch(opener, url)
        if status != 200:
            raise VerifyError("HTTP %d for %s" % (status, url))
        # "replace": a byte that is not UTF-8 cannot make a digest match, it can only break the line it is on.
        return index_digest(body.decode("utf-8", "replace"), item["name"], item["version"])
    status, _, got = fetch(opener, item["ref"], limit=max_download, digest=hashlib.sha256())
    if status != 200:
        raise VerifyError("HTTP %d" % status)
    return got


def verify(text, opener, release=False, max_download=256 << 20, out=print):
    """Check every pin; returns the number of failures."""
    bom_release, items = parse_bom(text)
    failures = 0
    if release and bom_release.endswith("-dev"):
        out("FAIL release %s: a release tag needs a BOM without -dev" % bom_release)
        failures += 1
    seen = {}
    for item in items:
        key = (item["kind"], item["ref"], item["digest"])
        if key in seen:
            seen[key].append(item["line"])
            continue
        seen[key] = lines = [item["line"]]
        item["lines"] = lines
    for item in items:
        if "lines" not in item:
            continue
        where = "%s %s (line %s)" % (item["kind"], item["ref"], ",".join(map(str, item["lines"])))
        problems = []
        if release and item["version"].lstrip("v").startswith("0.0.0+"):
            problems.append("version %s is not allowed in a release" % item["version"])
        if item["digest"] == "pending":
            if release:
                problems.append("digest pending is not allowed in a release")
            else:
                out("skip %s: digest pending, nothing compared" % where)
                continue
        else:
            try:
                got = resolve(opener, item, max_download)
                if got != item["digest"]:
                    problems.append("the BOM pins %s, the registry serves %s" % (item["digest"], got))
            except VerifyError as err:
                problems.append(str(err))
        if problems:
            failures += 1
            out("FAIL %s: %s" % (where, "; ".join(problems)))
        else:
            out("ok   %s %s" % (where, item["digest"]))
    return failures


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--bom", default=BOM)
    parser.add_argument("--release", action="store_true", help="also refuse -dev, pending digests and 0.0.0+ versions")
    parser.add_argument("--max-download-mb", type=int, default=256, help="size cap of one url download")
    args = parser.parse_args(argv)
    try:
        with open(args.bom, encoding="utf-8") as handle:
            failures = verify(handle.read(), OPENER.open, args.release, args.max_download_mb << 20)
    except VerifyError as err:
        print("FAIL %s" % err)
        failures = 1
    print("bom-verify: %s" % ("%d failed" % failures if failures else "all pins match"))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
