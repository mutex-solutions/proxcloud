package sso

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// A cached snapshot is sufficient for these boundary decisions: no repository or user lookup is
// needed for a retired alias or for the no-org-providers fast path.
func TestGuardRetiredAliasBeforeEmptyProviderShortcut(t *testing.T) {
	const issuer = "https://id.test/realms/customers"
	const retired = "org-0123456789ab"
	request := func(alias string) *httpx.RequestContext {
		return &httpx.RequestContext{Sub: "person", Issuer: issuer, Azp: "proxcloud-ui", IdP: alias, SSOScope: true}
	}
	cases := []struct {
		name  string
		other bool
		rc    *httpx.RequestContext
		want  string
	}{
		{"retired last provider", false, request(retired), RefusedIdPDisabled},
		{"retired with another provider", true, request(retired), RefusedIdPDisabled},
		{"native local with no providers", false, request(""), ""},
		{"native local with another provider", true, request(""), ""},
		{"operator-added social with no providers", false, request("github"), ""},
		{"unknown org-shaped alias with no providers", false, request("org-fedcba987654"), ""},
		{"not an alias prefix denylist", false, request(retired + "-social"), ""},
		{"unauthenticated", false, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{opt: Options{MainIssuer: issuer}, seen: newTTLCache[bool](time.Hour)}
			s.snap.cur = &snapshot{byAlias: map[string]*Provider{}, retired: map[string]struct{}{retired: {}}}
			s.snap.load = time.Now()
			if tc.other {
				s.snap.cur.byAlias["org-111111111111"] = &Provider{Alias: "org-111111111111", Protocol: protoOIDC, Enabled: true}
			}
			// Suppress the already-covered audit side effect; the refusal decision must precede any DB work.
			s.seen.put("person||"+actionLoginRefused+"|"+RefusedIdPDisabled, true)
			if got := s.Guard(context.Background(), tc.rc); got != tc.want {
				t.Fatalf("Guard = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGuardRetirementKeepsExistingExemptions(t *testing.T) {
	const issuer = "https://id.test/realms/customers"
	const retired = "org-0123456789ab"
	for _, tc := range []struct {
		name   string
		mutate func(*httpx.RequestContext)
		want   string
	}{
		{"operator own realm", func(rc *httpx.RequestContext) { rc.Issuer = "https://id.test/realms/operators" }, ""},
		{"HMAC key", func(rc *httpx.RequestContext) { rc.SigV4KeyID = "key" }, ""},
		{"no principal", func(rc *httpx.RequestContext) { rc.Sub = "" }, ""},
		{"operator local customer realm", func(rc *httpx.RequestContext) { rc.Azp, rc.IdP = "proxcloud-admin", "" }, ""},
		{"operator broker customer realm", func(rc *httpx.RequestContext) { rc.Azp = "proxcloud-admin" }, RefusedOperator},
		{"retired claim without SSO scope", func(rc *httpx.RequestContext) { rc.SSOScope = false }, RefusedIdPDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{opt: Options{MainIssuer: issuer, OperatorClients: []string{"proxcloud-admin"}}, seen: newTTLCache[bool](time.Hour)}
			s.snap.cur = &snapshot{byAlias: map[string]*Provider{}, retired: map[string]struct{}{retired: {}}}
			s.snap.load = time.Now()
			rc := &httpx.RequestContext{Sub: "person", Issuer: issuer, Azp: "proxcloud-ui", IdP: retired, SSOScope: true}
			tc.mutate(rc)
			for _, code := range []string{RefusedOperator, RefusedIdPDisabled} {
				s.seen.put("person||"+actionLoginRefused+"|"+code, true)
			}
			if got := s.Guard(context.Background(), rc); got != tc.want {
				t.Fatalf("Guard = %q, want %q", got, tc.want)
			}
		})
	}
}
