#!/usr/bin/env python3
"""Internal buckets on the object store (plan S20): one bucket and one service user per consumer, the user limited
to its bucket. Idempotent. Prints {"changed": bool}.
Env: S3_URL, ROOT_ACCESS, ROOT_SECRET, RETENTION_DAYS, REGION (default us-east-1), FLAVOR (rustfs | rgw | external,
default rustfs; external: ROOT_* is a plain S3 key that creates and keeps the bucket itself, no service user),
BUCKETS = JSON {bucket: secret} or {bucket: {"secret": s, "lock": false}} for user <bucket>-svc.
A locked bucket (the default) gets versioning and Object Lock GOVERNANCE (backups). "lock": false is a plain bucket for a
consumer that deletes what it no longer needs (the sandbox registry's garbage collection).
rustfs: the root key adds the user and a policy for its bucket. rgw: ROOT_* is an RGW user with users=* caps; the
service user is made through the admin API and creates its own bucket, so it owns it and reaches nothing else."""
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
ROOT = (os.environ["ROOT_ACCESS"], os.environ["ROOT_SECRET"])
RETENTION = int(os.environ.get("RETENTION_DAYS", "30"))
REGION = os.environ.get("REGION") or "us-east-1"
FLAVOR = os.environ.get("FLAVOR") or "rustfs"


def call(method, path, query="", body=b"", headers=None, cred=ROOT):
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
    scope = "%s/%s/s3/aws4_request" % (day, REGION)
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = ("AWS4" + cred[1]).encode()
    for part in (day, REGION, "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    sig = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    h["authorization"] = "AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s" % (cred[0], scope, ";".join(signed), sig)
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


def create_bucket(bucket, lock, cred):
    """Creates the bucket when it is missing; True when it did."""
    status = call("HEAD", "/" + bucket, cred=cred)[0]
    if status != 404:
        must((status, b""), "head " + bucket, ok=(200,))
        return False
    must(call("PUT", "/" + bucket, headers={"x-amz-bucket-object-lock-enabled": "true"} if lock else None, cred=cred), "create " + bucket)
    if lock:
        must(call("PUT", "/" + bucket, "versioning=", b'<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>', cred=cred), "versioning " + bucket)
        rule = ('<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled>'
                '<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>%d</Days></DefaultRetention></Rule></ObjectLockConfiguration>' % RETENTION)
        must(call("PUT", "/" + bucket, "object-lock=", rule.encode(), cred=cred), "object lock " + bucket)
    return True


changed = False
for bucket, spec in json.loads(os.environ["BUCKETS"]).items():
    if isinstance(spec, str):
        spec = {"secret": spec}
    secret, lock, user = spec["secret"], spec.get("lock", True), bucket + "-svc"
    if FLAVOR == "external":  # someone else's store: no admin API, the given key owns the bucket
        changed = create_bucket(bucket, lock, ROOT) or changed
        continue
    if FLAVOR == "rgw":
        # The service user's key is fixed by us (same secret on every run); creating it again is a no-op.
        status, body = call("GET", "/admin/user", "uid=" + user)
        if status == 404:
            must(call("PUT", "/admin/user", "uid=%s&display-name=%s&access-key=%s&secret-key=%s" % (user, user, user, secret)), "add user " + user)
            changed = True
        elif status != 200:
            must((status, body), "read user " + user)
        changed = create_bucket(bucket, lock, (user, secret)) or changed
        continue
    changed = create_bucket(bucket, lock, ROOT) or changed
    policy = bucket + "-rw"
    if call("GET", "/rustfs/admin/v3/user-info", "accessKey=" + user)[0] != 200:
        admin("PUT", "add-user", "accessKey=" + user, {"secretKey": secret, "status": "enabled"})
        admin("PUT", "add-canned-policy", "name=" + policy, {"Version": "2012-10-17", "Statement": [{
            "Effect": "Allow", "Action": ["s3:*"],
            "Resource": ["arn:aws:s3:::%s" % bucket, "arn:aws:s3:::%s/*" % bucket],
        }]})
        admin("POST", "idp/builtin/policy/attach", doc={"policies": [policy], "user": user})
        changed = True
print(json.dumps({"changed": changed}))
