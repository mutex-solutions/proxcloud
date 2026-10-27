package auth

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SignSigV4 signs r with AWS SigV4 using the given hmac key pair, reproducing
// verifySigV4's canonicalization exactly. Used by the MCP tool dispatcher to
// make in-process Admin-API calls on behalf of an api-key principal (the MCP
// client presents `Bearer pk.sk`; internal REST dispatch re-enters the normal
// SigV4 gate with a request we sign ourselves).
//
// The verifier does not pin region/service — it recomputes from the Credential
// scope — so the conventional us-east-1/execute-api pair is used.
func SignSigV4(r *http.Request, keyID, secret string, body []byte, now time.Time) {
	const region, service = "us-east-1", "execute-api"
	amzDate := now.UTC().Format("20060102T150405Z")
	scopeDate := amzDate[:8]
	r.Header.Set("X-Amz-Date", amzDate)
	signedHeaders := "host;x-amz-date"

	canonical := canonicalRequest(r, signedHeaders, body)
	scope := strings.Join([]string{scopeDate, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256(canonical),
	}, "\n")
	key := hmacSHA256([]byte("AWS4"+secret), scopeDate)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, stringToSign))

	r.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		keyID, scopeDate, region, service, signedHeaders, sig))
}
