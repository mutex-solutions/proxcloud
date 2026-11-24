package platformlife

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// installed is an installation named lab whose last apply was of ref (nil: never applied).
func installed(ref *BOMRef) *Installation {
	return &Installation{Name: "lab", Status: Status{BOM: ref}}
}

func TestPlanFor(t *testing.T) {
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := PlanFor(installed(nil), "")
	if err != nil || len(fresh.Blocking) != 0 || len(fresh.Changes) != len(cur.Components) || fresh.Target.SHA256 != cur.SHA256 || fresh.Upgrade != nil {
		t.Fatalf("a new installation installs every component: %+v %v", fresh, err)
	}
	for _, c := range fresh.Changes {
		if c.From != "" || c.To == "" || c.Cluster == "" {
			t.Fatalf("not an install: %+v", c)
		}
	}
	same, _ := PlanFor(installed(&BOMRef{Release: cur.Release, SHA256: cur.SHA256}), "")
	if len(same.Changes) != 0 || len(same.Blocking) != 0 || same.Upgrade != nil {
		t.Fatalf("installed = current: %+v", same)
	}
	old, _ := PlanFor(installed(&BOMRef{Release: "0.5.0", SHA256: strings.Repeat("0", 64)}), "")
	if len(old.Blocking) != 1 || !strings.HasPrefix(old.Blocking[0], CodeBOMUnsupported) || old.Upgrade != nil {
		t.Fatalf("a BOM this release does not embed: %+v", old)
	}
	// Both ways out are named, in the plan and in the 409 of a run, with what a removal needs first.
	for _, msg := range []string{old.Blocking[0], ErrBOMNotHere.Error()} {
		if !strings.Contains(msg, "release that installed the platform, or delete every tenant cluster and database, then remove managed services and install again") {
			t.Errorf("bom_unsupported does not name both ways out: %s", msg)
		}
	}
	// A target is this build's BOM or nothing.
	if _, err := PlanFor(installed(nil), strings.Repeat("2", 64)); !errors.Is(err, ErrBadTarget) {
		t.Errorf("a new installation with another target: %v", err)
	}
}

// A platform v0.8.0 installed, on a build whose own BOM is another one: an apply without a target stays on the
// installed BOM, so nothing changes and nothing blocks it; the plan names this build's BOM as newer and carries the
// plan of the move to it, which this pair does not allow (a development BOM, and every kind of pin differs).
func TestPlanForAPreviousBOM(t *testing.T) {
	old := newerBuild(t)
	cur, _ := Current()
	ref := BOMRef{Release: old.Release, SHA256: old.SHA256}
	p, err := PlanFor(installed(&ref), "")
	if err != nil || len(p.Changes) != 0 || len(p.Blocking) != 0 || p.Target != ref || p.Newer == nil ||
		*p.Newer != (BOMRef{Release: "9.9.9-dev", SHA256: cur.SHA256}) {
		t.Fatalf("plan %+v newer %+v: %v", p, p.Newer, err)
	}
	if u := p.Upgrade; u == nil || u.Allowed || u.Confirm != "" || u.From != ref || u.To != *p.Newer || len(u.Blocking) < 3 {
		t.Fatalf("the move to the newer BOM: %+v", p.Upgrade)
	}
	if same, _ := PlanFor(installed(&BOMRef{Release: cur.Release, SHA256: cur.SHA256}), ""); same.Newer != nil || same.Target.SHA256 != cur.SHA256 {
		t.Fatalf("installed = current names no newer BOM: %+v", same)
	}
	// A build of main between releases is not a previous BOM: 0.8.0-dev stays refused.
	if dev, _ := PlanFor(installed(&BOMRef{Release: "0.8.0-dev", SHA256: strings.Repeat("1", 64)}), ""); len(dev.Blocking) != 1 || dev.Newer != nil {
		t.Fatalf("a -dev BOM: %+v", dev)
	}
	// Only this build's BOM is a target.
	if _, err := PlanFor(installed(&ref), previousSHA256["0.8.1"]); !errors.Is(err, ErrBadTarget) {
		t.Errorf("a previous BOM as target: %v", err)
	}
}

