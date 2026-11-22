package project

import (
	"context"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// Without a certificate store or the portal's key, a balancer with a listener that ends TLS is not pushed: the row
// says why and the appliances keep what they serve. A balancer without one is not asked anything.
func TestLBApplyWithoutCertificateStore(t *testing.T) {
	h := &Handler{}
	plain := &loadBalancer{LBSpec: pve.LBSpec{Listeners: []pve.LBListener{{Port: 80, Protocol: "http", Pool: "web"}}}}
	if files, ok := h.lbApplyCerts(context.Background(), &Project{ID: "p1"}, plain); !ok || files != nil || plain.StatusMessage != "" {
		t.Errorf("a balancer without TLS: %v %q", ok, plain.StatusMessage)
	}
	for name, lb := range map[string]*loadBalancer{
		"never applied": {Status: lbStatusBuild},
		"serving":       {Status: lbStatusActive, AppliedHash: "abc"},
	} {
		lb.LBSpec = pve.LBSpec{Listeners: []pve.LBListener{{Port: 443, Protocol: "https", Pool: "web", Certificates: []string{"c1"}, CertFiles: []string{strings.Repeat("a", 64)}}}}
		files, ok := h.lbApplyCerts(context.Background(), &Project{ID: "p1"}, lb)
		if ok || files != nil || !strings.Contains(lb.StatusMessage, "no encryption key") {
			t.Errorf("%s: %v %q", name, ok, lb.StatusMessage)
		}
		if want := map[string]string{"never applied": lbStatusError, "serving": lbStatusActive}[name]; lb.Status != want {
			t.Errorf("%s: status %s, want %s", name, lb.Status, want)
		}
	}
}
