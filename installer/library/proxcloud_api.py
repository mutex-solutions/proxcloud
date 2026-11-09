#!/usr/bin/python3
"""Call the ProxCloud API with an Admin-API key (SigV4): the `uri` module cannot sign requests.
Runs on the mgmt host with the stdlib only."""
import datetime
import hashlib
import hmac
import json
import urllib.error
import urllib.parse
import urllib.request

from ansible.module_utils.basic import AnsibleModule

DOCUMENTATION = r"""
module: proxcloud_api
short_description: Signed request to the ProxCloud API
options:
  url: {required: true, type: str}
  method: {default: GET, type: str}
  body: {type: raw}
  access_key: {required: true, type: str}
  secret_key: {required: true, type: str, no_log: true}
  ok_status: {default: [200], type: list, elements: int}
"""


def _sign(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()


def sigv4_headers(method, url, body, access_key, secret_key, amz_date):
    """The headers pkg/auth/sigv4.go verifies: host + x-amz-date signed, scope us-east-1/execute-api."""
    u = urllib.parse.urlsplit(url)
    query = "&".join(sorted(
        "%s=%s" % (urllib.parse.quote(k, safe="-_.~"), urllib.parse.quote(v, safe="-_.~"))
        for k, v in urllib.parse.parse_qsl(u.query, keep_blank_values=True)
    ))
    canonical = "\n".join([
        method, u.path or "/", query,
        "host:%s\nx-amz-date:%s\n" % (u.netloc, amz_date),
        "host;x-amz-date", hashlib.sha256(body).hexdigest(),
    ])
    date = amz_date[:8]
    scope = "%s/us-east-1/execute-api/aws4_request" % date
    to_sign = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = ("AWS4" + secret_key).encode()
    for part in (date, "us-east-1", "execute-api", "aws4_request"):
        key = _sign(key, part)
    signature = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    return {
        "Host": u.netloc,
        "X-Amz-Date": amz_date,
        "Authorization": "AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host;x-amz-date, Signature=%s" % (access_key, scope, signature),
    }


def main():
    m = AnsibleModule(argument_spec=dict(
        url=dict(type="str", required=True),
        method=dict(type="str", default="GET"),
        body=dict(type="raw"),
        access_key=dict(type="str", required=True),
        secret_key=dict(type="str", required=True, no_log=True),
        ok_status=dict(type="list", elements="int", default=[200]),
    ), supports_check_mode=False)
    p = m.params
    body = b"" if p["body"] is None else json.dumps(p["body"]).encode()
    amz_date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    headers = sigv4_headers(p["method"], p["url"], body, p["access_key"], p["secret_key"], amz_date)
    if body:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(p["url"], data=body or None, method=p["method"], headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            status, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        status, raw = e.code, e.read()
    except urllib.error.URLError as e:
        m.fail_json(msg="%s %s: %s" % (p["method"], p["url"], e.reason))
    try:
        data = json.loads(raw) if raw else None
    except ValueError:
        data = raw.decode(errors="replace")
    result = dict(changed=p["method"] != "GET", status=status, json=data)
    if status not in p["ok_status"]:
        m.fail_json(msg="%s %s returned %s" % (p["method"], p["url"], status), **result)
    m.exit_json(**result)


if __name__ == "__main__":
    main()
