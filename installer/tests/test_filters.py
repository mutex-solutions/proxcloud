#!/usr/bin/env python3
"""Runnable check of the installer's decisions: python3 installer/tests/test_filters.py"""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "filter_plugins"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "library"))
import proxcloud as f  # noqa: E402
import proxcloud_api as api  # noqa: E402

STD = [
    {"storage": "local", "type": "dir", "content": "backup,iso,vztmpl,import"},
    {"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir"},
    {"storage": "local-zfs", "type": "zfspool", "content": "rootdir,images"},
    {"storage": "nas-backup", "type": "nfs", "content": "backup"},
    {"storage": "nas-capacity", "type": "nfs", "content": "images"},
    {"storage": "nas-templates", "type": "nfs", "content": "iso,import,images"},
]
c = f.classify_storages(STD)
assert c["tier"] == "standard"
assert set(c["profiles"]) == {"std", "local-lvm", "capacity"}, c["profiles"]
assert c["profiles"]["std"]["storages"] == {"*": "local-zfs"}
assert c["profiles"]["capacity"]["storages"] == {"*": "nas-capacity"}
assert c["templates"] == ["nas-templates"] and c["backups"] == ["local", "nas-backup"]
assert c["imports"] == ["local", "nas-templates"]
assert f.default_profile(c["profiles"]) == "std" and f.default_profile(c["profiles"], "capacity") == "capacity"

c = f.classify_storages([{"storage": "ceph", "type": "rbd", "content": "images"}])
assert c["tier"] == "full" and c["profiles"]["ceph"]["shared"]
assert f.default_profile(c["profiles"]) == "ceph"

assert f.fio_class(0.5, 50000) == "performance"
assert f.fio_class(3, 50000) == "standard"
assert f.fio_class(9, 50000) == "capacity"
assert f.fio_class(0.5, 100) == "capacity"  # IOPS can only downgrade
assert f.fio_class(1.5, 900, 2, 5, 500) == "performance"  # thresholds are knobs

assert f.template_nodes(["b", "a"], False) == ["a", "b"]
assert f.template_nodes(["b", "a"], True) == ["a"]
assert f.template_nodes(["b", "a"], True, True) == ["a", "b"]
assert f.template_nodes(["b", "a"], False, False) == ["a"]

want = [{"path": "/pool", "role": "R", "propagate": 1, "user": "u@pve", "token": "u@pve!t"}]
rows = [{"path": "/pool", "roleid": "R", "type": "user", "ugid": "u@pve", "propagate": 1}]
assert f.pve_acl_missing(rows, want) == [{"path": "/pool", "roles": "R", "propagate": 1, "tokens": "u@pve!t"}]
rows.append({"path": "/pool", "roleid": "R", "type": "token", "ugid": "u@pve!t", "propagate": 1})
assert f.pve_acl_missing(rows, want) == []

# SigV4 against the worked example the Go verifier also accepts (pkg/auth/sigv4_test.go signs the same way).
h = api.sigv4_headers("POST", "http://10.0.0.1:8080/api/v1/admin/service?b=1&a=2", b'{"x":1}', "pkA", "skB", "20261107T010203Z")
assert h["Host"] == "10.0.0.1:8080"
assert h["Authorization"].startswith("AWS4-HMAC-SHA256 Credential=pkA/20261107/us-east-1/execute-api/aws4_request, SignedHeaders=host;x-amz-date, Signature=")
assert h["Authorization"] != api.sigv4_headers("POST", "http://10.0.0.1:8080/api/v1/admin/service?a=2&b=1", b"{}", "pkA", "skB", "20261107T010203Z")["Authorization"]
print("ok")
