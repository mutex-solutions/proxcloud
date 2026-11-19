package platformlife

// seed.go: what a seed VM the portal builds boots with (P3 design §4.3, §5): its k3s server CA and tokens, the one-shot
// bootstrap manifest and the NoCloud files of its CD. Pure: the run clones, uploads (pve.BuildSeedISO of Files()) and
// attaches; nothing here calls PVE or Kubernetes.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const (
	// SeedPortalNamespace, SeedPortalAccount and SeedPortalTokenSecret are the portal's cluster-admin service account on
	// the seed and its first token Secret: the one object the bootstrap token can read (stage 5).
	SeedPortalNamespace   = "proxcloud-system"
	SeedPortalAccount     = "proxcloud-portal"
	SeedPortalTokenSecret = "proxcloud-portal-token-g1"
	// SeedBootstrapRBAC names the Role and RoleBinding that grant that read. Stage 5 deletes them with the token's own
	// Secret, kube-system/bootstrap-token-<token id>.
	SeedBootstrapRBAC = "proxcloud-bootstrap"
	// SeedBootstrapTTL is the bootstrap token's lifetime: stages 4 and 5 use it within that, or the seed is rebuilt.
	SeedBootstrapTTL   = 3 * time.Hour
	seedBootstrapGroup = "system:bootstrappers:proxcloud"
)

// SeedVMName is the name of a seed VM. It is part of what makes a VM the installation's own (design §4.2 stage 3), with
// its VMID and its pool: a clone carries its template's tags, so tags cannot be.
func SeedVMName(installationID string, generation int) string {
	return fmt.Sprintf("pc-seed-%s-g%d", installationID, generation)
}

// SeedCDName is a generation's CD on the ISO store. Its base is the NoCloud instance-id, and the run deletes every other
// pcplat-<id>-* CD by name.
func SeedCDName(installationID string, generation int) string {
	return seedInstanceID(installationID, generation) + ".iso"
}

func seedInstanceID(installationID string, generation int) string {
	return fmt.Sprintf("pcplat-%s-g%d", installationID, generation)
}

// SeedSecrets are what one seed generation is made with. The run stores all but CAKey (design §3.3): the key lives in
// memory and on the CD only, so a CD lost before boot means a new generation.
type SeedSecrets struct {
	CACert, CAKey  string // PEM: the k3s server CA the portal pins TLS to
	K3sToken       string
	BootstrapToken string // <6>.<16> of [a-z0-9]: the bearer stages 4 and 5 send
}

var bootstrapTokenRE = regexp.MustCompile(`^([a-z0-9]{6})\.([a-z0-9]{16})$`)

// NewSeedSecrets makes a generation's server CA (ECDSA P-256, ten years, as k3s makes its own) and its two tokens.
func NewSeedSecrets(installationID string, generation int, now time.Time) (SeedSecrets, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return SeedSecrets{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return SeedSecrets{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "proxcloud-seed-server-ca@" + seedInstanceID(installationID, generation)},
		NotBefore:             now.Add(-time.Hour), // clock skew between the portal and the seed
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return SeedSecrets{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return SeedSecrets{}, err
	}
	// rand.Text is 26 base32 characters (130 bits); lower-cased it is inside the bootstrap token's [a-z0-9].
	lower := func(n int) string { return strings.ToLower(rand.Text())[:n] }
	return SeedSecrets{
		CACert:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CAKey:          string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		K3sToken:       rand.Text(),
		BootstrapToken: lower(6) + "." + lower(16),
	}, nil
}

// BootstrapManifest is what the seed applies once at first boot, from tmpfs: the portal's service account, cluster-admin,
// with its token Secret, and a bootstrap token that expires at expires and whose group may get only that Secret.
func BootstrapManifest(bootstrapToken string, expires time.Time) (string, error) {
	m := bootstrapTokenRE.FindStringSubmatch(bootstrapToken)
	if m == nil {
		return "", fmt.Errorf("platform: bootstrap token is not <6>.<16> of [a-z0-9]")
	}
	ns := SeedPortalNamespace
	account := []any{map[string]any{"kind": "ServiceAccount", "name": SeedPortalAccount, "namespace": ns}}
	list := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		object("v1", "Namespace", "", ns),
		object("v1", "ServiceAccount", ns, SeedPortalAccount),
		with(object("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", SeedPortalAccount), map[string]any{
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "cluster-admin"},
			"subjects": account,
		}),
		with(object("v1", "Secret", ns, SeedPortalTokenSecret), map[string]any{
			"type": "kubernetes.io/service-account-token",
		}, "kubernetes.io/service-account.name", SeedPortalAccount),
		with(object("v1", "Secret", "kube-system", "bootstrap-token-"+m[1]), map[string]any{
			"type": "bootstrap.kubernetes.io/token",
			"stringData": map[string]any{
				"description":                    "ProxCloud portal: reads " + ns + "/" + SeedPortalTokenSecret + " once",
				"token-id":                       m[1],
				"token-secret":                   m[2],
				"expiration":                     expires.UTC().Format(time.RFC3339),
				"usage-bootstrap-authentication": "true",
				"auth-extra-groups":              seedBootstrapGroup,
			},
		}),
		with(object("rbac.authorization.k8s.io/v1", "Role", ns, SeedBootstrapRBAC), map[string]any{
			"rules": []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"},
				"resourceNames": []any{SeedPortalTokenSecret}, "verbs": []any{"get"}}},
		}),
		with(object("rbac.authorization.k8s.io/v1", "RoleBinding", ns, SeedBootstrapRBAC), map[string]any{
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": SeedBootstrapRBAC},
			"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": seedBootstrapGroup}},
		}),
	}}
	return jsonText(list)
}

