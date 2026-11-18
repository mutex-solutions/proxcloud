package platformlife

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
)

// nodeImage is a node image template of the embedded BOM's Kubernetes version on node, as Template updates leaves it:
// staged after the import, published after Publish.
func nodeImage(t *testing.T, node string, vmid int, published bool) pve.ClusterResource {
	t.Helper()
	cur, _ := Current()
	tag, ok := templateupdates.ParseTag(cur.Kubernetes.NodeImage.Tag)
	if !ok {
		t.Fatalf("the BOM's node image tag %q does not parse", cur.Kubernetes.NodeImage.Tag)
	}
	tags := templateupdates.TagImport + ";" + templateupdates.TagStaged
	if published {
		tags = strings.Join([]string{pve.TagManaged, templateupdates.TagK8sPublished, templateupdates.TagK8sImport,
			templateupdates.TagK8sVersion + strings.ReplaceAll(cur.Kubernetes.Version, ".", "-"), pve.TagTemplateVersion + tag.BuildVersion[:12]}, ";")
	}
	return pve.ClusterResource{Type: "qemu", VMID: vmid, Node: node, Template: 1, Pool: pve.TemplatePool, Name: "pc-ghcr-" + tag.OS + "-k8s", Tags: tags}
}

// The version is offered only when every node of the platform cluster publishes a node image of it; the nodes that do
// not are named.
func TestNodeImageLine(t *testing.T) {
	cur, _ := Current()
	in := validInstallation(t) // machines on pve1, pve2 and pve3
	all := []pve.ClusterResource{nodeImage(t, "pve1", 9101, true), nodeImage(t, "pve2", 9102, true), nodeImage(t, "pve3", 9103, true)}
	if line, missing := NodeImageLine(cur.BOM, in, all); line != "pve1=9101,pve2=9102,pve3=9103" || len(missing) != 0 {
		t.Fatalf("published everywhere: line %q, missing %v", line, missing)
	}
	staged := []pve.ClusterResource{all[0], nodeImage(t, "pve2", 9102, false), all[2]}
	if line, missing := NodeImageLine(cur.BOM, in, staged); line != "" || !reflect.DeepEqual(missing, []string{"pve2"}) {
		t.Fatalf("pve2 staged: line %q, missing %v", line, missing)
	}
	if line, missing := NodeImageLine(cur.BOM, in, nil); line != "" || !reflect.DeepEqual(missing, []string{"pve1", "pve2", "pve3"}) {
		t.Fatalf("nothing imported: line %q, missing %v", line, missing)
	}
	// A node the platform does not use adds its copy to the line and is never required.
	if line, _ := NodeImageLine(cur.BOM, in, append(all, nodeImage(t, "pve4", 9104, true))); line != "pve1=9101,pve2=9102,pve3=9103,pve4=9104" {
		t.Fatalf("with pve4: %q", line)
	}
}

// A version the row names is the operator's: it is neither replaced nor removed, and other versions stay.
func TestOfferVersionKeepsTheOperators(t *testing.T) {
	row := map[string]any{"config": map[string]any{"cluster": map[string]any{"versions": map[string]any{"1.34.2": "pve1=9300", "1.35.10": "pve1=9400"}}}}
	if OfferVersion(row, "1.35.10", "pve1=9101") {
		t.Fatal("overwrote a version the row names")
	}
	if !OfferVersion(row, "1.36.0", "pve1=9500") || OfferVersion(row, "1.37.0", "") {
		t.Fatal("a new version must be added, and none without a line")
	}
	want := map[string]any{"1.34.2": "pve1=9300", "1.35.10": "pve1=9400", "1.36.0": "pve1=9500"}
	if got := dig(row, "config", "cluster", "versions"); !reflect.DeepEqual(got, want) {
		t.Fatalf("versions %v", got)
	}
	bare := map[string]any{}
	if !OfferVersion(bare, "1.35.10", "pve1=9101") || str(bare, "config", "cluster", "versions", "1.35.10") != "pve1=9101" {
		t.Fatalf("a row without versions: %v", bare)
	}
}

