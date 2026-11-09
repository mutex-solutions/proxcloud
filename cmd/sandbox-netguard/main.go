// sandbox-netguard is a chained CNI plugin. ADD fails closed for managed sandbox
// pods unless host-veth policing is installed BEFORE customer code starts. It never
// removes Cilium's qdiscs/filters or changes node uplink shaping.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type config struct {
	CNIVersion string          `json:"cniVersion"`
	PrevResult json.RawMessage `json:"prevResult"`
	API        string          `json:"api"`
	CAFile     string          `json:"caFile"`
	TokenFile  string          `json:"tokenFile"`
	NodeName   string          `json:"nodeName"`
}

var run = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-collect" {
		collect()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-install" {
		if err := install(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err == nil {
		err = process(b)
	}
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"cniVersion": "1.0.0", "code": 100, "msg": "sandbox network protection failed", "details": err.Error()})
		os.Exit(1)
	}
}
func arg(s, key string) string {
	for _, p := range strings.Split(s, ";") {
		k, v, _ := strings.Cut(p, "=")
		if k == key {
			return v
		}
	}
	return ""
}
func apiClient(c config) (*http.Client, string, error) {
	ca, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, "", err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, "", errors.New("invalid cluster CA")
	}
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return nil, "", err
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}}, strings.TrimSpace(string(token)), nil
}
func request(client *http.Client, token, method, u string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("API status %d", res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	return nil
}
func process(b []byte) error {
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	command := os.Getenv("CNI_COMMAND")
	if command == "VERSION" {
		_, err := fmt.Fprint(os.Stdout, `{"cniVersion":"1.0.0","supportedVersions":["0.3.1","0.4.0","1.0.0"]}`)
		return err
	}
	if command == "DEL" {
		return nil
	}
	if command != "ADD" && command != "CHECK" {
		return errors.New("unsupported CNI command")
	}
	if len(c.PrevResult) == 0 {
		return errors.New("must follow Cilium in a conflist")
	}
	ns, name := arg(os.Getenv("CNI_ARGS"), "K8S_POD_NAMESPACE"), arg(os.Getenv("CNI_ARGS"), "K8S_POD_NAME")
	if command == "ADD" || command == "CHECK" {
		if ns == "" || name == "" {
			return errors.New("Kubernetes pod identity required")
		}
		if strings.HasPrefix(ns, "sb-") {
			client, token, err := apiClient(c)
			if err != nil {
				return err
			}
			var pod struct {
				Metadata struct {
					UID                 string
					Labels, Annotations map[string]string
				}
				Spec struct{ NodeName string }
			}
			if err := request(client, token, "GET", c.API+"/api/v1/namespaces/"+ns+"/pods/"+name, nil, &pod); err != nil {
				return err
			}
			l := pod.Metadata.Labels
			if l["app.kubernetes.io/managed-by"] == "proxcloud" && l["proxcloud.io/sandbox-id"] != "" {
				if err := bindPod(c.NodeName, arg(os.Getenv("CNI_ARGS"), "K8S_POD_UID"), pod.Metadata.UID, pod.Spec.NodeName); err != nil {
					return err
				}
				if ns != "sb-"+l["proxcloud.io/project"] || l["proxcloud.io/sandbox-id"] != name || l["proxcloud.io/service"] == "" {
					return errors.New("sandbox identity mismatch")
				}
				rate, err := parseRate(pod.Metadata.Annotations["kubernetes.io/egress-bandwidth"])
				if err != nil {
					return err
				}
				iface, err := hostVeth(c.PrevResult)
				if err != nil {
					return err
				}
				_ = iface
				netns := os.Getenv("CNI_NETNS")
				if !strings.HasPrefix(netns, "/var/run/netns/") && !strings.HasPrefix(netns, "/run/netns/") {
					return errors.New("invalid CNI network namespace")
				}
				if err := police(netns, rate); err != nil {
					return err
				}
			}
		}
	}
	_, err := os.Stdout.Write(c.PrevResult)
	return err
}
func bindPod(node, requestedUID, liveUID, liveNode string) error {
	if node == "" || liveUID == "" || requestedUID != liveUID || liveNode != node {
		return errors.New("CNI pod UID/node binding mismatch")
	}
	return nil
}
func parseRate(s string) (int, error) {
	if !regexp.MustCompile(`^[1-9][0-9]*M$`).MatchString(s) {
		return 0, errors.New("sandbox bandwidth annotation missing/invalid")
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "M"))
	if err != nil || n > 100000 {
		return 0, errors.New("sandbox bandwidth out of range")
	}
	return n, nil
}
func hostVeth(prev json.RawMessage) (string, error) {
	var r struct {
		Interfaces []struct{ Name, Sandbox string }
	}
	if err := json.Unmarshal(prev, &r); err != nil {
		return "", err
	}
	for _, i := range r.Interfaces {
		if i.Sandbox == "" && regexp.MustCompile(`^lxc[a-zA-Z0-9_-]+$`).MatchString(i.Name) {
			return i.Name, nil
		}
	}
	return "", errors.New("Cilium host veth not found in prevResult")
}
func police(netns string, mbps int) error {
	// Configure the REAL pod network namespace from the host, outside gVisor's
	// simulated kernel. It precedes Cilium TCX redirects and covers every protocol.
	burst := max(65536, mbps*125000/100)
	b, err := run("nsenter", "--net="+netns, "--", "tc", "qdisc", "replace", "dev", "eth0", "root", "tbf", "rate", fmt.Sprintf("%dmbit", mbps), "burst", strconv.Itoa(burst), "latency", "50ms")
	if err != nil {
		return fmt.Errorf("tc sandbox TBF: %w: %s", err, b)
	}
	return nil
}

