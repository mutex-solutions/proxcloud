package kamaji

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const gatewayTestChartPath = "../../../deploy/charts/proxmox-kamaji-cluster"
const gatewayTestUpstreamCommit = "c0836133e24073fcdcdbb1713aba3cc73a89d0b5"

func TestGatewayChartDefaultInstallsNeitherGatewayNorLegacyIngress(t *testing.T) {
	spec := testSpec()
	docs := gatewayChartDocuments(t, renderChart(t, spec))
	gatewayAssertManagementBoundary(t, docs)
	for _, doc := range docs {
		name := digStr(doc, "metadata", "name")
		if name == spec.ID+"-nginxgatewayfabric" || name == spec.ID+"-ingress" || strings.HasPrefix(name, spec.ID+"-gateway-api") {
			t.Errorf("default cluster unexpectedly renders optional routing component %s/%s", doc["kind"], name)
		}
	}
}

func TestGatewayChartEnabledRendersPinnedTenantProxyAndSchemaCarriers(t *testing.T) {
	spec := testSpec()
	spec.Addons = map[string]bool{"nginxGatewayFabric": true}
	docs := gatewayChartDocuments(t, renderChart(t, spec))
	gatewayAssertManagementBoundary(t, docs)
	proxy := gatewayFindChartDocument(t, docs, "HelmChartProxy", spec.ID+"-nginxgatewayfabric")
	for key, want := range map[string]any{
		"repoURL":     "oci://ghcr.io/nginx/charts",
		"chartName":   "nginx-gateway-fabric",
		"version":     "2.8.1",
		"namespace":   "nginx-gateway",
		"releaseName": "nginxgatewayfabric",
	} {
		if got := dig(proxy, "spec", key); got != want {
			t.Errorf("gateway proxy spec.%s = %v, want %v", key, got, want)
		}
	}
	if dig(proxy, "spec", "clusterSelector", "matchLabels", "proxcloud.io/cluster-id") != spec.ID {
		t.Error("gateway proxy must select only its tenant cluster")
	}
	if dig(proxy, "spec", "options", "wait") != true || dig(proxy, "spec", "options", "install", "createNamespace") != true {
		t.Error("gateway proxy must await its release and create the tenant namespace")
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(digStr(proxy, "spec", "valuesTemplate")), &values); err != nil {
		t.Fatalf("gateway valuesTemplate is not JSON: %v", err)
	}
	if dig(values, "nginxGateway", "gatewayClassName") != "nginx" ||
		dig(values, "nginxGateway", "gatewayControllerName") != "gateway.nginx.org/nginx-gateway-controller" {
		t.Errorf("GatewayClass/controller identity drifted: %#v", values["nginxGateway"])
	}
	if dig(values, "nginx", "service", "type") != "LoadBalancer" || dig(values, "nginx", "service", "externalTrafficPolicy") != "Local" {
		t.Errorf("tenant data-plane exposure drifted: %#v", values["nginx"])
	}
	gateways, ok := values["gateways"].([]any)
	if !ok || len(gateways) != 0 {
		t.Errorf("installing a controller must not choose an application Gateway: %#v", values["gateways"])
	}

	provenance := gatewayReadProvenance(t)
	expectedCarriers := make(map[string][]byte, len(provenance.Files))
	for _, entry := range provenance.Files {
		suffix := strings.ReplaceAll(strings.TrimPrefix(strings.TrimSuffix(entry.File, ".yaml"), "gateway.networking.k8s.io_"), "_", "-")
		raw, err := os.ReadFile(filepath.Join(gatewayTestChartPath, "files/gateway-api/v1.6.3", entry.File))
		if err != nil {
			t.Fatal(err)
		}
		expectedCarriers[spec.ID+"-gateway-api-"+suffix] = raw
	}
	carriers := map[string]bool{}
	var tenantDocs []map[string]any
	for _, doc := range docs {
		name := digStr(doc, "metadata", "name")
		if doc["kind"] == "HelmChartProxy" && name == spec.ID+"-ingress" {
			t.Error("opting into Gateway Fabric must not install legacy ingress-nginx")
		}
		if doc["kind"] != "Secret" || !strings.HasPrefix(name, spec.ID+"-gateway-api-") {
			continue
		}
		if carriers[name] {
			t.Errorf("duplicate schema carrier %s", name)
		}
		carriers[name] = true
		if doc["type"] != "addons.cluster.x-k8s.io/resource-set" || dig(doc, "metadata", "annotations", "argocd.argoproj.io/sync-options") != "Prune=false" {
			t.Errorf("schema carrier %s lost its resource-set type or independent lifecycle", name)
		}
		data, ok := doc["stringData"].(map[string]any)
		if !ok || len(data) != 1 {
			t.Fatalf("schema carrier %s must hold one payload, got %#v", name, doc["stringData"])
		}
		payload, ok := data["gateway-api.yaml"].(string)
		if !ok {
			t.Fatalf("schema carrier %s has no gateway-api.yaml string", name)
		}
		if len([]byte(payload)) >= 1<<20 {
			t.Errorf("schema carrier %s exceeds the decoded Secret 1 MiB budget: %d bytes", name, len([]byte(payload)))
		}
		original, ok := expectedCarriers[name]
		if !ok {
			t.Errorf("unexpected schema carrier %s", name)
		} else if strings.TrimRight(payload, "\n") != strings.TrimRight(string(original), "\n") {
			// YAML literal scalars normalize the final line break; every schema byte before it is retained.
			t.Errorf("carrier %s changed its pinned upstream schema", name)
		}
		tenantDocs = append(tenantDocs, gatewayChartDocuments(t, payload)...)
	}
	if len(carriers) != 11 || len(expectedCarriers) != 11 {
		t.Errorf("schema carriers = %d, pinned sources = %d; want exactly 11", len(carriers), len(expectedCarriers))
	}
	for name := range expectedCarriers {
		if !carriers[name] {
			t.Errorf("missing schema carrier %s", name)
		}
	}

	resourceSet := gatewayFindChartDocument(t, docs, "ClusterResourceSet", spec.ID+"-gateway-api")
	if dig(resourceSet, "spec", "strategy") != "Reconcile" ||
		dig(resourceSet, "spec", "clusterSelector", "matchLabels", "proxcloud.io/cluster-id") != spec.ID ||
		dig(resourceSet, "metadata", "annotations", "argocd.argoproj.io/sync-options") != "Prune=false" {
		t.Error("Gateway API resource set lost tenant scope, reconciliation, or independent schema lifecycle")
	}
	refs, ok := dig(resourceSet, "spec", "resources").([]any)
	if !ok || len(refs) != 11 {
		t.Fatalf("Gateway API CRS references = %#v, want 11 carrier Secrets", dig(resourceSet, "spec", "resources"))
	}
	referenced := map[string]bool{}
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("invalid schema resource reference: %#v", raw)
		}
		name, _ := ref["name"].(string)
		if ref["kind"] != "Secret" || !carriers[name] || referenced[name] {
			t.Errorf("schema CRS reference must name exactly one of its carriers: %#v", ref)
		}
		referenced[name] = true
	}
	gatewayAssertStandardResources(t, tenantDocs)
}

