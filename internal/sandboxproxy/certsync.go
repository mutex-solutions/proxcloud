package sandboxproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
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
// is missing, half written or not a matching pair is not copied, so a working certificate is never replaced by a
// broken one. The issuer and its DNS credential stay on the services cluster: only the certificate crosses.
func SyncCert(ctx context.Context, src SecretStore, from SecretRef, dst SecretStore, to SecretRef) (changed bool, err error) {
	in, err := src.GetSecretData(ctx, from.Namespace, from.Name)
	if err != nil {
		return false, fmt.Errorf("read %s/%s: %w", from.Namespace, from.Name, err)
	}
	crt, key := in["tls.crt"], in["tls.key"]
	if len(crt) == 0 || len(key) == 0 {
		return false, fmt.Errorf("%s/%s holds no certificate yet", from.Namespace, from.Name)
	}
	if _, err := tls.X509KeyPair(crt, key); err != nil {
		return false, fmt.Errorf("%s/%s is not a usable certificate: %w", from.Namespace, from.Name, err)
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