// An adopted row gets the run's version only when it names none for it; a created row carries it from the body.
func TestAdoptProviderOffersVersion(t *testing.T) {
	cur, _ := Current()
	v := cur.Kubernetes.Version
	in := RenderInput{ID: inID, Name: "lab", Region: "r1", PVEAPIURL: "https://pve:8006"}
	none, _ := ProviderBodies(cur.BOM, in, "KUBECONFIG")
	if got := dig(none, "config", "cluster", "versions"); !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("no published image: versions %v", got)
	}
	in.NodeImageLine = "pve1=9101"
	body, _ := ProviderBodies(cur.BOM, in, "KUBECONFIG")
	if str(body, "config", "cluster", "versions", v) != "pve1=9101" {
		t.Fatalf("body versions %v", dig(body, "config", "cluster", "versions"))
	}
	stored := func(versions map[string]any) map[string]any {
		return map[string]any{"_id": "svc-1", "name": "lab Kubernetes", "status": "PUBLIC",
			"config": map[string]any{"provider": "kamaji", ProviderMarker: inID, "cluster": map[string]any{"versions": versions}}}
	}
	row, err := AdoptProvider([]map[string]any{stored(map[string]any{"1.34.2": "pve1=9300"})}, body)
	if err != nil || str(row, "config", "cluster", "versions", v) != "pve1=9101" || str(row, "config", "cluster", "versions", "1.34.2") != "pve1=9300" {
		t.Fatalf("adopt added %v, %v", dig(row, "config", "cluster", "versions"), err)
	}
	row, err = AdoptProvider([]map[string]any{stored(map[string]any{v: "pve1=9999"})}, body)
	if err != nil || str(row, "config", "cluster", "versions", v) != "pve1=9999" {
		t.Fatalf("adopt overwrote the operator's line: %v, %v", dig(row, "config", "cluster", "versions"), err)
	}
}

// Stage 10 offers the BOM's Kubernetes version when its node image is published on the cluster's nodes, and says on
// its item which nodes lack it when it is not. A Proxmox that does not list its templates is a warning on the item:
// the providers are registered without the version and the run goes on. A version is offered once: a run that finds
// it in status.kubernetesOffered leaves the provider's versions alone, so one the operator removed stays removed.
func TestRegisterOffersPublishedVersion(t *testing.T) {
	cur, _ := Current()
	v := cur.Kubernetes.Version
	for _, tc := range []struct {
		name                 string
		published, before    bool
		unreadable           bool
		state, line, message string
		recorded             bool
	}{
		{name: "published", published: true, state: "done", line: "pve1=9101,pve2=9102,pve3=9103", message: "is offered", recorded: true},
		{name: "staged", state: "warn", message: "pve1, pve2, pve3"},
		{name: "unreadable", published: true, unreadable: true, state: "warn", message: "did not list its templates"},
		{name: "offered before", published: true, before: true, state: "done", message: "offers a version once", recorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlatform(t, RunApply)
			p.only(stageRegister)
			for i, n := range []string{"pve1", "pve2", "pve3"} {
				p.pve.templates = append(p.pve.templates, nodeImage(t, n, 9101+i, tc.published))
			}
			p.pve.noVMList = tc.unreadable
			if tc.before {
				p.mutate(func(_ *Run, in *Installation, _ *Secrets) { in.Status.KubernetesOffered = []string{v} })
			}
			if end := p.once(); end != "complete" {
				t.Fatalf("end %s %+v", end, p.stage(stageRegister))
			}
			_, in, _ := p.state.snapshot()
			if len(p.registers) != 2 || in.Status.Providers == nil {
				t.Fatalf("%d providers registered, status %+v", len(p.registers), in.Status.Providers)
			}
			got := str(p.registers[0], "config", "cluster", "versions", v)
			it := item(p.stage(stageRegister), "kubernetes-version")
			if it == nil || it.State != tc.state || !strings.Contains(it.Message, tc.message) || got != tc.line {
				t.Fatalf("offered %q, item %+v", got, it)
			}
			if want := map[bool][]string{true: {v}}[tc.recorded]; !reflect.DeepEqual(in.Status.KubernetesOffered, want) {
				t.Fatalf("status.kubernetesOffered %v, want %v", in.Status.KubernetesOffered, want)
			}
		})
	}
}

// An installation with no machine and no published image names no node: the message still reads as a sentence.
func TestNodesText(t *testing.T) {
	if NodesText(nil) != "any node" || NodesText([]string{"pve1", "pve2"}) != "pve1, pve2" {
		t.Fatalf("%q, %q", NodesText(nil), NodesText([]string{"pve1", "pve2"}))
	}
}

