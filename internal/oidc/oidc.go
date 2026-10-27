// Package oidc builds per-realm OIDC verifiers from the auth.* config
// (Keycloak is the issuer). It only performs issuer discovery and logs the
// outcome; token validation with these verifiers happens in pkg/auth.
package oidc

import (
	"context"
	"log/slog"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/mutex-solutions/proxcloud/internal/config"
)

// Realm pairs a verifier with its config.
type Realm struct {
	Name      string
	IssuerURI string
	ClientID  string
	Verifier  *oidc.IDTokenVerifier
}

// Discover attempts JWKS/issuer discovery for each configured realm. Missing
// issuer URIs (LOCAL_IDP realms) are skipped. Errors are logged, not fatal —
// discovery may be unreachable before the IdP is up.
func Discover(ctx context.Context, cfg *config.Config, log *slog.Logger) []Realm {
	specs := []struct {
		name  string
		realm config.OAuth2Realm
	}{
		{"main", cfg.Auth.Main},
		{"admin", cfg.Auth.Admin},
		{"admin-api", cfg.Auth.AdminAPI},
	}

	var out []Realm
	for _, s := range specs {
		if s.realm.IssuerURI == "" {
			continue
		}
		r := Realm{Name: s.name, IssuerURI: s.realm.IssuerURI, ClientID: s.realm.ClientID}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		// Fetch metadata from DiscoveryURI when the IdP is reachable under a different
		// URL than the issuer the browser sees (reverse proxy / container network).
		// The token's `iss` is still checked against IssuerURI — go-oidc keeps
		// enforcing it, we only relax WHERE the document is fetched from.
		fetchFrom := s.realm.IssuerURI
		if s.realm.DiscoveryURI != "" {
			fetchFrom = s.realm.DiscoveryURI
			dctx = oidc.InsecureIssuerURLContext(dctx, s.realm.IssuerURI)
		}
		provider, err := oidc.NewProvider(dctx, fetchFrom)
		cancel()
		if err != nil {
			log.Warn("oidc issuer discovery failed (non-fatal)", "realm", s.name, "issuer", s.realm.IssuerURI, "err", err)
		} else {
			// SkipClientIDCheck: Keycloak public-client tokens carry aud=account,
			// not the client id. Issuer+signature+expiry are still enforced. The
			// authorized-party binding (azp == client-id) is enforced in pkg/auth.verifyJWT
			// (azpAllowed) so aud=account tokens keep passing the verifier.
			r.Verifier = provider.Verifier(&oidc.Config{ClientID: s.realm.ClientID, SkipClientIDCheck: true})
			log.Info("oidc realm discovered", "realm", s.name, "issuer", s.realm.IssuerURI)
		}
		out = append(out, r)
	}
	return out
}
