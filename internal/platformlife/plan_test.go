package platformlife

import (
	"strings"
	"testing"
)

func TestPlanFor(t *testing.T) {
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := PlanFor(nil)
	if err != nil || len(fresh.Blocking) != 0 || len(fresh.Changes) != len(cur.Components) || fresh.Target.SHA256 != cur.SHA256 {
		t.Fatalf("a new installation installs every component: %+v %v", fresh, err)
	}
	for _, c := range fresh.Changes {
		if c.From != "" || c.To == "" || c.Cluster == "" {
			t.Fatalf("not an install: %+v", c)
		}
	}
	same, _ := PlanFor(&BOMRef{Release: cur.Release, SHA256: cur.SHA256})
	if len(same.Changes) != 0 || len(same.Blocking) != 0 {
		t.Fatalf("installed = current: %+v", same)
	}
	old, _ := PlanFor(&BOMRef{Release: "0.5.0", SHA256: strings.Repeat("0", 64)})
	// No release upgrades between BOMs, so the reason must not name an upgrade path.
	if len(old.Blocking) != 1 || !strings.HasPrefix(old.Blocking[0], CodeBOMUnsupported) || strings.Contains(old.Blocking[0], "upgrade through") {
		t.Fatalf("a BOM this release does not embed: %+v", old)
	}
}

// Diff: a version change needs upgradeFrom, a stateful component is never removed, pins are listed.
func TestDiff(t *testing.T) {
	from, to := embedded(t), embedded(t)
	to.Kubernetes.Version = "1.36.1"
	cilium := comp(t, to, ClusterWorkload, "cilium")
	cilium.Chart.Version = "1.18.17"
	certManager := comp(t, to, ClusterSeed, "cert-manager")
	certManager.Chart.Version, certManager.UpgradeFrom = "v1.20.3", []string{"v1.20.2"}
	comp(t, to, ClusterWorkload, "metallb").Chart.Digest = "sha256:" + strings.Repeat("a", 64)
	var kept []Component
	for _, c := range to.Components {
		if c.Name != "kamaji-etcd" && c.Name != "cnpg" {
			kept = append(kept, c)
		}
	}
	to.Components = kept

	got := map[string]PlanChange{}
	for _, c := range Diff(from, to) {
		got[c.Cluster+"/"+c.Name] = c
	}
	if c := got["/kubernetes.version"]; c.From != "1.35.10" || c.To != "1.36.1" || c.Blocking != "" {
		t.Errorf("pin: %+v", c)
	}
	if c := got["workload/cilium"]; c.Blocking == "" {
		t.Errorf("a version not in upgradeFrom must block: %+v", c)
	}
	if c := got["seed/cert-manager"]; c.From != "v1.20.2" || c.Blocking != "" {
		t.Errorf("listed in upgradeFrom: %+v", c)
	}
	if c := got["workload/metallb"]; c.To != "sha256:"+strings.Repeat("a", 64) || c.Blocking != "" {
		t.Errorf("digest only: %+v", c)
	}
	if c := got["workload/kamaji-etcd"]; c.To != "" || c.Blocking == "" {
		t.Errorf("a stateful component removed must block: %+v", c)
	}
	if c := got["workload/cnpg"]; c.To != "" || c.Blocking != "" {
		t.Errorf("a stateless component removed: %+v", c)
	}
	if len(got) != 6 {
		t.Errorf("unexpected changes: %+v", got)
	}
}
