package project

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// What Services that end TLS make of a cluster's balancer. The config is a golden beside the renderer's own TLS
// goldens (internal/cloud/pve/testdata), which the container test hands to the HAProxy of both appliance images
// with real certificate files under the two names (test/integration/lb_tls_test.go).
func TestLBClusterServicesEndTLS(t *testing.T) {
	hashA, hashB, hashRefused, hashGone := strings.Repeat("0a", 32), strings.Repeat("0b", 32), strings.Repeat("0c", 32), strings.Repeat("0d", 32)
	fileA, fileB := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	rows := map[string]*lbCertificate{
		hashA:       {LBCertificate: pve.LBCertificate{File: fileA}},
		hashB:       {LBCertificate: pve.LBCertificate{File: fileB}},
		hashRefused: {Refused: "The private key does not belong to the certificate"},
	}
	ref := func(name, hash string) lbgw.CertRef { return lbgw.CertRef{Secret: name, SHA256: hash} }
	services := []lbgw.Service{
		// 443 ends TLS and speaks HTTP to the members, 80 stays as it is, and the UDP port is never TLS.
		{UID: "shop", Namespace: "web", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30080}, {Protocol: "TCP", Port: 443, NodePort: 30443}, {Protocol: "UDP", Port: 443, NodePort: 30443}},
			ExternalTrafficPolicy: "Cluster", Certificates: []lbgw.CertRef{ref("shop-tls", hashA), ref("api-tls", hashB)},
			Annotations: map[string]string{"proxcloud.io/lb-tls-secret": "shop-tls,api-tls", "proxcloud.io/lb-tls-ports": "443", "proxcloud.io/lb-x-forwarded-for": "true"}},
		// TLS ends on the balancer and the members get plain TCP, with the client's address in a PROXY header.
		{UID: "mqtt", Namespace: "iot", Ports: []lbgw.Port{{Protocol: "TCP", Port: 8883, NodePort: 31883}}, ExternalTrafficPolicy: "Cluster",
			Certificates: []lbgw.CertRef{ref("mqtt-tls", hashB)},
			Annotations:  map[string]string{"proxcloud.io/lb-tls-secret": "mqtt-tls", "proxcloud.io/lb-backend-protocol": "tcp", "proxcloud.io/lb-proxy-protocol": "v2"}},
	}
	vips := []string{"203.0.113.10", "203.0.113.11"}
	var spec pve.LBSpec
	for i, w := range services {
		if _, reason, msg := lbgw.CheckService(&w, i, lbgw.Protocol); reason != "" {
			t.Fatalf("%s is refused: %s %s", w.UID, reason, msg)
		}
		spec = lbClusterServiceSpecTLS(spec, w, vips[i], []string{"10.0.0.250", "10.0.0.249"}, "c1", rows)
	}
	got, path := pve.RenderClusterHAProxy(spec, vips), "../../cloud/pve/testdata/haproxy-tls-k8s.cfg"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want, err := os.ReadFile(path); err != nil || got != string(want) {
		t.Errorf("haproxy-tls-k8s.cfg differs (%v):\n%s", err, got)
	}
	listener := func(port int, protocol string) pve.LBListener {
		t.Helper()
		i := slices.IndexFunc(spec.Listeners, func(l pve.LBListener) bool { return l.Port == port && l.Protocol == protocol })
		if i < 0 {
			t.Fatalf("no %s listener on %d in %+v", protocol, port, spec.Listeners)
		}
		return spec.Listeners[i]
	}
	pool := func(l pve.LBListener) pve.LBPool {
		return spec.Pools[slices.IndexFunc(spec.Pools, func(p pve.LBPool) bool { return p.Name == l.Pool })]
	}
	https, tls, plain, udp := listener(443, "https"), listener(8883, "tls"), listener(80, "http"), listener(443, "udp")
	if !slices.Equal(https.CertFiles, []string{fileA, fileB}) || !slices.Equal(https.Certificates, []string{lbClusterCertID("c1", hashA), lbClusterCertID("c1", hashB)}) || pool(https).Protocol != "http" {
		t.Errorf("the https listener: %+v, pool %s", https, pool(https).Protocol)
	}
	if !slices.Equal(tls.CertFiles, []string{fileB}) || pool(tls).Protocol != "tcp" || !pool(tls).ProxyProtocol {
		t.Errorf("the tls listener: %+v, pool %+v", tls, pool(tls))
	}
	if plain.Certificates != nil || udp.Certificates != nil || pool(udp).Protocol != "udp" {
		t.Errorf("the ports that end no TLS: %+v %+v", plain, udp)
	}
	// The same Secret content in another cluster is another row: no id of one cluster names a row of another.
	if lbClusterCertID("c1", hashA) == lbClusterCertID("c2", hashA) || lbClusterCertOwner("c1") == lbClusterCertOwner("c2") {
		t.Error("two clusters share a certificate row")
	}
	// A spec the balancer's own validation takes (it clears what only the platform sets, so it runs on a copy).
	check := pve.LBSpec{Listeners: slices.Clone(spec.Listeners)}
	for _, p := range spec.Pools {
		p.Members = nil
		check.Pools = append(check.Pools, p)
	}
	for i := range check.Listeners {
		check.Listeners[i].Port = 1000 + i
	}
	if msg := check.Validate(); msg != "" {
		t.Fatalf("the spec does not pass the balancer's own validation: %s", msg)
	}

	// A certificate the cluster holds no row for, or one the check refused, is in no config; a listener left
	// without any gets no frontend at all, and never one without TLS.
	for name, refs := range map[string][]lbgw.CertRef{"a certificate that is gone": {ref("x", hashGone)}, "a refused certificate": {ref("x", hashRefused)}} {
		w := lbgw.Service{UID: "x", Ports: []lbgw.Port{{Protocol: "TCP", Port: 443, NodePort: 30443}}, Certificates: refs}
		one := lbClusterServiceSpecTLS(pve.LBSpec{}, w, "203.0.113.10", []string{"10.0.0.250"}, "c1", rows)
		cfg := pve.RenderClusterHAProxy(one, []string{"203.0.113.10"})
		if one.Listeners[0].Protocol != "https" || one.Listeners[0].Certificates != nil || strings.Contains(cfg, ":443") {
			t.Errorf("%s: %+v\n%s", name, one.Listeners[0], cfg)
		}
	}
	// A Service that came without certificates is the Service it was before this release, whatever its annotations
	// say: the portal holds it back before it gets here (lbClusterTLS), and nothing here ends TLS without a file.
	w := lbgw.Service{UID: "x", Ports: []lbgw.Port{{Protocol: "TCP", Port: 443, NodePort: 30443}}, Annotations: map[string]string{"proxcloud.io/lb-tls-ports": "443", "proxcloud.io/lb-backend-protocol": "tcp"}}
	if one := lbClusterServiceSpec(pve.LBSpec{}, w, "203.0.113.10", nil); one.Listeners[0].Protocol != "" || one.Pools[0].Protocol != "tcp" {
		t.Errorf("a Service without certificates: %+v", one)
	}
}