// Stage 10 applies spec.tenants.attach to both providers once: not without a choice, not again when it was applied,
// and again at the next run when a provider could not take it. An attach that fails (failed names the provider) is a
// warning on that provider's item: the other provider is still tried, both stay registered and the run completes.
func TestRegisterAppliesAttachChoice(t *testing.T) {
	type call struct{ service, choice, actor string }
	run := func(t *testing.T, spec, applied string, done bool, failed string) ([]call, string, Stage) {
		t.Helper()
		p := newPlatform(t, RunApply)
		p.only(stageRegister)
		var calls []call
		p.r.Attach = func(_ context.Context, installation, service, choice, actor string) (bool, string, error) {
			if installation != inID {
				t.Errorf("installation %q", installation)
			}
			calls = append(calls, call{service, choice, actor})
			if service == failed {
				return true, "", fmt.Errorf("the database is away")
			}
			return done || (failed == "" && service == "svc-kamaji" && spec == "all" && applied == "none"), "note " + service, nil
		}
		p.mutate(func(r *Run, in *Installation, _ *Secrets) {
			r.RequestedBy, in.Spec.Tenants.Attach, in.Status.TenantsAttach = "admin-1", spec, applied
		})
		if end := p.once(); end != "complete" {
			t.Fatalf("end %s %+v", end, p.stage(stageRegister))
		}
		_, in, _ := p.state.snapshot()
		if in.Status.Providers == nil || in.Status.Providers.Kubernetes != "svc-kamaji" || in.Status.Providers.Database != "svc-dbaas" {
			t.Fatalf("providers not recorded: %+v", in.Status.Providers)
		}
		return calls, in.Status.TenantsAttach, p.stage(stageRegister)
	}
	both := func(choice string) []call {
		return []call{{"svc-kamaji", choice, "admin-1"}, {"svc-dbaas", choice, "admin-1"}}
	}
	for _, tc := range []struct {
		spec, applied string
		done          bool
		failed        string
		calls         []call
		recorded      string
	}{
		{"", "", true, "", nil, ""},                                            // an install from before the choice: nothing is touched
		{AttachNone, "", true, "", nil, ""},                                    // none is what a new provider row already is
		{AttachAll, "", true, "", both(AttachAll), AttachAll},                  // a first install
		{AttachNew, "", true, "", both(AttachNew), AttachNew},                  //
		{AttachAll, AttachAll, true, "", nil, AttachAll},                       // an apply after it: projects detached since stay detached
		{AttachNone, AttachAll, true, "", both(AttachNone), AttachNone},        // the operator took the choice back
		{AttachAll, AttachNew, false, "", both(AttachAll), AttachNew},          // a provider could not take it: tried again next run
		{AttachAll, AttachNone, false, "", both(AttachAll), AttachNone},        // one took it, the other did not: not recorded, both again next run
		{AttachAll, AttachNew, true, "svc-kamaji", both(AttachAll), AttachNew}, // an attach error: a warning, the other provider is still tried
	} {
		t.Run(fmt.Sprintf("%s after %s done=%v failed=%s", tc.spec, tc.applied, tc.done, tc.failed), func(t *testing.T) {
			calls, recorded, stage := run(t, tc.spec, tc.applied, tc.done, tc.failed)
			if !reflect.DeepEqual(calls, tc.calls) || recorded != tc.recorded {
				t.Fatalf("calls %v, recorded %q", calls, recorded)
			}
			if len(tc.calls) == 0 {
				return
			}
			k8s, db := item(stage, "attach:svc-kamaji"), item(stage, "attach:svc-dbaas")
			if k8s == nil || db == nil {
				t.Fatalf("items %+v", stage.Items)
			}
			switch {
			case tc.failed != "":
				if k8s.State != "warn" || !strings.Contains(k8s.Message, "the database is away") || !strings.Contains(k8s.Message, "next apply") || db.State != "done" {
					t.Fatalf("items %+v, %+v", k8s, db)
				}
			case tc.applied == AttachNone: // the partial case: the Kubernetes provider took it
				if k8s.State != "done" || db.State != "warn" {
					t.Fatalf("items %+v, %+v", k8s, db)
				}
			case k8s.Message != "note svc-kamaji" || (k8s.State == "done") != tc.done:
				t.Fatalf("item %+v", k8s)
			}
		})
	}
}

func TestValidateAttachChoice(t *testing.T) {
	for choice, ok := range map[string]bool{"": true, AttachAll: true, AttachNew: true, AttachNone: true, "some": false} {
		in := validInstallation(t)
		in.Spec.Tenants.Attach = choice
		if err := in.Validate(); (err == nil) != ok {
			t.Errorf("attach %q: %v", choice, err)
		}
	}
}
