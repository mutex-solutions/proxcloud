package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signV4 is an independent-of-the-verifier test signer following the AWS SigV4 spec steps.
// The live cross-check is `curl --aws-sigv4` (a third implementation); this keeps the
// verifier's parsing/canonicalization honest in-unit.
func signV4(method, path, query, host, amzDate, keyID, secret, region, service string, body []byte) string {
	payload := sha256.Sum256(body)
	canonical := strings.Join([]string{
		method, path, query,
		"host:" + host + "\n" + "x-amz-date:" + amzDate + "\n",
		"host;x-amz-date",
		hex.EncodeToString(payload[:]),
	}, "\n")
	scope := strings.Join([]string{amzDate[:8], region, service, "aws4_request"}, "/")
	ch := sha256.Sum256([]byte(canonical))
	sts := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(ch[:])}, "\n")
	mac := func(key []byte, s string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(s))
		return m.Sum(nil)
	}
	k := mac([]byte("AWS4"+secret), amzDate[:8])
	k = mac(k, region)
	k = mac(k, service)
	k = mac(k, "aws4_request")
	sig := hex.EncodeToString(mac(k, sts))
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=host;x-amz-date, Signature=%s",
		keyID, amzDate[:8], region, service, sig)
}

func TestVerifySigV4(t *testing.T) {
	a := New(slog.Default())
	a.SetHmacLookup(func(_ context.Context, id string) (string, bool) {
		if id == "pkTEST" {
			return "skSECRET", true
		}
		return "", false
	})
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	newReq := func(authz string) (rc bool, keyID string) {
		r := httptest.NewRequest("GET", "http://api.test/admin-api/v1/users?limit=2&email=a%40b.com", nil)
		r.Host = "api.test"
		r.Header.Set("X-Amz-Date", amzDate)
		r.Header.Set("Authorization", authz)
		got, ok := a.verifySigV4(r, authz)
		if !ok {
			return false, ""
		}
		return true, got.SigV4KeyID
	}

	authz := signV4("GET", "/admin-api/v1/users", "email=a%40b.com&limit=2", "api.test", amzDate, "pkTEST", "skSECRET", "us-east-1", "execute-api", nil)
	if ok, key := newReq(authz); !ok || key != "pkTEST" {
		t.Fatalf("valid signature rejected (key=%q)", key)
	}
	// Wrong secret → reject.
	bad := signV4("GET", "/admin-api/v1/users", "email=a%40b.com&limit=2", "api.test", amzDate, "pkTEST", "WRONG", "us-east-1", "execute-api", nil)
	if ok, _ := newReq(bad); ok {
		t.Fatal("tampered signature accepted")
	}
	// Unknown key id → reject.
	unknown := signV4("GET", "/admin-api/v1/users", "email=a%40b.com&limit=2", "api.test", amzDate, "pkNOPE", "skSECRET", "us-east-1", "execute-api", nil)
	if ok, _ := newReq(unknown); ok {
		t.Fatal("unknown key accepted")
	}
	// Stale X-Amz-Date → reject (±5m skew).
	old := time.Now().UTC().Add(-time.Hour).Format("20060102T150405Z")
	staleAuthz := signV4("GET", "/admin-api/v1/users", "email=a%40b.com&limit=2", "api.test", old, "pkTEST", "skSECRET", "us-east-1", "execute-api", nil)
	r := httptest.NewRequest("GET", "http://api.test/admin-api/v1/users?limit=2&email=a%40b.com", nil)
	r.Host = "api.test"
	r.Header.Set("X-Amz-Date", old)
	if _, ok := a.verifySigV4(r, staleAuthz); ok {
		t.Fatal("stale request accepted")
	}
	// Body must be part of the signature.
	bodyAuthz := signV4("POST", "/admin-api/v1/users", "", "api.test", amzDate, "pkTEST", "skSECRET", "us-east-1", "execute-api", []byte(`{"email":"x@y.z"}`))
	pr := httptest.NewRequest("POST", "http://api.test/admin-api/v1/users", strings.NewReader(`{"email":"x@y.z"}`))
	pr.Host = "api.test"
	pr.Header.Set("X-Amz-Date", amzDate)
	if rc, ok := a.verifySigV4(pr, bodyAuthz); !ok || rc.SigV4KeyID != "pkTEST" {
		t.Fatal("valid signed body rejected")
	}
	pr2 := httptest.NewRequest("POST", "http://api.test/admin-api/v1/users", strings.NewReader(`{"email":"TAMPERED"}`))
	pr2.Host = "api.test"
	pr2.Header.Set("X-Amz-Date", amzDate)
	if _, ok := a.verifySigV4(pr2, bodyAuthz); ok {
		t.Fatal("tampered body accepted")
	}
}