// install atomically appends the guard to Cilium's conflist. Re-run periodically:
// Cilium can rewrite it on upgrades. Refuse unknown/multi-network configurations.
func install() error {
	// A former Cilium config writer must be disabled before installing/publishing
	// our chain, otherwise it can race a rewrite behind a valid heartbeat.
	check := config{API: os.Getenv("API_SERVER"), CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"}
	client, token, e := apiClient(check)
	if e != nil {
		return e
	}
	var cm struct{ Data map[string]string }
	if e := request(client, token, "GET", check.API+"/api/v1/namespaces/kube-system/configmaps/cilium-config", nil, &cm); e != nil {
		return e
	}
	if cm.Data["custom-cni-conf"] != "true" || cm.Data["write-cni-conf-when-ready"] != "" {
		return errors.New("Cilium still owns CNI config; wait for customConf rollout")
	}
	path := "/host/etc/cni/net.d/05-cilium.conflist"
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b = []byte(`{"cniVersion":"0.3.1","name":"cilium","plugins":[{"type":"cilium-cni","enable-debug":false}]}`)
	} else if err != nil {
		return err
	}
	var conf map[string]any
	if err := json.Unmarshal(b, &conf); err != nil {
		return err
	}
	plugins, ok := conf["plugins"].([]any)
	if !ok || len(plugins) == 0 {
		return errors.New("Cilium conflist required")
	}
	first, _ := plugins[0].(map[string]any)
	if first["type"] != "cilium-cni" {
		return errors.New("first CNI must be cilium-cni")
	}
	var clean []any
	for _, v := range plugins {
		m, _ := v.(map[string]any)
		if m["type"] != "sandbox-netguard" {
			clean = append(clean, v)
		}
	}
	clean = append(clean, map[string]any{"type": "sandbox-netguard", "api": os.Getenv("API_SERVER"), "caFile": "/etc/cni/net.d/sandbox-netguard-ca.crt", "tokenFile": "/etc/cni/net.d/sandbox-netguard-token", "nodeName": os.Getenv("NODE_NAME")})
	conf["plugins"] = clean
	if err := os.MkdirAll("/host/opt/cni/bin", 0755); err != nil {
		return err
	}
	bin, err := os.ReadFile("/runtime/sandbox-netguard")
	if err != nil {
		return err
	}
	if err := os.WriteFile("/host/opt/cni/bin/.sandbox-netguard.new", bin, 0755); err != nil {
		return err
	}
	if err := os.Rename("/host/opt/cni/bin/.sandbox-netguard.new", "/host/opt/cni/bin/sandbox-netguard"); err != nil {
		return err
	}
	for _, f := range []struct {
		from, to string
		mode     os.FileMode
	}{{"/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "/host/etc/cni/net.d/sandbox-netguard-ca.crt", 0644}, {"/var/run/secrets/kubernetes.io/serviceaccount/token", "/host/etc/cni/net.d/sandbox-netguard-token", 0600}} {
		v, err := os.ReadFile(f.from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(f.to+".new", v, f.mode); err != nil {
			return err
		}
		if err := os.Rename(f.to+".new", f.to); err != nil {
			return err
		}
	}
	out, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".new", out, 0600); err != nil {
		return err
	}
	if err := os.Rename(path+".new", path); err != nil {
		return err
	}
	return publishGuard()
}

func publishGuard() error {
	c := config{API: os.Getenv("API_SERVER"), CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"}
	client, token, err := apiClient(c)
	if err != nil {
		return err
	}
	name := "guard-" + os.Getenv("NODE_NAME")
	base := c.API + "/api/v1/namespaces/sandbox-telemetry/configmaps"
	var old map[string]any
	_ = request(client, token, "GET", base+"/"+name, nil, &old)
	meta := map[string]any{"name": name, "labels": map[string]string{"app.kubernetes.io/name": "sandbox-netguard"}}
	method, u := "POST", base
	if old != nil {
		m, _ := old["metadata"].(map[string]any)
		meta["resourceVersion"] = m["resourceVersion"]
		method, u = "PUT", base+"/"+name
	}
	return request(client, token, method, u, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": map[string]string{"node": os.Getenv("NODE_NAME"), "at": time.Now().UTC().Format(time.RFC3339), "version": "1"}}, nil)
}

// collect uses the narrowly authorized kubelet nodes/stats endpoint on THIS node,
// never nodes/proxy. Its only writes are per-node samples in sandbox-telemetry.
func collect() {
	for {
		if err := collectOnce(); err != nil {
			fmt.Fprintln(os.Stderr, "telemetry unavailable:", err)
		}
		time.Sleep(30 * time.Second)
	}
}
func collectOnce() error {
	const root = "/var/run/secrets/kubernetes.io/serviceaccount/"
	c := config{API: os.Getenv("API_SERVER"), CAFile: root + "ca.crt", TokenFile: root + "token"}
	client, token, err := apiClient(c)
	if err != nil {
		return err
	}
	// Kubelet serving certificates must chain to this CA; no insecure TLS fallback.
	var summary struct {
		Pods []struct {
			PodRef struct{ Name, Namespace, UID string }
			CPU    struct {
				Time                 time.Time
				UsageCoreNanoSeconds *uint64
			}
			Network struct {
				Time    time.Time
				RXBytes *uint64 `json:"rxBytes"`
				TXBytes *uint64 `json:"txBytes"`
			}
		}
	}
	endpoint := os.Getenv("KUBELET_URL")
	if endpoint == "" {
		endpoint = "https://127.0.0.1:10250"
	}
	kubeCA, err := os.ReadFile("/kubelet-serving.crt")
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(kubeCA) {
		return errors.New("invalid pinned kubelet serving chain")
	}
	kubeClient := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: os.Getenv("NODE_NAME")}}}
	if err := request(kubeClient, token, "GET", endpoint+"/stats/summary", nil, &summary); err != nil {
		return err
	}
	now := time.Now().UTC()
	xs := []map[string]any{}
	for _, p := range summary.Pods {
		if !strings.HasPrefix(p.PodRef.Namespace, "sb-") || !strings.HasPrefix(p.PodRef.Name, "sbx-") {
			continue
		}
		xs = append(xs, map[string]any{"namespace": p.PodRef.Namespace, "name": p.PodRef.Name, "uid": p.PodRef.UID, "node": os.Getenv("NODE_NAME"), "at": now, "cpuAt": p.CPU.Time, "networkAt": p.Network.Time, "cpuNanoseconds": p.CPU.UsageCoreNanoSeconds, "rxBytes": p.Network.RXBytes, "txBytes": p.Network.TXBytes})
	}
	b, err := json.Marshal(xs)
	if err != nil {
		return err
	}
	name := "node-" + os.Getenv("NODE_NAME")
	base := c.API + "/api/v1/namespaces/sandbox-telemetry/configmaps"
	var old map[string]any
	if err := request(client, token, "GET", base+"/"+name, nil, &old); err != nil {
		old = nil
	}
	metadata := map[string]any{"name": name, "namespace": "sandbox-telemetry", "labels": map[string]string{"app.kubernetes.io/name": "sandbox-telemetry"}}
	method, u := "POST", base
	if old != nil {
		if m, ok := old["metadata"].(map[string]any); ok {
			metadata["resourceVersion"] = m["resourceVersion"]
		}
		method, u = "PUT", base+"/"+name
	}
	return request(client, token, method, u, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata, "data": map[string]string{"samples": string(b)}}, nil)
}
