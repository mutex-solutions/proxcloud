package platformlife

// plan.go — GET …/plan (design §7): a read-only diff of the installed BOM against the one this release embeds, with the
// reasons an upgrade would be refused. The live reason (an Application not Healthy) comes with the live read.

import (
	"fmt"
	"slices"
)

// PlanChange is one difference: a component (Cluster and Name) or a top-level pin (Name only). From "" is an install,
// To "" a removal.
type PlanChange struct {
	Cluster  string `json:"cluster,omitempty"`
	Name     string `json:"name"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Blocking string `json:"blocking,omitempty"`
}

// Plan is the answer of GET …/plan.
type Plan struct {
	Installed *BOMRef      `json:"installed"` // nil: nothing installed yet
	Target    BOMRef       `json:"target"`
	Changes   []PlanChange `json:"changes"`
	Blocking  []string     `json:"blocking"`
}

// embeddedBOM finds an embedded BOM by sha256. Only the current one is embedded today.
// ponytail: one BOM; the previous ones of this and the last minor (deploy/platform/previous/) join here with the first
// release that changes the BOM.
func embeddedBOM(sha string) (*BOM, bool) {
	cur, err := Current()
	if err != nil || cur.SHA256 != sha {
		return nil, false
	}
	return cur.BOM, true
}

// PlanFor diffs the installed BOM (nil for a new installation) against the current one. An installed BOM this build
// does not embed blocks with bom_unsupported: a release embeds one BOM and none upgrades between BOMs yet.
func PlanFor(installed *BOMRef) (Plan, error) {
	cur, err := Current()
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Installed: installed, Target: BOMRef{Release: cur.Release, SHA256: cur.SHA256}, Changes: []PlanChange{}, Blocking: []string{}}
	var from *BOM
	if installed != nil {
		var ok bool
		if from, ok = embeddedBOM(installed.SHA256); !ok {
			p.Blocking = append(p.Blocking, fmt.Sprintf("%s: the installed BOM %s is not the one this release embeds, and no release upgrades between BOMs yet; run the ProxCloud release that installed the platform", CodeBOMUnsupported, installed.Release))
			return p, nil
		}
	}
	p.Changes = Diff(from, cur.BOM)
	for _, c := range p.Changes {
		if c.Blocking != "" {
			p.Blocking = append(p.Blocking, c.Cluster+"/"+c.Name+": "+c.Blocking)
		}
	}
	return p, nil
}

// Diff lists what moving from from to to changes. With from nil every component is an install. A version change is
// blocking unless to's entry lists the old version in upgradeFrom; a stateful component is never removed by a run.
func Diff(from, to *BOM) []PlanChange {
	out := []PlanChange{}
	if from != nil {
		pins := func(b *BOM) [][2]string {
			return [][2]string{
				{"kubernetes.version", b.Kubernetes.Version}, {"kubernetes.nodeImage", b.Kubernetes.NodeImage.Digest},
				{"seed.image", b.Seed.Image.Digest}, {"seed.k3s", b.Seed.K3s.Version},
				{"images.kubeVip", b.Images.KubeVip}, {"images.clusterctl", b.Images.Clusterctl},
				{"tenantCharts.kubernetes", b.TenantCharts.Kubernetes.Version + "@" + b.TenantCharts.Kubernetes.Digest},
				{"tenantCharts.database", b.TenantCharts.Database.Version + "@" + b.TenantCharts.Database.Digest},
			}
		}
		for i, f := range pins(from) {
			if t := pins(to)[i]; f[1] != t[1] {
				out = append(out, PlanChange{Name: f[0], From: f[1], To: t[1]})
			}
		}
	}
	find := func(b *BOM, cluster, name string) *Component {
		if b == nil {
			return nil
		}
		for i := range b.Components {
			if c := &b.Components[i]; c.Cluster == cluster && c.Name == name {
				return c
			}
		}
		return nil
	}
	for _, t := range to.Components {
		f := find(from, t.Cluster, t.Name)
		switch {
		case f == nil:
			out = append(out, PlanChange{Cluster: t.Cluster, Name: t.Name, To: t.Chart.Version})
		case f.Chart.Version != t.Chart.Version:
			c := PlanChange{Cluster: t.Cluster, Name: t.Name, From: f.Chart.Version, To: t.Chart.Version}
			if !slices.Contains(t.UpgradeFrom, f.Chart.Version) {
				c.Blocking = fmt.Sprintf("version %s is not in upgradeFrom of %s", f.Chart.Version, t.Chart.Version)
			}
			out = append(out, c)
		case f.Chart.Digest != t.Chart.Digest:
			out = append(out, PlanChange{Cluster: t.Cluster, Name: t.Name, From: f.Chart.Digest, To: t.Chart.Digest})
		}
	}
	if from != nil {
		for _, f := range from.Components {
			if find(to, f.Cluster, f.Name) != nil {
				continue
			}
			c := PlanChange{Cluster: f.Cluster, Name: f.Name, From: f.Chart.Version}
			if f.Stateful {
				c.Blocking = "a stateful component is never removed by a run"
			}
			out = append(out, c)
		}
	}
	return out
}