func TestGatewayChartLegacyProxySurvivesUnrelatedAddonEdits(t *testing.T) {
	spec := testSpec()
	current := map[string]any{
		"ingress":       map[string]any{"enabled": true, "operatorNote": "legacy raw block must survive"},
		"metricsServer": map[string]any{"enabled": true},
		"certManager":   map[string]any{"enabled": false},
	}
	// This is the 0.1.9 proxy contract, retained by 0.1.10. Never use the new-create
	// builder to request legacy ingress: only already-existing raw values may retain it.
	want := map[string]any{
		"apiVersion": "addons.cluster.x-k8s.io/v1alpha1",
		"kind":       "HelmChartProxy",
		"metadata":   map[string]any{"name": spec.ID + "-ingress"},
		"spec": map[string]any{
			"clusterSelector": map[string]any{"matchLabels": map[string]any{"proxcloud.io/cluster-id": spec.ID}},
			"repoURL":         "https://kubernetes.github.io/ingress-nginx",
			"chartName":       "ingress-nginx",
			"version":         "4.13.3",
			"namespace":       "ingress-nginx",
			"releaseName":     "ingress",
			"options":         map[string]any{"wait": false, "timeout": "10m", "install": map[string]any{"createNamespace": true}},
			"valuesTemplate":  "{}",
		},
	}
	for _, tc := range []struct {
		name  string
		picks map[string]bool
	}{
		{"platform bump retaining raw values", nil},
		{"cert-manager edit", map[string]bool{"certManager": true}},
		{"metrics-server edit", map[string]bool{"metricsServer": false}},
		{"unchanged old-client legacy flag", map[string]bool{"ingress": true, "certManager": true, "metricsServer": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addons := current
			if tc.picks != nil {
				var err error
				addons, err = MergeAddonValues("0.1.10", current, tc.picks)
				if err != nil {
					t.Fatal(err)
				}
			}
			values := gatewayChartRawValues(spec)
			values["addons"] = addons
			out, err := gatewayRenderRawChart(t, spec.ID, values)
			if err != nil {
				t.Fatalf("legacy retained values must render: %v\n%s", err, out)
			}
			docs := gatewayChartDocuments(t, out)
			got := gatewayFindChartDocument(t, docs, "HelmChartProxy", spec.ID+"-ingress")
			if !reflect.DeepEqual(got, want) {
				t.Errorf("legacy proxy drift would replace/uninstall traffic-bearing release:\ngot %#v\nwant %#v", got, want)
			}
			for _, doc := range docs {
				name := digStr(doc, "metadata", "name")
				if name == spec.ID+"-nginxgatewayfabric" || strings.HasPrefix(name, spec.ID+"-gateway-api") {
					t.Errorf("retaining legacy ingress silently installs replacement component %s", name)
				}
			}
		})
	}
}