// SeedBoot is one seed generation's first boot.
type SeedBoot struct {
	Generation int
	Net0       string // the clone's net0 read back from PVE after SetVMConfig: its MAC is what the network config matches
	Secrets    SeedSecrets
	Now        time.Time // the bootstrap token expires SeedBootstrapTTL later
}

// SeedUserData is the seed's cloud-config (design §4.3): the server CA before k3s first starts, k3s's config, the
// bootstrap manifest on tmpfs, then k3s from the BOM checked by sha256, one apply of the manifest, and cloud-init turned
// off, so a later boot keeps the network it wrote and nothing reapplies the manifest. The CA key, the k3s token and the
// bootstrap token are the only secrets in it (§5).
func SeedUserData(b *BOM, in RenderInput, boot SeedBoot) (string, error) {
	manifest, err := BootstrapManifest(boot.Secrets.BootstrapToken, boot.Now.Add(SeedBootstrapTTL))
	if err != nil {
		return "", err
	}
	k3sConfig, err := jsonText(map[string]any{
		"cluster-init":          true,
		"token":                 boot.Secrets.K3sToken,
		"tls-san":               []any{in.SeedAddress},
		"node-ip":               in.SeedAddress,
		"write-kubeconfig-mode": "0600",
		"disable":               []any{"traefik", "servicelb", "local-storage"},
		"secrets-encryption":    true,
		"kube-apiserver-arg":    []any{"anonymous-auth=false", "enable-bootstrap-token-auth=true"},
	})
	if err != nil {
		return "", err
	}
	file := func(path, content, mode string) any {
		return map[string]any{"path": path, "content": content, "permissions": mode, "owner": "root:root"}
	}
	k := b.Seed.K3s
	fetch := k3sFetch(k, "/usr/local/bin/k3s")
	raw, err := jsonText(map[string]any{
		"write_files": []any{
			file("/var/lib/rancher/k3s/server/tls/server-ca.crt", boot.Secrets.CACert, "0644"),
			file("/var/lib/rancher/k3s/server/tls/server-ca.key", boot.Secrets.CAKey, "0600"),
			file("/etc/rancher/k3s/config.yaml", k3sConfig, "0600"),
			file("/run/proxcloud/bootstrap.yaml", manifest, "0600"),
		},
		// cloud-init writes these lines into one sh script: set -eu stops it at the first failure, so a k3s that fails
		// its checksum never starts, and the seed never answers (stage 4 times out, Retry rebuilds it).
		"runcmd": []any{
			"set -eu",
			fetch[0], fetch[1], fetch[2],
			"curl -fsSL --retry 5 -o /run/proxcloud/install.sh " + q(k.InstallScript.URL),
			"printf '%s  %s\\n' " + q(k.InstallScript.SHA256) + " /run/proxcloud/install.sh | sha256sum -c -",
			"INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_VERSION=" + q(k.Version) + " sh /run/proxcloud/install.sh",
			"timeout 600 sh -c 'until k3s kubectl get --raw /readyz >/dev/null 2>&1; do sleep 5; done'",
			"k3s kubectl apply -f /run/proxcloud/bootstrap.yaml",
			"rm -f /run/proxcloud/bootstrap.yaml /run/proxcloud/install.sh",
			"touch /etc/cloud/cloud-init.disabled",
		},
	})
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + raw, nil
}

// q quotes s for sh.
func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// k3sFetch are the sh lines that bring the BOM's k3s binary to path: the download, its sha256 check and the mode. The
// first boot (SeedUserData) and the upgrade Job (SeedK3sJob) both run exactly these, under set -eu.
func k3sFetch(k K3s, path string) [3]string {
	return [3]string{
		"curl -fsSL --retry 5 -o " + path + " " + q(k.Binary.URL),
		"printf '%s  %s\\n' " + q(k.Binary.SHA256) + " " + path + " | sha256sum -c -",
		"chmod 0755 " + path,
	}
}

// SeedFiles are the NoCloud files of a generation's CD, through the tenant seed's renderers: meta-data with the
// instance-id and hostname, vendor-data with the break-glass SSH keys, a static network config from the clone's net0.
func SeedFiles(b *BOM, in RenderInput, boot SeedBoot) (pve.Seed, error) {
	network, err := pve.RenderNetworkConfig(pve.VMConfig{
		"net0":      boot.Net0,
		"ipconfig0": fmt.Sprintf("ip=%s/%d,gw=%s", in.SeedAddress, in.Network.Prefix, in.Network.Gateway),
	}, in.Network.DNS)
	if err != nil {
		return pve.Seed{}, err
	}
	userData, err := SeedUserData(b, in, boot)
	if err != nil {
		return pve.Seed{}, err
	}
	return pve.RenderSeed(pve.SeedSpec{
		InstanceID: seedInstanceID(in.ID, boot.Generation),
		Hostname:   SeedVMName(in.ID, boot.Generation),
		UserData:   userData,
		SSHKeys:    strings.Join(in.SSHKeys, "\n"),
		Network:    network,
	})
}

// object is a Kubernetes object with its type and name; with adds the rest.
func object(apiVersion, kind, namespace, name string) map[string]any {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}
}

// with sets fields on obj and, given key/value pairs, annotations.
func with(obj map[string]any, fields map[string]any, annotations ...string) map[string]any {
	for k, v := range fields {
		obj[k] = v
	}
	if len(annotations) > 0 {
		a := map[string]any{}
		for i := 0; i+1 < len(annotations); i += 2 {
			a[annotations[i]] = annotations[i+1]
		}
		obj["metadata"].(map[string]any)["annotations"] = a
	}
	return obj
}

// jsonText is indented JSON, which cloud-init, k3s and kubectl read as YAML, with < > & left as they are.
func jsonText(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return buf.String(), err
}
