#!/usr/bin/env python3
"""Validate the curated docs screenshot inventory; no browser, network or image rewriting.

Run from any directory: python3 web/check-docs-images.py
The manifest records synthetic UI illustrations, not backend acceptance evidence.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import struct
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "web/docs-image-audit.json"


def png_metadata(path):
    raw = path.read_bytes()
    if raw[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError("not a PNG: " + str(path.relative_to(ROOT)))
    width, height = struct.unpack("!II", raw[16:24])
    offset, kinds = 8, set()
    while offset + 12 <= len(raw):
        size = struct.unpack("!I", raw[offset:offset + 4])[0]
        kind = raw[offset + 4:offset + 8]
        if offset + 12 + size > len(raw):
            raise ValueError("truncated PNG: " + str(path.relative_to(ROOT)))
        kinds.add(kind)
        offset += size + 12
    if offset != len(raw) or b"IEND" not in kinds:
        raise ValueError("invalid PNG chunk boundary")
    if kinds.intersection({b"tEXt", b"zTXt", b"iTXt", b"eXIf"}):
        raise ValueError("unexpected text/EXIF metadata in screenshot")
    return width, height, hashlib.sha256(raw).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--refresh-reviewed-hashes", action="store_true", help="update dimensions/hashes only after successful capture and human visual/privacy review")
    args = parser.parse_args()
    inventory = json.loads(MANIFEST.read_text())
    if args.refresh_reviewed_hashes:
        for record in inventory["screenshots"]:
            app, filename = record["app"], record["file"]
            assert app in ("client", "admin") and Path(filename).name == filename and filename.endswith(".png")
            width, height, digest = png_metadata(ROOT / "web" / app / "public/docs-img" / filename)
            record.update(width=width, height=height, sha256=digest)
        MANIFEST.write_text(json.dumps(inventory, indent=2) + "\n")
        print("Refreshed reviewed screenshot metadata; this is an operator attestation, not automatic visual validation.")
    assert inventory["schemaVersion"] == 1
    assert inventory["source"] == "current-ui-with-synthetic-documentation-fixtures"
    expected, referenced = {}, set()
    for record in inventory["screenshots"]:
        app, filename = record["app"], record["file"]
        assert app in ("client", "admin") and Path(filename).name == filename
        assert filename.endswith(".png")
        key = (app, filename)
        assert key not in expected, "duplicate screenshot record"
        path = ROOT / "web" / app / "public/docs-img" / filename
        width, height, digest = png_metadata(path)
        assert (width, height) == (record["width"], record["height"]), filename + ": dimensions changed"
        assert digest == record["sha256"], filename + ": regenerate manifest after reviewed recapture"
        assert width == 1440 and height >= 1000, filename + ": wrong capture viewport"
        assert record["route"].startswith("/") and record["state"].strip()
        assert record["documents"], filename + ": no consuming documentation"
        for name in record["documents"]:
            doc = ROOT / name
            assert str(doc.resolve()).startswith(str((ROOT / "web" / app / "src/docs/content").resolve()) + "/")
            text = doc.read_text()
            assert re.search(r"!\[[^\]]+\]\(/docs-img/" + re.escape(filename) + r"\)", text), filename + ": missing documented image/alt text"
        expected[key] = record

    # Include new untracked docs images during local development, not only committed files.
    actual = set()
    for app in ("client", "admin"):
        actual.update((app, path.name) for path in (ROOT / "web" / app / "public/docs-img").iterdir() if path.is_file())
    assert actual == set(expected), "image files and manifest differ: " + repr(actual.symmetric_difference(expected))

    documents = subprocess.check_output(["git", "ls-files", "*.md"], cwd=ROOT, text=True).splitlines()
    for name in documents:
        if not name.startswith(("web/client/src/docs/content/", "web/admin/src/docs/content/")):
            continue
        app = name.split("/")[1]
        text = (ROOT / name).read_text()
        for alt, filename in re.findall(r"!\[([^\]]*)\]\(/docs-img/([^/)]+)\)", text):
            assert alt.strip(), name + ": image has no alt text"
            key = (app, filename)
            assert key in expected, name + ": untracked screenshot reference"
            assert name in expected[key]["documents"], name + ": consuming doc absent from manifest"
            referenced.add(key)
    assert referenced == set(expected), "unreferenced curated screenshot"
    print(f"Validated {len(expected)} PNG screenshots: dimensions, hashes, metadata, alt text and consuming doc references.")
    print("This checks file/reference integrity, not screenshot meaning or backend/runtime acceptance.")


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, KeyError, ValueError, OSError, struct.error) as error:
        print("Docs image audit failed: " + str(error), file=sys.stderr)
        raise SystemExit(1)