// Every previous BOM against this build's: a pair plans the seed's k3s and a replacement of machines exactly when its
// k3s or Kubernetes pins differ. A previous BOM with this build's pins (a candidate main still equals) is the release
// line alone, record only; one with older pins that planned no roll would move the record and leave the cluster behind.
func TestPreviousBOMsMoveAsTheirPinsDiffer(t *testing.T) {
	cur := embedded(t)
	prev, err := Previous()
	if err != nil || len(prev) != len(previousSHA256) {
		t.Fatalf("previous BOMs: %d of %d, %v", len(prev), len(previousSHA256), err)
	}
	for _, b := range prev {
		changes := Classify(b.BOM, cur)
		if slices.ContainsFunc(changes, func(c PlanChange) bool { return c.Blocking != "" }) {
			continue // refused as a whole: nothing is planned
		}
		k3s := slices.ContainsFunc(changes, func(c PlanChange) bool { return c.Class == ClassSeed })
		if want := b.Seed.K3s.Version != cur.Seed.K3s.Version; k3s != want {
			t.Errorf("%s to %s: a seed k3s row = %v, the k3s pins differ = %v", b.Release, cur.Release, k3s, want)
		}
		if want := b.Kubernetes.Version != cur.Kubernetes.Version || b.Kubernetes.NodeImage != cur.Kubernetes.NodeImage; hasMachines(changes) != want {
			t.Errorf("%s to %s: machines rows = %v, the Kubernetes pins differ = %v", b.Release, cur.Release, hasMachines(changes), want)
		}
		same := *b.BOM
		same.Release = cur.Release
		if reflect.DeepEqual(&same, cur) && (len(changes) != 1 || changes[0].Name != "release" || changes[0].Class != ClassRecordOnly) {
			t.Errorf("%s to %s differ in the release line alone, and the plan is %+v", b.Release, cur.Release, changes)
		}
	}
}

// The answer for each previous BOM by name, now that this build's BOM moves cert-manager v1.20.2 -> v1.21.3 (the test
// above skips refused pairs): a BOM on this build's Kubernetes, k3s and node image plans exactly the rows named here,
// cert-manager as class chart on both clusters (none from 0.10.2-rc.1, 0.10.2, 0.11.0-rc.1, 0.11.0-rc.2, 0.11.0,
// 0.11.1-rc.1, 0.11.1-rc.2, 0.11.1-rc.3 or 0.11.1-rc.4, which pin v1.21.3 already), and the tenant chart to 0.1.19
// record only from every one of them but 0.11.1-rc.1, 0.11.1-rc.2, 0.11.1-rc.3 and 0.11.1-rc.4, which pin 0.1.19
// already and plan the release alone (from 0.1.17 for 0.11.0-rc.1, 0.11.0-rc.2 and 0.11.0, from 0.1.16 for the four
// before them, from 0.1.15 for 0.10.1-rc.1): the run writes it on the Kubernetes provider and touches no cluster.
// pc-platform's own chart stays where 0.11.0 has it, so only 0.10.1-rc.1, which pins 0.1.15 for it, has a
// seed/pc-platform row. Every BOM on Kubernetes 1.35.10 is refused by the pair gate on both cert-manager rows, because
// that pair also moves k3s, Kubernetes and the node image. A newly kept BOM needs its line.
func TestPreviousBOMsMoveCertManager(t *testing.T) {
	cur := embedded(t)
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string][]string{
		"0.10.1-rc.1": {"/release", "/tenantCharts.kubernetes", "seed/cert-manager", "seed/pc-platform", "workload/cert-manager"},
		"0.10.1-rc.2": {"/release", "/tenantCharts.kubernetes", "seed/cert-manager", "workload/cert-manager"},
		"0.10.1":      {"/release", "/tenantCharts.kubernetes", "seed/cert-manager", "workload/cert-manager"},
		"0.10.2-rc.1": {"/release", "/tenantCharts.kubernetes"},
		"0.10.2":      {"/release", "/tenantCharts.kubernetes"},
		"0.11.0-rc.1": {"/release", "/tenantCharts.kubernetes"},
		"0.11.0-rc.2": {"/release", "/tenantCharts.kubernetes"},
		"0.11.0":      {"/release", "/tenantCharts.kubernetes"},
		"0.11.1-rc.1": {"/release"},
		"0.11.1-rc.2": {"/release"},
		"0.11.1-rc.3": {"/release"},
		"0.11.1-rc.4": {"/release"},
	}
	gate := "not supported by this release: a third-party chart moves only in an upgrade that changes no k3s, Kubernetes " +
		"version or node image, and this one also changes kubernetes.version, kubernetes.nodeImage, seed.k3s. First finish an " +
		"upgrade to a release with this one's Kubernetes version (" + cur.Kubernetes.Version + "), k3s (" + cur.Seed.K3s.Version +
		") and node image (" + cur.Kubernetes.NodeImage.Tag + "), with that release's build, then start this one"
	seen := 0
	for _, b := range prev {
		var rows, refused []string
		for _, c := range Classify(b.BOM, cur) {
			row := c.Cluster + "/" + c.Name
			rows = append(rows, row)
			if c.Blocking != "" {
				refused = append(refused, row)
				if c.Name != "cert-manager" || c.Blocking != gate {
					t.Errorf("%s: %s refused with %q, want only cert-manager refused with the pair gate's sentence", b.Release, row, c.Blocking)
				}
			}
			if c.Name == "cert-manager" && (c.From != "v1.20.2" || c.To != "v1.21.3") {
				t.Errorf("%s: %s moves %s to %s", b.Release, row, c.From, c.To)
			}
			if c.Name == "cert-manager" && c.Blocking == "" && c.Class != ClassChart {
				t.Errorf("%s: %s is class %s, want chart", b.Release, row, c.Class)
			}
			if from := b.TenantCharts.Kubernetes.Version; c.Name == "tenantCharts.kubernetes" && (c.Class != ClassRecordOnly ||
				!strings.Contains(c.From, "/proxmox-kamaji-cluster:"+from+"@") || !strings.Contains(c.To, "/proxmox-kamaji-cluster:0.1.19@")) {
				t.Errorf("%s: %s is class %s and moves %s to %s, want record only from %s to 0.1.19", b.Release, row, c.Class, c.From, c.To, from)
			}
		}
		if want, ok := allowed[b.Release]; ok {
			seen++
			if !slices.Equal(rows, want) || len(refused) != 0 {
				t.Errorf("%s to %s plans %q (refused %q), want %q allowed", b.Release, cur.Release, rows, refused, want)
			}
			continue
		}
		if b.Kubernetes.Version != "1.35.10" {
			t.Errorf("%s (Kubernetes %s) has no stated answer: add its rows to allowed", b.Release, b.Kubernetes.Version)
		}
		if !slices.Equal(refused, []string{"seed/cert-manager", "workload/cert-manager"}) {
			t.Errorf("%s to %s refuses %q, want both cert-manager rows (the plan is %q)", b.Release, cur.Release, refused, rows)
		}
	}
	if seen != len(allowed) {
		t.Errorf("%d of the %d BOMs named in allowed are embedded", seen, len(allowed))
	}
}

