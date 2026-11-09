package sandboxproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"time"
)

// SecretStore is the slice of kamajik8s.Client the certificate sync uses.
type SecretStore interface {
	GetSecretData(ctx context.Context, ns, name string) (map[string][]byte, error)
	PatchSecretData(ctx context.Context, ns, name string, stringData map[string]string) error
}

// SecretRef names a Secret.
type SecretRef struct{ Namespace, Name string }

// SyncCert copies tls.crt and tls.key from src (the services cluster, where cert-manager writes the wildcard
// certificate) onto dst (the sandbox cluster's sandbox-proxy-tls, which must exist) when they differ. A source that
// is missing, half written, not a matching pair, not valid now or not for *.domain is not copied, so a working
// certificate is never replaced by a broken one (an operator's Secret with issuer: secret is checked like
// cert-manager's). The issuer and its DNS credential stay on the services cluster: only the certificate crosses.
func SyncCert(ctx context.Context, src SecretStore, from SecretRef, dst SecretStore, to SecretRef, domain string) (changed bool, err error) {
	in, err := src.GetSecretData(ctx, from.Namespace, from.Name)
	if err != nil {
		return false, fmt.Errorf("read %s/%s: %w", from.Namespace, from.Name, err)
	}
	crt, key := in["tls.crt"], in["tls.key"]
	if len(crt) == 0 || len(key) == 0 {
		return false, fmt.Errorf("%s/%s holds no certificate yet", from.Namespace, from.Name)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		return false, fmt.Errorf("%s/%s is not a usable certificate: %w", from.Namespace, from.Name, err)
	}
	if now := time.Now(); now.Before(pair.Leaf.NotBefore) || now.After(pair.Leaf.NotAfter) {
		return false, fmt.Errorf("%s/%s is valid from %s to %s only", from.Namespace, from.Name,
			pair.Leaf.NotBefore.Format(time.RFC3339), pair.Leaf.NotAfter.Format(time.RFC3339))
	}
	if domain == "" {
		return false, fmt.Errorf("no sandbox domain to check %s/%s against", from.Namespace, from.Name)
	}
	if err := pair.Leaf.VerifyHostname("49983-sbx-0." + domain); err != nil {
		return false, fmt.Errorf("%s/%s does not cover *.%s: %w", from.Namespace, from.Name, domain, err)
	}
	out, err := dst.GetSecretData(ctx, to.Namespace, to.Name)
	if err != nil {
		return false, fmt.Errorf("read %s/%s: %w", to.Namespace, to.Name, err)
	}
	if out == nil {
		return false, fmt.Errorf("%s/%s does not exist on the sandbox cluster", to.Namespace, to.Name)
	}
	if bytes.Equal(out["tls.crt"], crt) && bytes.Equal(out["tls.key"], key) {
		return false, nil
	}
	if err := dst.PatchSecretData(ctx, to.Namespace, to.Name, map[string]string{"tls.crt": string(crt), "tls.key": string(key)}); err != nil {
		return false, fmt.Errorf("write %s/%s: %w", to.Namespace, to.Name, err)
	}
	return true, nil
}
