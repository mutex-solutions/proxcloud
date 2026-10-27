// Package kamajik8s is a deliberately minimal Kubernetes API client for the Kamaji
// MANAGEMENT cluster: enough REST to apply/read/delete a handful of namespaced objects
// (ArgoCD Application, Secret, Namespace, TenantControlPlane, MachineDeployment) over a
// kubeconfig, and nothing else.
//
// ponytail: hand-rolled REST instead of k8s.io/client-go — we need 6 verbs on 5 kinds, no
// watch/informers/discovery, and client-go would be the largest dependency in go.mod by far.
// Upgrade path: swap this package for client-go dynamic if we ever need watches or exec.
package kamajik8s

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// kubeconfig models the subset of a kubeconfig file we consume: current-context resolved to
// one cluster (server + CA) and one user (client cert/key or bearer token). Exec plugins,
// auth-provider blocks and proxy-url are NOT supported — the proxcloud service account is a
// plain token/cert kubeconfig minted for it (see the plan's mgmt-RBAC item).
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                string `yaml:"server"`
			CertificateAuthority  string `yaml:"certificate-authority"`
			CAData                string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKey             string `yaml:"client-key"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
}

// restConfig is the resolved connection: base URL + auth, ready to build an http.Client.
type restConfig struct {
	server  string
	token   string
	tlsCfg  *tls.Config
	timeout time.Duration
}

// parseKubeconfig resolves the current-context of a kubeconfig into a restConfig.
func parseKubeconfig(raw []byte) (*restConfig, error) {
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("kubeconfig: parse: %w", err)
	}
	if kc.CurrentContext == "" {
		return nil, fmt.Errorf("kubeconfig: no current-context")
	}
	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			clusterName, userName = c.Context.Cluster, c.Context.User
			break
		}
	}
	if clusterName == "" {
		return nil, fmt.Errorf("kubeconfig: context %q not found", kc.CurrentContext)
	}

	rc := &restConfig{timeout: 30 * time.Second}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		if _, err := url.Parse(c.Cluster.Server); err != nil || c.Cluster.Server == "" {
			return nil, fmt.Errorf("kubeconfig: cluster %q has no valid server", clusterName)
		}
		rc.server = c.Cluster.Server
		tlsCfg.InsecureSkipVerify = c.Cluster.InsecureSkipTLSVerify
		ca, err := load(c.Cluster.CAData, c.Cluster.CertificateAuthority)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: cluster CA: %w", err)
		}
		if len(ca) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ca) {
				return nil, fmt.Errorf("kubeconfig: cluster CA: no PEM certs")
			}
			tlsCfg.RootCAs = pool
		}
	}
	if rc.server == "" {
		return nil, fmt.Errorf("kubeconfig: cluster %q not found", clusterName)
	}
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		rc.token = u.User.Token
		cert, err := load(u.User.ClientCertificateData, u.User.ClientCertificate)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: client cert: %w", err)
		}
		key, err := load(u.User.ClientKeyData, u.User.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig: client key: %w", err)
		}
		if len(cert) > 0 && len(key) > 0 {
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig: client keypair: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{pair}
		}
	}
	if rc.token == "" && len(tlsCfg.Certificates) == 0 {
		return nil, fmt.Errorf("kubeconfig: user %q has neither token nor client certificate", userName)
	}
	rc.tlsCfg = tlsCfg
	return rc, nil
}

// load returns inline base64 data when present, else reads the referenced file (both empty → nil).
func load(b64, path string) ([]byte, error) {
	if b64 != "" {
		return base64.StdEncoding.DecodeString(b64)
	}
	if path != "" {
		return os.ReadFile(path)
	}
	return nil, nil
}

// httpClient builds the TLS-configured http.Client for this restConfig.
func (rc *restConfig) httpClient() *http.Client {
	return &http.Client{
		Timeout:   rc.timeout,
		Transport: &http.Transport{TLSClientConfig: rc.tlsCfg},
	}
}
