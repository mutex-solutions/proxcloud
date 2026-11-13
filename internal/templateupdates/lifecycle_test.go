package templateupdates

import (
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func osPayload() VMPayload {
	return VMPayload{Version: 1, Package: "proxcloud-vm-debian-13", Tag: "debian-13-os-20261111000000-aaaaaaaaaaaa", Digest: dg('9'),
		OS: "debian-13", Variant: "os", BuildVersion: "20261111000000", ServiceID: "svc", Node: "pve1"}
}

func k8sPayload() VMPayload {
	return VMPayload{Version: 1, Package: "proxcloud-vm-ubuntu-2604-k8s", Tag: "ubuntu-2604-k8s-1.35.10-20261112010203-cccccccccccc", Digest: dg('8'),
		OS: "ubuntu-2604", Variant: "kubernetes", KubernetesVersion: "1.35.10", BuildVersion: "20261112010203", ServiceID: "svc", Node: "pve1"}
}

// Publish turns a staged import into exactly the tags pve.DiscoverTemplates and build.py read, with
// the catalog tags leading (installer selectors sort by the tag string); idempotent.
func TestPublishedTagsOS(t *testing.T) {
	p := osPayload()
	got := publishedTags(TemplateTags(p), p)
	if !strings.HasPrefix(got, "proxcloud;pc-public;pc-img-debian-13;pc-v-202611110000;") {
		t.Fatalf("lead: %s", got)
	}
	if !sameTags(got, "proxcloud;pc-public;pc-img-debian-13;pc-v-202611110000;pc-src-999999999999;pc-tu-import") {
		t.Fatalf("published tags: %s", got)
	}
	if publishedTags(got, p) != got {
		t.Fatal("publish is not idempotent")
	}
	rows := pve.DiscoverTemplates([]pve.ClusterResource{{Type: "qemu", VMID: 9300, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Tags: got}})
	if len(rows) != 1 || rows[0].Slug != "debian-13" || rows[0].Copies["pve1"].Version != "202611110000" {
		t.Fatalf("discovery: %+v", rows)
	}
}

// A Kubernetes node image is never a tenant catalog entry, staged or published.
func TestPublishedTagsK8sNeverInTenantCatalog(t *testing.T) {
	p := k8sPayload()
	got := publishedTags(TemplateTags(p), p)
	if strings.Contains(got, "pc-public") || strings.Contains(got, "pc-img-") || !hasTagValue(got, TagK8sPublished) || !hasTagValue(got, "pc-v-202611120102") {
		t.Fatalf("k8s published tags: %s", got)
	}
	if rows := pve.DiscoverTemplates([]pve.ClusterResource{{Type: "qemu", VMID: 9300, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Tags: got}}); len(rows) != 0 {
		t.Fatalf("k8s image in the tenant catalog: %+v", rows)
	}
	if !isPublished(got, "kubernetes") || isPublished(got, "os") {
		t.Fatal("isPublished")
	}
}

// Withdraw takes the template out of every catalog and every pc-img- selector, keeps it, and is
// sticky: pc-public put back by hand does not bring it back into DiscoverTemplates.
func TestWithdrawnTags(t *testing.T) {
	p := osPayload()
	pub := publishedTags(TemplateTags(p), p)
	w := withdrawnTags(pub)
	if strings.Contains(w, "pc-public") || strings.Contains(w, "pc-img-") || !hasTagValue(w, pve.TagTemplateWithdrawn) || !hasTagValue(w, TagImport) || !hasTagValue(w, "pc-src-999999999999") {
		t.Fatalf("withdrawn: %s", w)
	}
	if withdrawnTags(w) != w {
		t.Fatal("withdraw is not idempotent")
	}
	readded := w + ";pc-public;pc-img-debian-13"
	if rows := pve.DiscoverTemplates([]pve.ClusterResource{{Type: "qemu", VMID: 9300, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Tags: readded}}); len(rows) != 0 {
		t.Fatalf("a withdrawn template re-entered the catalog: %+v", rows)
	}
	// A legacy import (no marker) gets the marker on withdraw, so build.py never prunes it.
	legacy := "proxcloud;pc-src-999999999999;pc-img-debian-13;pc-v-202611110000;pc-public"
	if lw := withdrawnTags(legacy); !hasTagValue(lw, TagImport) {
		t.Fatalf("legacy withdraw: %s", lw)
	}
}

// Fallback: after the newest version is withdrawn, DiscoverTemplates serves the previous one.
func TestWithdrawFallsBackToPreviousVersion(t *testing.T) {
	p := osPayload()
	newest := publishedTags(TemplateTags(p), p)
	older := "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000"
	rows := []pve.ClusterResource{
		{Type: "qemu", VMID: 9200, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Tags: older},
		{Type: "qemu", VMID: 9300, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Tags: newest, Name: TemplateName(p)},
	}
	if d := pve.DiscoverTemplates(rows); d[0].VMID != 9300 {
		t.Fatalf("before: %+v", d)
	}
	if lineDefault(rows, p, "pve1") != 9300 {
		t.Fatal("lineDefault before")
	}
	rows[1].Tags = withdrawnTags(newest)
	if d := pve.DiscoverTemplates(rows); len(d) != 1 || d[0].VMID != 9200 {
		t.Fatalf("after: %+v", d)
	}
	if lineDefault(rows, p, "pve1") != 9200 {
		t.Fatal("lineDefault after")
	}
}

func TestServesLineAndCopyOf(t *testing.T) {
	p := osPayload()
	row := func(vmid int, pool, tags, name string, tpl int) pve.ClusterResource {
		return pve.ClusterResource{Type: "qemu", VMID: vmid, Node: "pve1", Template: tpl, Pool: pool, Tags: tags, Name: name}
	}
	pub := publishedTags(TemplateTags(p), p)
	cases := []struct {
		r            pve.ClusterResource
		serves, copy bool
	}{
		{row(1, pve.TemplatePool, pub, TemplateName(p), 1), true, true},
		{row(2, pve.TemplatePool, TemplateTags(p), TemplateName(p), 1), false, true},                      // staged
		{row(3, "pc-tenant", pub, TemplateName(p), 1), false, false},                                      // tenant pool
		{row(4, pve.TemplatePool, pub, TemplateName(p), 0), false, false},                                 // not a template (a clone)
		{row(5, pve.TemplatePool, "proxcloud;pc-public;pc-img-debian-13;pc-v-1", "pc-x", 1), true, false}, // pipeline version
		{row(6, pve.TemplatePool, "proxcloud;pc-public;pc-img-debian-12;pc-v-1", "pc-x", 1), false, false},
		{row(7, pve.TemplatePool, withdrawnTags(pub), TemplateName(p), 1), false, true},
	}
	for _, c := range cases {
		if servesLine(c.r, p) != c.serves || copyOf(c.r, p) != c.copy {
			t.Errorf("vm %d: serves=%v copy=%v", c.r.VMID, servesLine(c.r, p), copyOf(c.r, p))
		}
	}
	k := k8sPayload()
	pipe := row(8, pve.TemplatePool, "proxcloud;pc-k8s;pc-k8s-1-35-10;pc-k8s-os-ubuntu-2604", "ubuntu-2604-kube-v1.35.10", 1)
	other := row(9, pve.TemplatePool, "proxcloud;pc-k8s;pc-k8s-1-34-1;pc-k8s-os-ubuntu-2604", "ubuntu-2604-kube-v1.34.1", 1)
	imp := row(10, pve.TemplatePool, publishedTags(TemplateTags(k), k), TemplateName(k), 1)
	if !servesLine(pipe, k) || servesLine(other, k) || !servesLine(imp, k) {
		t.Fatal("k8s line")
	}
	if v := versionsMapValue([]pve.ClusterResource{pipe, other, imp, {Type: "qemu", VMID: 11, Node: "pve2", Template: 1, Pool: pve.TemplatePool, Tags: imp.Tags, Name: imp.Name}}, k); v != "pve1=10,pve2=11" {
		t.Fatalf("versions map value %q", v)
	}
}

func TestMarkWithdrawn(t *testing.T) {
	d := &Discovery{VM: []VMLine{{Line: "a", Latest: &VerifiedVM{Candidate: Candidate{Digest: dg('1')}}, UpdateAvailable: true}, {Line: "b", Latest: &VerifiedVM{Candidate: Candidate{Digest: dg('2')}}, UpdateAvailable: true}}}
	MarkWithdrawn(d, map[string]BlockedDigest{dg('1'): {Digest: dg('1'), Reason: "bad"}})
	if d.VM[0].LatestWithdrawn == nil || d.VM[0].UpdateAvailable || d.VM[1].LatestWithdrawn != nil || !d.VM[1].UpdateAvailable {
		t.Fatalf("%+v", d.VM)
	}
}

func TestEditTagsKeepsOrderNoDuplicates(t *testing.T) {
	if got := editTags("a;b;c;b", []string{"b"}, []string{"d", "a"}); got != "a;c;d" {
		t.Fatalf("got %q", got)
	}
}

// Staged imports are seen as installed versions of their line (by name, no pc-img- tag).
func TestInstalledForStagedImport(t *testing.T) {
	p := osPayload()
	row := &VMLine{OS: "debian-13", Variant: "os"}
	inst, ok := installedFor(row, pve.ClusterResource{Type: "qemu", Template: 1, Pool: pve.TemplatePool, VMID: 9300, Node: "pve1", Name: TemplateName(p), Tags: TemplateTags(p)})
	if !ok || inst.Version != "202611110000" || inst.Published || inst.SourceDigest != "999999999999" {
		t.Fatalf("%+v %v", inst, ok)
	}
	inst, ok = installedFor(row, pve.ClusterResource{Type: "qemu", Template: 1, Pool: pve.TemplatePool, VMID: 9300, Node: "pve1", Name: TemplateName(p), Tags: withdrawnTags(publishedTags(TemplateTags(p), p))})
	if !ok || !inst.Withdrawn || inst.Published {
		t.Fatalf("withdrawn %+v %v", inst, ok)
	}
}

// Hardening: a Kubernetes publish strips pc-public / pc-img-* put on by hand.
func TestPublishedTagsK8sStripsCatalogTags(t *testing.T) {
	p := k8sPayload()
	got := publishedTags(TemplateTags(p)+";pc-public;pc-img-ubuntu-2604", p)
	if strings.Contains(got, "pc-public") || strings.Contains(got, "pc-img-") {
		t.Fatalf("k8s publish kept catalog tags: %s", got)
	}
}

// planWithdraw: per-provider "last version" and per-node "losing the last copy".
func TestPlanWithdraw(t *testing.T) {
	p := osPayload()
	pub := publishedTags(TemplateTags(p), p)
	row := func(vmid int, node, tags, name string) pve.ClusterResource {
		return pve.ClusterResource{Type: "qemu", VMID: vmid, Node: node, Template: 1, Pool: pve.TemplatePool, Tags: tags, Name: name}
	}
	older := "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000"
	// Copies on pve1 and pve2; an older version only on pve1 → pve2 loses its last copy.
	pl := planWithdraw([]pve.ClusterResource{row(1, "pve1", pub, TemplateName(p)), row(2, "pve2", pub, TemplateName(p)), row(3, "pve1", older, "pc-x")}, p)
	if pl.lastOnProvider || len(pl.copies) != 2 || pl.publishedCopies != 2 || len(pl.nodesLosingLast) != 1 || pl.nodesLosingLast[0] != "pve2" {
		t.Fatalf("plan %+v", pl)
	}
	// Only copies → last on the provider (node loss not reported separately).
	pl = planWithdraw([]pve.ClusterResource{row(1, "pve1", pub, TemplateName(p))}, p)
	if !pl.lastOnProvider || len(pl.nodesLosingLast) != 0 {
		t.Fatalf("plan %+v", pl)
	}
	// Staged copy only: nothing published is lost.
	pl = planWithdraw([]pve.ClusterResource{row(1, "pve1", TemplateTags(p), TemplateName(p))}, p)
	if pl.lastOnProvider || pl.publishedCopies != 0 || len(pl.copies) != 1 {
		t.Fatalf("plan %+v", pl)
	}
}