func TestGatewayChartRejectsLegacyConflictAndUnsupportedKubernetes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		legacy  bool
		message string
	}{
		{"dual routing implementations", "1.35.4", true, "legacy ingress-nginx"},
		{"Kubernetes below supported minimum", "1.31.9", false, "requires Kubernetes >=1.32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			spec.Version = tc.version
			values := gatewayChartRawValues(spec)
			values["addons"] = map[string]any{
				"nginxGatewayFabric": map[string]any{"enabled": true},
				"ingress":            map[string]any{"enabled": tc.legacy},
			}
			out, err := gatewayRenderRawChart(t, spec.ID, values)
			if err == nil || !strings.Contains(out, tc.message) {
				t.Errorf("render must fail on %s, got err=%v\n%s", tc.message, err, out)
			}
		})
	}
}

func TestGatewayChartVendoredSchemaProvenance(t *testing.T) {
	p := gatewayReadProvenance(t)
	if p.Upstream != "https://github.com/kubernetes-sigs/gateway-api" || p.Tag != "v1.6.3" ||
		p.Commit != gatewayTestUpstreamCommit || p.Channel != "standard" || p.License != "Apache-2.0" {
		t.Fatalf("Gateway API provenance identity drifted: %+v", p)
	}
	wantFiles := map[string]bool{"gateway.networking.k8s.io_vap_safeupgrades.yaml": true}
	for _, resource := range []string{"gatewayclasses", "gateways", "grpcroutes", "httproutes", "referencegrants", "backendtlspolicies", "listenersets", "tcproutes", "tlsroutes", "udproutes"} {
		wantFiles["gateway.networking.k8s.io_"+resource+".yaml"] = true
	}
	if len(p.Files) != len(wantFiles) {
		t.Fatalf("provenance lists %d files, want %d", len(p.Files), len(wantFiles))
	}
	seen := map[string]bool{}
	var docs []map[string]any
	for _, entry := range p.Files {
		if !wantFiles[entry.File] || seen[entry.File] {
			t.Fatalf("unexpected or duplicate provenance filename %q", entry.File)
		}
		seen[entry.File] = true
		wantURL := "https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/" + gatewayTestUpstreamCommit + "/config/crd/standard/" + entry.File
		if entry.URL != wantURL {
			t.Errorf("%s source URL is not immutable official Standard metadata: %s", entry.File, entry.URL)
		}
		raw, err := os.ReadFile(filepath.Join(gatewayTestChartPath, "files/gateway-api/v1.6.3", entry.File))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if len(raw) != entry.Bytes || hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Errorf("%s bytes/hash differ from provenance: bytes=%d sha256=%x", entry.File, len(raw), sum)
		}
		docs = append(docs, gatewayChartDocuments(t, string(raw))...)
	}
	entries, err := os.ReadDir(filepath.Join(gatewayTestChartPath, "files/gateway-api/v1.6.3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") && !seen[entry.Name()] {
			t.Errorf("schema %s is rendered without provenance", entry.Name())
		}
	}
	gatewayAssertStandardResources(t, docs)
}

