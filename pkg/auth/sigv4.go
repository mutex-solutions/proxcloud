package auth

// sigv4.go verifies AWS SigV4 requests: requests carrying an
// `Authorization: AWS4-HMAC-SHA256 Credential=<keyId>/<date>/<region>/<service>/aws4_request,
// SignedHeaders=..., Signature=...` header are verified against the `hmac_keys` collection
// (HmacKeyService: id=pk<md5>, secretKey=sk<sha1>). The signature is recomputed over the
// canonical request per the AWS Signature Version 4 spec. On success the RequestContext
// carries SigV4KeyID (ROLE_SIGV4_USER).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// maxSigV4Skew is the maximum allowed request clock skew.
const maxSigV4Skew = 5 * time.Minute

// HmacKeyLookup resolves an access-key id to its secret. ok=false when unknown.
type HmacKeyLookup func(ctx context.Context, accessKeyID string) (secret string, ok bool)

// SetHmacLookup wires the hmac_keys resolver (nil → SigV4 requests are rejected).
func (a *Authenticator) SetHmacLookup(l HmacKeyLookup) {
	a.mu.Lock()
	a.hmacLookup = l
	a.mu.Unlock()
}

// verifySigV4 authenticates an AWS4-HMAC-SHA256 request. The body is read + restored so the
// handler still sees it.
func (a *Authenticator) verifySigV4(r *http.Request, authz string) (*httpx.RequestContext, bool) {
	a.mu.RLock()
	lookup := a.hmacLookup
	a.mu.RUnlock()
	if lookup == nil {
		return nil, false
	}
	credential := sigv4AuthParam(authz, "Credential")
	signedHeaders := sigv4AuthParam(authz, "SignedHeaders")
	gotSig := sigv4AuthParam(authz, "Signature")
	parts := strings.Split(credential, "/")
	if len(parts) != 5 || signedHeaders == "" || gotSig == "" {
		return nil, false
	}
	keyID, scopeDate, region, service := parts[0], parts[1], parts[2], parts[3]
	secret, ok := lookup(r.Context(), keyID)
	if !ok {
		return nil, false
	}
	amzDate := r.Header.Get("X-Amz-Date")
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || absDuration(time.Since(t)) > maxSigV4Skew {
		return nil, false
	}
	// Read + restore the body (the canonical request hashes the payload).
	// r.Body is nil on hand-built in-process requests (e.g. MCP dispatch).
	if r.Body == nil {
		r.Body = http.NoBody
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	canonical := canonicalRequest(r, signedHeaders, body)
	scope := strings.Join([]string{scopeDate, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256(canonical),
	}, "\n")
	key := hmacSHA256([]byte("AWS4"+secret), scopeDate)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	want := hex.EncodeToString(hmacSHA256(key, stringToSign))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(gotSig))) {
		return nil, false
	}
	return &httpx.RequestContext{SigV4KeyID: keyID}, true
}

var multiSpace = regexp.MustCompile(`\s+`)

// canonicalRequest builds the AWS SigV4 canonical request from the live request.
func canonicalRequest(r *http.Request, signedHeaders string, body []byte) string {
	names := strings.Split(strings.ToLower(signedHeaders), ";")
	sort.Strings(names)
	var hdrs strings.Builder
	for _, n := range names {
		v := r.Header.Get(n)
		if n == "host" {
			v = r.Host
		}
		hdrs.WriteString(n)
		hdrs.WriteString(":")
		hdrs.WriteString(multiSpace.ReplaceAllString(strings.TrimSpace(v), " "))
		hdrs.WriteString("\n")
	}
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.Join([]string{
		r.Method,
		path,
		canonicalQuery(r.URL.Query()),
		hdrs.String(),
		strings.Join(names, ";"),
		hexSHA256(string(body)),
	}, "\n")
}

// canonicalQuery: RFC3986-encode keys+values, sort by key then value.
func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{awsEncode(k), awsEncode(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.k+"="+p.v)
	}
	return strings.Join(parts, "&")
}

// awsEncode is the SigV4 URI-encode: unreserved chars (A-Za-z0-9 - . _ ~) pass, all else %XX.
func awsEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString(strings.ToUpper(url.QueryEscape(string(c))))
			// url.QueryEscape encodes space as '+' — normalize to %20.
		}
	}
	return strings.ReplaceAll(b.String(), "+", "%20")
}

func hexSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// sigv4AuthParam extracts Credential/SignedHeaders/Signature from the Authorization header.
func sigv4AuthParam(authz, key string) string {
	rest := strings.TrimPrefix(authz, "AWS4-HMAC-SHA256")
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, key+"=") {
			return strings.TrimPrefix(part, key+"=")
		}
	}
	return ""
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