// nextRelease makes the build under test a release 0.9.0 whose BOM is the 0.8.1 BOM with edit applied, and answers the
// embedded 0.8.1 BOM: the pair an upgrade moves between. It starts from 0.8.1's file, not from the BOM on main, so
// the pair stays the same when main's pins move.
func nextRelease(t *testing.T, edit func(*BOM)) Embedded {
	t.Helper()
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	old, ok := Lookup(previousSHA256["0.8.1"])
	if !ok {
		t.Fatal("this build does not embed the 0.8.1 BOM")
	}
	saved := *cur.BOM
	t.Cleanup(func() { *cur.BOM = saved })
	*cur.BOM = *old.BOM
	cur.Release, cur.Components = "0.9.0", slices.Clone(old.Components)
	if edit != nil {
		edit(cur.BOM)
	}
	return old
}

// ownCharts bumps the product's own charts as a release does: each new version lists the one it replaces.
func ownCharts(b *BOM) {
	for i := range b.Components {
		switch c := &b.Components[i]; c.Chart.Name {
		case "proxcloud-platform":
			c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = "0.1.3", "sha256:"+strings.Repeat("c", 64), []string{"0.1.2"}
		case "proxmox-kamaji-cluster":
			c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = "0.1.14", "sha256:"+strings.Repeat("d", 64), []string{"0.1.13"}
		}
	}
	b.TenantCharts.Kubernetes.Version, b.TenantCharts.Kubernetes.Digest = "0.1.14", "sha256:"+strings.Repeat("d", 64)
}

// newerCertManager moves both cert-manager entries as a release that read the upgrade notes and compared the render
// does: the next minor, listing the installed version in upgradeFrom (synthetic: no BOM pins v1.21.3 yet).
func newerCertManager(b *BOM) {
	for i := range b.Components {
		if c := &b.Components[i]; c.Name == "cert-manager" {
			c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = "v1.21.3", "sha256:"+strings.Repeat("f", 64), []string{"v1.20.2"}
		}
	}
}

