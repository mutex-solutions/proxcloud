#!/usr/bin/env python3
"""Validate app release tags and emit strict GitHub step outputs (stdlib only).

App images use the tag verbatim, so only stable vX.Y.Z and vX.Y.Z-rc.N
are release tags here. In particular, SemVer build metadata is not a Docker tag.
No input is stripped, evaluated as shell text, or used before full validation.
"""
import os
import re
import sys


_NUMBER = r"(?:0|[1-9][0-9]*)"
_TAG = re.compile(rf"v{_NUMBER}\.{_NUMBER}\.{_NUMBER}(?:-rc\.{_NUMBER})?")


def release_policy(tag):
    """Return the exact version and gh flags, or fail closed on an invalid tag."""
    if not isinstance(tag, str) or not _TAG.fullmatch(tag):
        raise ValueError("app release tag must be vX.Y.Z or vX.Y.Z-rc.N, without leading zeroes")
    prerelease = "-rc." in tag
    return {"tag": tag, "version": tag[1:],
            "flags": ["--prerelease", "--latest=false"] if prerelease else ["--latest"]}


def main():
    try:
        policy = release_policy(os.environ.get("GITHUB_REF_NAME", ""))
    except ValueError as error:
        # Do not echo untrusted tag contents into workflow commands or logs.
        print("::error::%s" % error, file=sys.stderr)
        return 1
    print("tag=%s" % policy["tag"])
    print("version=%s" % policy["version"])
    # All flags are fixed literals, never substrings supplied by the tag.
    print("flags=%s" % " ".join(policy["flags"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