func gatewayChartRawValues(spec ClusterSpec) map[string]any {
	cfg := testCfg()
	cfg.Defaults.Versions[spec.Version] = "pve1=9210"
	return BuildValues(cfg, spec)
}

// Raw values exercise retained legacy installations and template failures, not the new-create API.
func gatewayRenderRawChart(t *testing.T, release string, values map[string]any) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", release, gatewayTestChartPath, "-f", path).CombinedOutput()
	return string(out), err
}

func gatewayChartDocuments(t *testing.T, raw string) []map[string]any {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			return docs
		}
		if err != nil {
			t.Fatalf("decode rendered chart YAML: %v", err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
}

func gatewayFindChartDocument(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	var result map[string]any
	for _, doc := range docs {
		if doc["kind"] == kind && digStr(doc, "metadata", "name") == name {
			if result != nil {
				t.Fatalf("duplicate rendered %s/%s", kind, name)
			}
			result = doc
		}
	}
	if result == nil {
		t.Fatalf("missing rendered %s/%s", kind, name)
	}
	return result
}

func gatewayAssertManagementBoundary(t *testing.T, docs []map[string]any) {
	t.Helper()
	for _, doc := range docs {
		switch doc["kind"] {
		case "CustomResourceDefinition", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "GatewayClass", "Gateway", "HTTPRoute", "GRPCRoute", "TCPRoute", "TLSRoute", "UDPRoute", "ListenerSet", "BackendTLSPolicy", "ReferenceGrant", "Job":
			t.Errorf("tenant schema/application resource escaped its carrier into management YAML: %s/%s", doc["kind"], digStr(doc, "metadata", "name"))
		}
	}
}

func gatewayAssertStandardResources(t *testing.T, docs []map[string]any) {
	t.Helper()
	want := map[string]bool{
		"ValidatingAdmissionPolicy/safe-upgrades.gateway.networking.k8s.io":        true,
		"ValidatingAdmissionPolicyBinding/safe-upgrades.gateway.networking.k8s.io": true,
	}
	for _, resource := range []string{"gatewayclasses", "gateways", "grpcroutes", "httproutes", "referencegrants", "backendtlspolicies", "listenersets", "tcproutes", "tlsroutes", "udproutes"} {
		want["CustomResourceDefinition/"+resource+".gateway.networking.k8s.io"] = true
	}
	if len(docs) != len(want) {
		t.Errorf("tenant prerequisite documents = %d, want 10 CRDs plus policy and binding", len(docs))
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		kind, _ := doc["kind"].(string)
		name := digStr(doc, "metadata", "name")
		identity := kind + "/" + name
		if !want[identity] || seen[identity] {
			t.Errorf("unexpected/duplicate Gateway API Standard resource %s", identity)
		}
		seen[identity] = true
		if dig(doc, "metadata", "annotations", "gateway.networking.k8s.io/bundle-version") != "v1.6.3" ||
			dig(doc, "metadata", "annotations", "gateway.networking.k8s.io/channel") != "standard" {
			t.Errorf("resource %s lost Standard v1.6.3 provenance", identity)
		}
		if kind == "CustomResourceDefinition" {
			if doc["apiVersion"] != "apiextensions.k8s.io/v1" || dig(doc, "spec", "group") != "gateway.networking.k8s.io" {
				t.Errorf("resource %s is not a Gateway API v1 CRD", identity)
			}
		} else if doc["apiVersion"] != "admissionregistration.k8s.io/v1" {
			t.Errorf("resource %s is not a stable v1 admission resource", identity)
		}
	}
	for identity := range want {
		if !seen[identity] {
			t.Errorf("missing Gateway API Standard resource %s", identity)
		}
	}
}

type gatewaySchemaProvenance struct {
	Upstream string `json:"upstream"`
	Tag      string `json:"tag"`
	Commit   string `json:"commit"`
	Channel  string `json:"channel"`
	License  string `json:"license"`
	Files    []struct {
		File   string `json:"file"`
		URL    string `json:"url"`
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func gatewayReadProvenance(t *testing.T) gatewaySchemaProvenance {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(gatewayTestChartPath, "files/gateway-api/provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p gatewaySchemaProvenance
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}