// The plan of 0.8.1 to a next release, as the operator reads it before starting: own charts alone are accepted, and
// cert-manager alone; another third-party chart or a node image next to them refuses the whole move, and so does a
// k3s or node image move next to cert-manager's. `go test ./internal/platformlife -run TestUpgradePlanGolden -update`
// rewrites the files.
func TestUpgradePlanGolden(t *testing.T) {
	for _, c := range []struct {
		name    string
		edit    func(*BOM)
		allowed bool
	}{
		{"0.9.0-release-only", nil, true},
		{"0.9.0-own-charts", ownCharts, true},
		{"0.9.0-seed-k3s", newerK3s("v1.35.11+k3s1"), true},
		{"0.9.0-third-party-chart", func(b *BOM) {
			ownCharts(b)
			comp(t, b, ClusterWorkload, "cilium").Chart.Version = "1.18.17"
		}, false},
		{"0.9.0-node-image", func(b *BOM) {
			ownCharts(b)
			b.Kubernetes.NodeImage.Tag, b.Kubernetes.NodeImage.Digest = "ubuntu-2604-k8s-1.35.10-20270101000000-000000000000", "sha256:"+strings.Repeat("e", 64)
		}, false},
		{"0.9.0-cert-manager", newerCertManager, true},
		{"0.9.0-cert-manager-with-k3s", func(b *BOM) { newerK3s("v1.35.11+k3s1")(b); newerCertManager(b) }, false},
		{"0.9.0-cert-manager-with-node-image", func(b *BOM) { // a rebuilt image of the same version, on the chart that can order the roll
			newerK8s(b)
			b.Kubernetes.Version, b.Kubernetes.NodeImage.Tag = "1.35.10", "ubuntu-2604-k8s-1.35.10-20270101000000-000000000000"
			newerCertManager(b)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := nextRelease(t, c.edit)
			ref := refOf(old)
			in := installed(&ref)
			in.Spec.NodeImage.Mode = ImageBOM // a roll takes the BOM's image
			p, err := PlanFor(in, "")
			if err != nil || p.Upgrade == nil || p.Upgrade.Allowed != c.allowed || p.Target != ref || len(p.Changes) != 0 {
				t.Fatalf("plan %+v upgrade %+v: %v", p, p.Upgrade, err)
			}
			if want := "upgrade lab to 0.9.0"; c.allowed != (p.Upgrade.Confirm == want) {
				t.Errorf("confirm %q", p.Upgrade.Confirm)
			}
			p.Upgrade.To.SHA256 = "" // of this build's BOM file, which is not the pair's
			raw, err := json.MarshalIndent(p.Upgrade, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, filepath.Join("upgrade", "0.8.1-to-"+c.name, "plan.golden.json"), append(raw, '\n'))
		})
	}
}

