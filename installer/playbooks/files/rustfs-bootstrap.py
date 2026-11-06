#!/usr/bin/env python3
"""Internal buckets on RustFS (plan S20): one bucket and one service user per consumer, the user limited
to its bucket; versioning and Object Lock GOVERNANCE on. Idempotent. Prints {"changed": bool}.
Env: S3_URL, ROOT_ACCESS, ROOT_SECRET, BUCKETS = JSON {bucket: secret of user <bucket>-svc}, RETENTION_DAYS."""
import datetime
import hashlib
import hmac
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ["S3_URL"].rstrip("/")
ACCESS, SECRET = os.environ["ROOT_ACCESS"], os.environ["ROOT_SECRET"]
RETENTION = int(os.environ.get("RETENTION_DAYS", "30"))


def call(method, path, query="", body=b"", headers=None):
    now = datetime.datetime.now(datetime.timezone.utc)
    amz, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    host = urllib.parse.urlsplit(BASE).netloc
    digest = hashlib.sha256(body).hexdigest()
    h = {"host": host, "x-amz-date": amz, "x-amz-content-sha256": digest}
    h.update({k.lower(): v for k, v in (headers or {}).items()})
    signed = sorted(h)
    q = "&".join(sorted(
        "%s=%s" % (urllib.parse.quote(k, safe="-_.~"), urllib.parse.quote(v, safe="-_.~"))
        for k, v in urllib.parse.parse_qsl(query, keep_blank_values=True)
    ))
    canonical = "\n".join([method, path, q, "".join("%s:%s\n" % (k, h[k]) for k in signed), ";".join(signed), digest])
    scope = "%s/us-east-1/s3/aws4_request" % day
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = ("AWS4" + SECRET).encode()
    for part in (day, "us-east-1", "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    h["authorization"] = "AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s" % (ACCESS, scope, ";".join(signed), sig)
    h.pop("host")
    req = urllib.request.Request(BASE + path + ("?" + query if query else ""), data=body or None, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def must(res, what, ok=(200, 204)):
    if res[0] not in ok:
        sys.exit("%s: HTTP %s %s" % (what, res[0], res[1][:300].decode(errors="replace")))
    return res


def admin(method, path, query="", doc=None):
    return must(call(method, "/rustfs/admin/v3/" + path, query, json.dumps(doc).encode() if doc is not None else b""), path)


changed = False
for bucket, secret in json.loads(os.environ["BUCKETS"]).items():
    if call("HEAD", "/" + bucket)[0] == 404:
        must(call("PUT", "/" + bucket, headers={"x-amz-bucket-object-lock-enabled": "true"}), "create " + bucket)
        must(call("PUT", "/" + bucket, "versioning=", b'<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>'), "versioning " + bucket)
        lock = ('<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled>'
                '<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>%d</Days></DefaultRetention></Rule></ObjectLockConfiguration>' % RETENTION)
        must(call("PUT", "/" + bucket, "object-lock=", lock.encode()), "object lock " + bucket)
        changed = True
    user, policy = bucket + "-svc", bucket + "-rw"
    if call("GET", "/rustfs/admin/v3/user-info", "accessKey=" + user)[0] != 200:
        admin("PUT", "add-user", "accessKey=" + user, {"secretKey": secret, "status": "enabled"})
        admin("PUT", "add-canned-policy", "name=" + policy, {"Version": "2012-10-17", "Statement": [{
            "Effect": "Allow", "Action": ["s3:*"],
            "Resource": ["arn:aws:s3:::%s" % bucket, "arn:aws:s3:::%s/*" % bucket],
        }]})
        admin("POST", "idp/builtin/policy/attach", doc={"policies": [policy], "user": user})
        changed = True
print(json.dumps({"changed": changed}))