// Every kind of entry this release does not move refuses the upgrade alone, with a reason that names the entry.
func TestClassifyRefuses(t *testing.T) {
	for name, edit := range map[string]func(*BOM){
		"kubernetes.version":         func(b *BOM) { b.Kubernetes.Version = "1.36.1" },
		"kubernetes.nodeImage":       func(b *BOM) { b.Kubernetes.NodeImage.Digest = "sha256:" + strings.Repeat("e", 64) },
		"seed.image":                 func(b *BOM) { b.Seed.Image.Digest = "sha256:" + strings.Repeat("e", 64) },
		"seed.k3s":                   func(b *BOM) { b.Seed.K3s.Binary.SHA256 = strings.Repeat("e", 64) },
		"images.kubeVip":             func(b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip:v0.9.0@sha256:" + strings.Repeat("e", 64) },
		"images.clusterctl":          func(b *BOM) { b.Images.Clusterctl += "0" },
		"capi.seed.infrastructure":   func(b *BOM) { b.CAPI.Seed.Infrastructure = "proxmox:v0.9.3" },
		"capi.workload.controlPlane": func(b *BOM) { b.CAPI.Workload.ControlPlane = "kamaji:v0.23.0" },
		"seed/argocd":                func(b *BOM) { comp(t, b, ClusterSeed, "argocd").Chart.Version = "10.14.1" },
		"workload/cnpg":              func(b *BOM) { comp(t, b, ClusterWorkload, "cnpg").Chart.Digest = "sha256:" + strings.Repeat("e", 64) },
		"workload/kamaji-etcd":       func(b *BOM) { comp(t, b, ClusterWorkload, "kamaji-etcd").Chart.Version = "0.18.2" },
		// upgradeFrom does not let a third-party chart move: only the allow-list does (thirdPartyMove).
		"workload/cilium": func(b *BOM) {
			c := comp(t, b, ClusterWorkload, "cilium")
			c.Chart.Version, c.UpgradeFrom = "1.18.17", []string{"1.18.16"}
		},
		// cert-manager moves under the rules of an own chart: a published version does not change, and the release
		// lists each version it checked.
		"seed/cert-manager": func(b *BOM) {
			comp(t, b, ClusterSeed, "cert-manager").Chart.Digest = "sha256:" + strings.Repeat("e", 64)
		},
		"workload/cert-manager": func(b *BOM) {
			c := comp(t, b, ClusterWorkload, "cert-manager")
			c.Chart.Version, c.Chart.Digest, c.UpgradeFrom = "v1.21.3", "sha256:"+strings.Repeat("f", 64), []string{"v1.20.3"}
		},
		// An own chart at a version the release did not list, moved to another wave, or with other values.
		"seed/pc-platform": func(b *BOM) { comp(t, b, ClusterSeed, ClusterName).Chart.Version = "0.1.14" },
		"workload/capi-tenant": func(b *BOM) {
			comp(t, b, ClusterWorkload, "capi-tenant").Chart.Digest = "sha256:" + strings.Repeat("e", 64)
		},
		"workload/platform": func(b *BOM) { comp(t, b, ClusterWorkload, "platform").Wave = 4 },
		"seed/capi":         func(b *BOM) { comp(t, b, ClusterSeed, "capi").Values = map[string]any{"a": 1} },
		"workload/metallb": func(b *BOM) {
			b.Components = slices.DeleteFunc(b.Components, func(c Component) bool { return c.Name == "metallb" })
		},
		"workload/new": func(b *BOM) {
			b.Components = append(b.Components, Component{Name: "new", Cluster: ClusterWorkload, Wave: 3, Install: InstallApplication, Namespace: "new",
				Chart: Chart{Repo: "ghcr.io/mutex-solutions/charts", Name: "new", Version: "0.1.0", Digest: "sha256:" + strings.Repeat("e", 64)}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			old := nextRelease(t, edit)
			cur, _ := Current()
			var refused []PlanChange
			for _, c := range Classify(old.BOM, cur.BOM) {
				if c.Class == ClassRefused {
					refused = append(refused, c)
				} else if c.Blocking != "" || c.Impact == "" {
					t.Errorf("a change that is not refused blocks, or says nothing of its effect: %+v", c)
				}
			}
			if len(refused) != 1 || strings.TrimPrefix(refused[0].Cluster+"/", "/")+refused[0].Name != name || !strings.HasPrefix(refused[0].Blocking, later) {
				t.Fatalf("refused: %+v", refused)
			}
			ref := refOf(old)
			_, up, err := move(installed(&ref), cur.SHA256)
			if err != nil || up.Allowed || up.Confirm != "" || len(up.Blocking) != 1 || !strings.HasPrefix(up.Blocking[0], name+": "+later) {
				t.Fatalf("move: %+v, %v", up, err)
			}
		})
	}
}

// The allow-list is cert-manager's component name and chart name together: an entry with only one of the two, moved
// as cert-manager may move, is refused as any other third-party chart.
func TestOnlyCertManagerMoves(t *testing.T) {
	for name, rename := range map[string]func(*Component){
		"another component name": func(c *Component) { c.Name = "issuers" },
		"another chart name":     func(c *Component) { c.Chart.Name = "cert-manager-fork" },
	} {
		t.Run(name, func(t *testing.T) {
			old := nextRelease(t, newerCertManager)
			cur, _ := Current()
			from := *old.BOM
			from.Components = slices.Clone(old.Components)
			for _, b := range []*BOM{&from, cur.BOM} {
				for i := range b.Components {
					if c := &b.Components[i]; c.Cluster == ClusterWorkload && c.Name == "cert-manager" {
						rename(c)
					}
				}
			}
			classes := map[string]string{}
			for _, c := range Classify(&from, cur.BOM) {
				classes[c.Cluster+"/"+c.Name] = c.Class + " " + c.Blocking
			}
			if classes["seed/cert-manager"] != ClassChart+" " {
				t.Errorf("seed/cert-manager: %q", classes["seed/cert-manager"])
			}
			renamed := classes["workload/issuers"] + classes["workload/cert-manager"]
			if !strings.HasPrefix(renamed, ClassRefused+" "+later+"a third-party chart is upgraded only by a later release") {
				t.Errorf("the renamed entry: %q", renamed)
			}
		})
	}
}

// move is the decision of StartRun. Without a target an apply stays on the installed BOM; with this build's BOM as
// target it starts the upgrade; once started every apply continues to the target and none goes back; a later release
// may replace the target when both the installed BOM and the unfinished target may move to its BOM.
func TestMove(t *testing.T) {
	old := nextRelease(t, ownCharts)
	cur, _ := Current()
	ref, target := refOf(old), refOf(cur)
	in := installed(&ref)
	if to, up, err := move(in, ""); err != nil || up != nil || to.SHA256 != old.SHA256 {
		t.Fatalf("no target: %+v %v", up, err)
	}
	if to, up, err := move(in, old.SHA256); err != nil || up != nil || to.SHA256 != old.SHA256 {
		t.Fatalf("the installed BOM as target: %+v %v", up, err)
	}
	if _, _, err := move(in, previousSHA256["0.8.0"]); !errors.Is(err, ErrBadTarget) {
		t.Fatalf("another embedded BOM as target: %v", err)
	}
	to, up, err := move(in, cur.SHA256)
	if err != nil || to.SHA256 != cur.SHA256 || !up.Allowed || up.InProgress || up.Confirm != "upgrade lab to 0.9.0" || up.From != ref || up.To != target {
		t.Fatalf("the upgrade: %+v %v", up, err)
	}
	// Started: Retry (no target) goes on to the target without a confirmation, and the way back is refused.
	in.Status.TargetBOM = &target
	if to, up, err := move(in, ""); err != nil || to.SHA256 != cur.SHA256 || !up.Allowed || !up.InProgress || up.Confirm != "" {
		t.Fatalf("Retry of a started upgrade: %+v %v", up, err)
	}
	var refused *UpgradeRefused
	if _, _, err := move(in, old.SHA256); !errors.As(err, &refused) || !strings.Contains(err.Error(), "does not go back") {
		t.Fatalf("back to the installed BOM: %v", err)
	}
	if p, err := PlanFor(in, ""); err != nil || p.Target != target || p.Upgrade == nil || !p.Upgrade.InProgress || p.Replacement != nil || len(p.Blocking) != 0 {
		t.Fatalf("the plan of a started upgrade: %+v %v", p, err)
	}

	// A release 0.9.1 replaces the unfinished target 0.9.0 (here: the embedded 0.8.0 BOM stands for it, being a BOM
	// this build embeds). Its charts must list the versions of both: the installed ones and the unfinished target's.
	mid, _ := Lookup(previousSHA256["0.8.0"])
	midRef := refOf(mid)
	in.Status.TargetBOM = &midRef
	if _, up, err := move(in, cur.SHA256); err != nil || !up.Allowed || up.InProgress || up.Confirm == "" {
		t.Fatalf("a later release over an unfinished target it may replace: %+v %v", up, err)
	}
	// The plan carries both: the unfinished move, which an apply without a target continues, and the move to this
	// build's BOM in its place, with the phrase that starts it.
	if p, err := PlanFor(in, ""); err != nil || p.Target != midRef || p.Upgrade == nil || !p.Upgrade.InProgress || p.Upgrade.To != midRef ||
		p.Replacement == nil || !p.Replacement.Allowed || p.Replacement.InProgress || p.Replacement.To != target || p.Replacement.Confirm != "upgrade lab to 0.9.0" {
		t.Fatalf("the plan over an unfinished target of an earlier build: %+v %v", p, err)
	}
	comp(t, cur.BOM, ClusterWorkload, "cilium").Chart.Version = "1.18.17"
	if _, up, err := move(in, cur.SHA256); err != nil || up.Allowed || len(up.Blocking) != 2 || !strings.HasPrefix(up.Blocking[1], "from the unfinished target 0.8.0: workload/cilium") {
		t.Fatalf("a later release that may not replace the target: %+v %v", up, err)
	}
	// Going on to a target already started is never refused, whatever the classifier says of the pair now.
	in.Status.TargetBOM = &target
	if _, up, err := move(in, ""); err != nil || !up.Allowed || !up.InProgress || len(up.Blocking) != 0 {
		t.Fatalf("Retry of a started upgrade the classifier would refuse now: %+v %v", up, err)
	}
}

// A development BOM is never a target: no later build embeds it (Previous holds releases only), so the platform could
// be applied by that one build alone. The pair on main is such a pair.
func TestADevelopmentBOMIsNoTarget(t *testing.T) {
	old := nextRelease(t, func(b *BOM) { b.Release = "0.9.0-dev" })
	cur, _ := Current()
	ref := refOf(old)
	_, up, err := move(installed(&ref), cur.SHA256)
	if err != nil || up.Allowed || len(up.Blocking) != 1 || !strings.Contains(up.Blocking[0], "development BOM") {
		t.Fatalf("%+v %v", up, err)
	}
}

// A release candidate's BOM is a release BOM, so it is a target: the build after it embeds it as a previous BOM
// (TestEveryReleaseHasItsPreviousBOM), and the candidate is how a lab drills the upgrade before the release.
func TestAReleaseCandidateIsATarget(t *testing.T) {
	old := nextRelease(t, func(b *BOM) { ownCharts(b); b.Release = "0.9.0-rc.1" })
	cur, _ := Current()
	ref := refOf(old)
	_, up, err := move(installed(&ref), cur.SHA256)
	if err != nil || !up.Allowed || len(up.Blocking) != 0 || up.Confirm != "upgrade lab to 0.9.0-rc.1" {
		t.Fatalf("%+v %v", up, err)
	}
}

// A platform installed before the rule existed (0.8.x): the plan of an apply and the plan of the upgrade both list the
// cluster firewall rule the run writes in stage 3 (production upgrade of 2026-11-19: the run wrote it, the plan did
// not name it). With the rule in place, or unreadable, the plan is the BOMs' alone; nothing is listed before an install.
func TestPlanListsTheAPIRuleARunWrites(t *testing.T) {
	old := nextRelease(t, nil)
	ref := refOf(old)
	in := validInstallation(t)
	in.ID, in.Status.BOM = "pi-1", &ref
	f := &preflightPVE{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	portal := &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID)}
	ctx := context.Background()
	plan := func(in *Installation) Plan {
		t.Helper()
		p, err := PlanFor(in, "")
		if err != nil {
			t.Fatal(err)
		}
		p.ListAPIAccess(ctx, in, portal)
		return p
	}

	p := plan(in)
	if len(p.Changes) != 1 || p.Changes[0].Name != "pve-api-rule" || p.Changes[0].Class != ClassSite || p.Changes[0].Stage != StageSeedVM ||
		!strings.Contains(p.Changes[0].To, pve.APIAccessComment(in.ID)) || len(p.Blocking) != 0 {
		t.Fatalf("the apply's plan: %+v", p.Changes)
	}
	if p.Upgrade == nil || !p.Upgrade.Allowed || len(p.Upgrade.Changes) != 2 || p.Upgrade.Changes[1] != p.Changes[0] {
		t.Fatalf("the upgrade's plan: %+v", p.Upgrade)
	}
	p.Upgrade.To.SHA256 = "" // of this build's BOM file, which is not the pair's
	raw, err := json.MarshalIndent(p.Upgrade, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, filepath.Join("upgrade", "0.8.1-to-0.9.0-release-only-no-api-rule", "plan.golden.json"), append(raw, '\n'))

	fresh := *in
	fresh.Status.BOM = nil
	if p := plan(&fresh); slices.ContainsFunc(p.Changes, func(c PlanChange) bool { return c.Name == "pve-api-rule" }) {
		t.Errorf("before the install: %+v", p.Changes)
	}
	f.extra = map[string]string{"/cluster/firewall/rules": "!"}
	if p := plan(in); len(p.Changes) != 0 || len(p.Upgrade.Changes) != 1 {
		t.Errorf("rules that cannot be read: %+v", p)
	}
	src, dest, port := in.APIAccess()
	b, _ := json.Marshal([]pve.FirewallRule{pve.APIAccessRule(in.ID, src, dest, port)})
	f.extra["/cluster/firewall/rules"] = string(b)
	if p := plan(in); len(p.Changes) != 0 || len(p.Upgrade.Changes) != 1 {
		t.Errorf("the rule in place: %+v", p)
	}
	if len(f.writes) != 0 {
		t.Errorf("the plan wrote: %v", f.writes)
	}
}
