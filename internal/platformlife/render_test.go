package platformlife

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/deploy"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The shared layout of design §8.3 on three PVE nodes; dbaas spans all three, so PostgreSQL offers 3 replicas.
func renderInput() RenderInput {
	group := func(name string, count int, nodes ...string) RenderNodeGroup {
		return RenderNodeGroup{Name: name, Nodes: nodes, Count: count, Cores: 2, MemoryMiB: 4096, DiskGiB: 30}
	}
	return RenderInput{
		ID: "pi-0123abcd", Name: "Lab", Region: "az1", RegionName: "Zone 1",
		PVEAPIURL: "https://192.0.2.11:8006", BootStorage: "local-zfs",
		Network:     RenderNetwork{Bridge: "vmbr1", Gateway: "192.0.2.1", Prefix: 24, DNS: []string{"192.0.2.53"}},
		SeedAddress: "192.0.2.5", SSHKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample ops@example"},
		Cluster: RenderCluster{
			Endpoint: "192.0.2.10", NodeRange: []string{"192.0.2.20-192.0.2.39"}, MetalLBRange: "192.0.2.240-192.0.2.250",
			ControlPlane: RenderMachine{Node: "pve1", Cores: 2, MemoryMiB: 4096, DiskGiB: 30},
			NodeGroups:   []RenderNodeGroup{group("system", 3, "pve1", "pve2"), group("kamaji", 1, "pve2"), group("dbaas", 3, "pve1", "pve2", "pve3")},
		},
		Templates: map[string]int{"pve1": 9100, "pve2": 9101, "pve3": 9102},
		WorkerMTU: 1450,
	}
}

// Stand-ins for every credential a run holds, each findable by a scan.
var (
	plantCAPI       = PVEToken{ID: "capmox@pve!capi", Secret: "PLANT-capi-secret-0001"}
	plantCSI        = PVEToken{ID: "csi@pve!csi", Secret: "PLANT-csi-secret-0002"}
	plantKubeconfig = "PLANT-provider-kubeconfig-0003"
	plantSeed       = SeedSecrets{
		CACert:         "-----BEGIN CERTIFICATE-----\nPLANT-ca-cert\n-----END CERTIFICATE-----\n",
		CAKey:          "-----BEGIN EC PRIVATE KEY-----\nPLANT-ca-key-0004\n-----END EC PRIVATE KEY-----\n",
		K3sToken:       "PLANT-k3s-token-0005",
		BootstrapToken: "plant7.plant0bootstrap6",
	}
	plantBoot = SeedBoot{
		Generation: 1, Net0: "virtio=BC:24:11:AA:BB:CC,bridge=vmbr1,firewall=1", Secrets: plantSeed,
		Now: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
	}
	plants = map[string]string{
		"capi token": plantCAPI.Secret, "csi token": plantCSI.Secret, "provider kubeconfig": plantKubeconfig,
		"CA key": "PLANT-ca-key-0004", "k3s token": plantSeed.K3sToken, "bootstrap secret": "plant0bootstrap6",
	}
)

type rendered struct {
	cluster       map[string]any
	helmChart     map[string]any
	projects      []map[string]any
	apps          []map[string]any
	pveSecret     map[string]any
	csiSecret     map[string]any
	k8sBody, dbBd map[string]any
	seed          map[string]string // the CD's files
}

func renderAll(t *testing.T, b *BOM, in RenderInput) rendered {
	t.Helper()
	var r rendered
	var err error
	if r.cluster, err = ClusterValues(b, in); err != nil {
		t.Fatal(err)
	}
	if r.helmChart, err = SeedArgoCD(b, in); err != nil {
		t.Fatal(err)
	}
	r.projects = SeedProjects(b, in)
	if r.apps, err = Applications(b, in); err != nil {
		t.Fatal(err)
	}
	r.pveSecret = PVECredentialsSecret(b, in, plantCAPI)
	if r.csiSecret, err = CSIConfigSecret(b, in, plantCSI); err != nil {
		t.Fatal(err)
	}
	r.k8sBody, r.dbBd = ProviderBodies(b, in, plantKubeconfig)
	seed, err := SeedFiles(b, in, plantBoot)
	if err != nil {
		t.Fatal(err)
	}
	r.seed = seed.Files()
	return r
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed; review, then rerun with -update:\n%s", path, got)
	}
}

func yamlDoc(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Every renderer's output for the fixture and the embedded BOM, so a change to a renderer or to the BOM shows up as a
// diff to review. `go test ./internal/platformlife -run TestRenderGolden -update` rewrites them.
func TestRenderGolden(t *testing.T) {
	r := renderAll(t, embedded(t), renderInput())
	golden(t, "render-cluster-values.golden.yaml", yamlDoc(t, r.cluster))
	golden(t, "render-seed-argocd.golden.yaml", yamlDoc(t, map[string]any{"helmChart": r.helmChart, "projects": r.projects, "applications": r.apps}))
	golden(t, "render-secrets.golden.yaml", yamlDoc(t, []any{r.pveSecret, r.csiSecret}))
	golden(t, "render-providers.golden.yaml", yamlDoc(t, map[string]any{"kubernetes": r.k8sBody, "database": r.dbBd}))
	for name, content := range r.seed {
		golden(t, filepath.Join("seed-cd", name+".golden"), []byte(content))
	}
	manifest, err := BootstrapManifest(plantSeed.BootstrapToken, plantBoot.Now.Add(SeedBootstrapTTL))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "seed-bootstrap-manifest.golden.json", []byte(manifest))
}

// Each credential appears only where design §5 puts it: the CAPI token in the seed's pc-platform-pve, the CSI token in
// the workload's proxmox-csi-plugin, the provider kubeconfig in the provider secret, and the CA key, k3s token and
// bootstrap secret in the seed's user-data. No Application, project, chart values, HelmChart, provider config or other
// CD file carries any of them.
func TestNoSecretOutsideItsPlace(t *testing.T) {
	r := renderAll(t, embedded(t), renderInput())
	scan := func(what string, v any, allowed ...string) {
		t.Helper()
		text, ok := v.(string)
		if !ok {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			text = string(raw)
		}
		for name, plant := range plants {
			if has := strings.Contains(text, plant); has != slices.Contains(allowed, name) {
				t.Errorf("%s: %s present=%v, want %v", what, name, has, !has)
			}
		}
	}
	scan("cluster values", r.cluster)
	scan("seed ArgoCD HelmChart", r.helmChart)
	scan("seed AppProjects and repositories", r.projects)
	for _, app := range r.apps {
		scan("Application "+app["metadata"].(map[string]any)["name"].(string), app)
	}
	scan("pc-platform-pve", r.pveSecret, "capi token")
	scan("proxmox-csi-plugin", r.csiSecret, "csi token")
	for _, body := range []map[string]any{r.k8sBody, r.dbBd} {
		scan(body["name"].(string)+" config", body["config"])
		scan(body["name"].(string)+" secret", body["secret"], "provider kubeconfig")
	}
	for name, content := range r.seed {
		if name == "user-data" {
			scan("seed "+name, content, "CA key", "k3s token", "bootstrap secret")
		} else {
			scan("seed "+name, content)
		}
	}
}

// The integration contract of the proxcloud-platform chart (PR 4 review) and of ArgoCD itself.
func TestApplicationsFollowTheContract(t *testing.T) {
	b, in := embedded(t), renderInput()
	r := renderAll(t, b, in)
	project := r.projects[0]["spec"].(map[string]any)
	if r.projects[0]["metadata"].(map[string]any)["name"] != argoProject {
		t.Fatalf("first project is %v", r.projects[0]["metadata"])
	}
	want := 0
	for _, c := range b.Components {
		if c.Install == InstallApplication {
			want++
		}
	}
	if len(r.apps) != want {
		t.Fatalf("%d Applications for %d application components", len(r.apps), want)
	}
	names := map[string]bool{}
	metadataOwners := map[string]int{}
	for _, app := range r.apps {
		meta := app["metadata"].(map[string]any)
		name := meta["name"].(string)
		spec := app["spec"].(map[string]any)
		src := spec["source"].(map[string]any)
		dst := spec["destination"].(map[string]any)
		sync := spec["syncPolicy"].(map[string]any)
		c := b.Components[slices.IndexFunc(b.Components, func(c Component) bool {
			return c.Name == meta["labels"].(map[string]any)["proxcloud.io/component"] && destination(c.Cluster, in) == dst["server"]
		})]
		if names[name] {
			t.Errorf("Application %s twice", name)
		}
		names[name] = true
		if spec["project"] != argoProject || meta["finalizers"] != nil {
			t.Errorf("%s: project %v, finalizers %v", name, spec["project"], meta["finalizers"])
		}
		if !slices.Contains(project["sourceRepos"].([]any), src["repoURL"]) {
			t.Errorf("%s: repo %v is not in the project's sourceRepos", name, src["repoURL"])
		}
		if !permitted(project["destinations"].([]any), dst["server"].(string), dst["namespace"].(string)) {
			t.Errorf("%s: destination %v is not in the project's destinations", name, dst)
		}
		if src["targetRevision"] != c.Chart.Version || dst["namespace"] != c.Namespace {
			t.Errorf("%s: %v in %v, want the BOM's %s in %s", name, src["targetRevision"], dst["namespace"], c.Chart.Version, c.Namespace)
		}
		if p := sync["automated"].(map[string]any)["prune"]; p != !c.Stateful {
			t.Errorf("%s: prune %v on a component with stateful %v", name, p, c.Stateful)
		}
		if !slices.Contains(sync["syncOptions"].([]any), any("CreateNamespace=true")) || sync["retry"] == nil {
			t.Errorf("%s: needs CreateNamespace=true and a retry policy: %v", name, sync)
		}
		if sync["managedNamespaceMetadata"] != nil {
			metadataOwners[dst["server"].(string)+" "+c.Namespace]++
		}
		if c.Cluster == ClusterWorkload && dst["server"] != WorkloadServer(in.Cluster.Endpoint) {
			t.Errorf("%s: workload component sent to %v", name, dst["server"])
		}
		if c.Name == "capi-tenant" {
			// capi-tenant syncs into proxcloud-system before platform (wave 3) renders the Namespace.
			if pi := slices.IndexFunc(b.Components, func(c Component) bool { return c.Cluster == ClusterWorkload && c.Name == "platform" }); b.Components[pi].Wave <= c.Wave {
				t.Errorf("platform (wave %d) no longer follows capi-tenant (wave %d): recheck CreateNamespace", b.Components[pi].Wave, c.Wave)
			}
		}
	}
	for ns, n := range metadataOwners {
		if n > 1 {
			t.Errorf("%d Applications own managedNamespaceMetadata of %s", n, ns)
		}
	}
	if metadataOwners[WorkloadServer(in.Cluster.Endpoint)+" proxcloud-system"] != 0 {
		t.Error("proxcloud-system is the platform chart's Namespace: no Application may manage its metadata")
	}
	locked := r.projects[1]
	if locked["metadata"].(map[string]any)["name"] != "default" || !reflect.DeepEqual(locked["spec"],
		map[string]any{"sourceRepos": []any{}, "destinations": []any{}, "clusterResourceWhitelist": []any{}}) {
		t.Errorf("seed default project not locked: %v", locked)
	}
}

// The values the run passes to the proxcloud-platform chart are its examples, which chart_test.go renders: the examples
// stand in for the BOM's clusterctl digest and the site's MetalLB range only.
func TestPlatformChartValuesAreItsExamples(t *testing.T) {
	b, in := embedded(t), renderInput()
	b.Images.Clusterctl = "ghcr.io/mutex-solutions/proxcloud-clusterctl:v1.14.4-1@sha256:" + strings.Repeat("0", 64)
	in.Cluster.MetalLBRange = "192.0.2.240-192.0.2.250"
	apps, err := Applications(b, in)
	if err != nil {
		t.Fatal(err)
	}
	normal := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for app, example := range map[string]string{"capi": "capi", ClusterName + "-capi-tenant": "capi-tenant", ClusterName + "-platform": "platform"} {
		i := slices.IndexFunc(apps, func(a map[string]any) bool { return a["metadata"].(map[string]any)["name"] == app })
		got := normal(apps[i]["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"]).(map[string]any)
		if p, ok := got["platform"].(map[string]any); ok {
			// Explicit in the run (the BOM's tenant repository), left to the chart default in the example.
			if p["chartRepository"] != b.TenantCharts.Kubernetes.Repo {
				t.Errorf("chartRepository %v", p["chartRepository"])
			}
			delete(p, "chartRepository")
		}
		raw, err := os.ReadFile(filepath.Join(platformChart, "examples", "values-"+example+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := yaml.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, normal(want)) {
			t.Errorf("%s: values differ from examples/values-%s.yaml\n got: %v\nwant: %v", app, example, got, want)
		}
	}
}

// The workload cluster values render through proxmox-kamaji-cluster in kubeadm mode: one MachineDeployment per group
// and node with the count split as the chart splits it, the group label on every machine, the group taint on every
// group but system, the control plane on its node behind the VIP.
func TestClusterValuesRenderThroughTheChart(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	values, err := ClusterValues(embedded(t), renderInput())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, yamlDoc(t, values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", ClusterName, "../../deploy/charts/proxmox-kamaji-cluster", "-n", ClusterName, "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	replicas := map[string]int{}
	taints := map[string]any{}
	var kcp, endpoint any
	for _, o := range yamlObjects(t, out) {
		spec, _ := o["spec"].(map[string]any)
		switch o["kind"] {
		case "MachineDeployment":
			labels := spec["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
			replicas[labels["proxcloud.io/node-group"].(string)+"/"+o["metadata"].(map[string]any)["name"].(string)] = spec["replicas"].(int)
			if labels[GroupLabel] != labels["proxcloud.io/node-group"] {
				t.Errorf("machine labels %v", labels)
			}
		case "KubeadmConfigTemplate":
			reg := spec["template"].(map[string]any)["spec"].(map[string]any)["joinConfiguration"].(map[string]any)["nodeRegistration"].(map[string]any)
			taints[o["metadata"].(map[string]any)["name"].(string)] = reg["taints"]
		case "KubeadmControlPlane":
			kcp = spec["replicas"]
		case "ProxmoxCluster":
			endpoint = spec["controlPlaneEndpoint"].(map[string]any)["host"]
		}
	}
	if want := map[string]int{
		"system/pc-platform-system-pve1": 2, "system/pc-platform-system-pve2": 1, "kamaji/pc-platform-kamaji-pve2": 1,
		"dbaas/pc-platform-dbaas-pve1": 1, "dbaas/pc-platform-dbaas-pve2": 1, "dbaas/pc-platform-dbaas-pve3": 1,
	}; !reflect.DeepEqual(replicas, want) {
		t.Errorf("MachineDeployments %v, want %v", replicas, want)
	}
	for name, ts := range taints {
		group := strings.Split(strings.TrimPrefix(name, ClusterName+"-"), "-")[0]
		if (ts == nil) != (group == GroupSystem) {
			t.Errorf("%s: taints %v", name, ts)
		}
	}
	if kcp != 1 || endpoint != "192.0.2.10" {
		t.Errorf("control plane replicas %v, endpoint %v", kcp, endpoint)
	}
}

// permitted is ArgoCD's destination check of an AppProject for one object: the server exact, the namespace a glob.
func permitted(destinations []any, server, namespace string) bool {
	return slices.ContainsFunc(destinations, func(d any) bool {
		m := d.(map[string]any)
		ok, _ := path.Match(m["namespace"].(string), namespace)
		return m["server"] == server && ok
	})
}

// podSpec is the pod spec an object creates pods from, or nil.
func podSpec(o map[string]any) map[string]any {
	spec, _ := o["spec"].(map[string]any)
	switch o["kind"] {
	case "Pod":
		return spec
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
	case "CronJob":
		spec, _ = spec["jobTemplate"].(map[string]any)["spec"].(map[string]any)
	default:
		return nil
	}
	tmpl, _ := spec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	return pod
}

// Every Application's chart rendered with its values, as ArgoCD renders it: each namespaced object lands in a namespace
// the project permits on that server (ArgoCD checks every object, not only the destination), and every pod of a
// component on a tainted group is placed there by its own spec. The namespace annotations are only a backstop:
// PodNodeSelector acts when a pod is created, and another Application of the same wave may create the namespace first,
// without them (kamaji-etcd next to kamaji-crds in kamaji-system). Our charts render from
// deploy/charts; the upstream ones only from the Helm repository cache, as unit tests are offline, so an uncached chart
// is skipped: `helm pull <repo>/<chart> --version <v> -d "$(helm env HELM_REPOSITORY_CACHE)"` caches one.
func TestApplicationsRenderInsideTheProject(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	cache, err := exec.Command(helm, "env", "HELM_REPOSITORY_CACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	b, in := embedded(t), renderInput()
	b.Images.Clusterctl = "ghcr.io/mutex-solutions/proxcloud-clusterctl:v1.14.4-1@sha256:" + strings.Repeat("0", 64) // the chart refuses `pending`
	apps, err := Applications(b, in)
	if err != nil {
		t.Fatal(err)
	}
	destinations := SeedProjects(b, in)[0]["spec"].(map[string]any)["destinations"].([]any)
	var comps []Component // apps[i] is comps[i]
	for _, c := range b.Components {
		if c.Install == InstallApplication {
			comps = append(comps, c)
		}
	}
	for i, app := range apps {
		c, name := comps[i], app["metadata"].(map[string]any)["name"].(string)
		chart := filepath.Join("../../deploy/charts", c.Chart.Name)
		if c.Chart.Repo != "ghcr.io/mutex-solutions/charts" {
			chart = filepath.Join(strings.TrimSpace(string(cache)), c.Chart.Name+"-"+c.Chart.Version+".tgz")
			if _, err := os.Stat(chart); err != nil {
				t.Logf("%s: %s not in the Helm cache, not rendered", name, filepath.Base(chart))
				continue
			}
		}
		spec := app["spec"].(map[string]any)
		f := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(f, yamlDoc(t, spec["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"]), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(helm, "template", c.Name, chart, "-n", c.Namespace, "--kube-version", b.Kubernetes.Version, "-f", f)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: helm template: %v\n%s", name, err, stderr.String())
		}
		server := spec["destination"].(map[string]any)["server"].(string)
		tainted := c.Group != "" && c.Group != GroupSystem && c.Group != "all"
		for _, o := range yamlObjects(t, out) {
			meta := o["metadata"].(map[string]any)
			if ns, ok := meta["namespace"].(string); ok && !permitted(destinations, server, ns) {
				t.Errorf("%s: %v %s/%v is outside the project's destinations on %s", name, o["kind"], ns, meta["name"], server)
			}
			if pod := podSpec(o); tainted && pod != nil {
				sel, _ := pod["nodeSelector"].(map[string]any)
				tols, _ := pod["tolerations"].([]any)
				want := groupPlacement(c.Group).Tolerations[0]
				if sel[GroupLabel] != c.Group || !slices.ContainsFunc(tols, func(t any) bool { return reflect.DeepEqual(t, want) }) {
					t.Errorf("%s: %v %v is not placed on group %s by its values: nodeSelector %v, tolerations %v",
						name, o["kind"], meta["name"], c.Group, sel, tols)
				}
			}
		}
	}
}

func TestClusterValuesRefuseAMissingTemplate(t *testing.T) {
	in := renderInput()
	delete(in.Templates, "pve3")
	if _, err := ClusterValues(embedded(t), in); err == nil || !strings.Contains(err.Error(), "pve3") {
		t.Errorf("want an error naming pve3, got %v", err)
	}
}

// The provider bodies carry exactly the keys stage 60's loop sends, apart from the changes design §4.2 names.
// Where stage 60 sends a Jinja expression (regions, services.*, cluster.versions) the keys below it are not compared.
func TestProviderBodiesCarryStage60Keys(t *testing.T) {
	raw, err := os.ReadFile("../../installer/playbooks/60-register.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(raw, &plays); err != nil {
		t.Fatal(err)
	}
	var loop []any
	for _, play := range plays {
		for _, task := range asList(play["tasks"]) {
			if m, _ := task.(map[string]any); m["name"] == "Register the managed-service providers" {
				loop = asList(m["loop"])
			}
		}
	}
	if len(loop) != 2 {
		t.Fatalf("stage 60's provider loop has %d items, want 2", len(loop))
	}
	k8s, db := ProviderBodies(embedded(t), renderInput(), plantKubeconfig)
	for i, c := range []struct {
		body           map[string]any
		added, removed []string
	}{
		{k8s, []string{"config.namespacePlacement", "config.attachToNewProjects", "config.platformInstallationId"}, nil},
		{db, []string{"config.namespacePlacement", "config.attachToNewProjects", "config.platformInstallationId", "config.database.egressFence"},
			[]string{"config.database.backup", "secret.backupAccessKey", "secret.backupSecretKey"}},
	} {
		stage60 := keyPaths(loop[i])
		var normal any
		raw, _ := json.Marshal(c.body)
		if err := json.Unmarshal(raw, &normal); err != nil {
			t.Fatal(err)
		}
		mine := keyPaths(normal)
		under := func(p string, set []string) bool {
			return slices.ContainsFunc(set, func(q string) bool { return p == q || strings.HasPrefix(p, q+".") })
		}
		var leaves []string // stage 60 keys holding a scalar (a Jinja expression)
		for p := range stage60 {
			if !slices.ContainsFunc(sortedKeys(stage60), func(q string) bool { return strings.HasPrefix(q, p+".") }) {
				leaves = append(leaves, p)
			}
		}
		want := map[string]bool{}
		for p := range stage60 {
			if !under(p, c.removed) {
				want[p] = true
			}
		}
		for _, p := range c.added {
			want[p] = true
		}
		got := map[string]bool{}
		for p := range mine {
			if !slices.ContainsFunc(append(leaves, c.added...), func(q string) bool { return strings.HasPrefix(p, q+".") }) {
				got[p] = true
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: keys\n got %v\nwant %v", c.body["name"], sortedKeys(got), sortedKeys(want))
		}
	}
}

func asList(v any) []any { l, _ := v.([]any); return l }

func keyPaths(v any) map[string]bool {
	out := map[string]bool{}
	var walk func(v any, prefix string)
	walk = func(v any, prefix string) {
		m, _ := v.(map[string]any)
		for k, sub := range m {
			out[prefix+k] = true
			walk(sub, prefix+k+".")
		}
	}
	walk(v, "")
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The providers read the bodies as stage 10 sends them (a JSON document): both validate, carry their placement, and the
// database provider has its fence and no backup store.
func TestProviderBodiesAreReadByTheProviders(t *testing.T) {
	b, in := embedded(t), renderInput()
	decode := func(body map[string]any) *externalservice.ExternalService {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		var es externalservice.ExternalService
		if err := json.Unmarshal(raw, &es); err != nil {
			t.Fatal(err)
		}
		return &es
	}
	for _, threeNodes := range []bool{true, false} {
		if !threeNodes {
			in.Cluster.NodeGroups[2].Nodes = []string{"pve1", "pve2"}
		}
		k8sBody, dbBody := ProviderBodies(b, in, plantKubeconfig)
		k8s, db := decode(k8sBody), decode(dbBody)
		kc := k8s.KamajiConfig()
		if !k8s.IsKamaji() || kc.Validate() != nil || kc.ChartVersion != b.TenantCharts.Kubernetes.Version || len(kc.Defaults.Versions) != 0 ||
			kc.NamespacePlacement.NodeSelector[GroupLabel] != "kamaji" || len(kc.NamespacePlacement.Tolerations) != 1 || k8s.AttachToNewProjects() {
			t.Errorf("kubernetes provider: %+v (validate %v)", kc, kc.Validate())
		}
		dc := db.DbaasConfig()
		want := []int{1}
		if threeNodes {
			want = []int{1, 3}
		}
		if !db.IsDbaas() || dc.Validate() != nil || !dc.EgressFence || dc.Backup.Enabled() || dc.NamespacePlacement.NodeSelector[GroupLabel] != "dbaas" ||
			!reflect.DeepEqual(dc.Engines["postgresql"].Replicas, want) || db.AttachToNewProjects() || dc.ChartVersion != b.TenantCharts.Database.Version {
			t.Errorf("database provider (%d dbaas nodes): %+v (validate %v)", len(in.Cluster.NodeGroups[2].Nodes), dc, dc.Validate())
		}
	}
}

// The workload ArgoCD carries the health checks of both providers' kinds, which installer stage 50 patches into argocd-cm.
func TestWorkloadArgoCDCarriesTheTenantHealthChecks(t *testing.T) {
	apps, err := Applications(embedded(t), renderInput())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(apps, func(a map[string]any) bool { return a["metadata"].(map[string]any)["name"] == ClusterName+"-argocd" })
	cm := apps[i]["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"].(map[string]any)["configs"].(map[string]any)["cm"].(map[string]any)
	for _, raw := range [][]byte{deploy.MgmtArgoCDHealth, deploy.DbaasArgoCDHealth} {
		var f struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal(raw, &f); err != nil || len(f.Data) == 0 {
			t.Fatalf("argocd-health.yaml: %v", err)
		}
		for k, v := range f.Data {
			if cm[k] != v {
				t.Errorf("%s missing from the workload ArgoCD's argocd-cm", k)
			}
		}
	}
}

func TestMergeValuesRefusesAConflict(t *testing.T) {
	got, err := mergeValues(map[string]any{"image": map[string]any{"tag": "a"}, "x": 1}, map[string]any{"image": map[string]any{"pull": "b"}, "x": 1})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"image": map[string]any{"tag": "a", "pull": "b"}, "x": 1}) {
		t.Errorf("merge: %v %v", got, err)
	}
	if _, err := mergeValues(map[string]any{"image": map[string]any{"tag": "a"}}, map[string]any{"image": map[string]any{"tag": "b"}}); err == nil ||
		!strings.Contains(err.Error(), "image.tag") {
		t.Errorf("conflict: %v", err)
	}
}

func TestNewSeedSecrets(t *testing.T) {
	now := time.Now()
	a, err := NewSeedSecrets("pi-0123abcd", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSeedSecrets("pi-0123abcd", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair([]byte(a.CACert), []byte(a.CAKey)); err != nil {
		t.Fatalf("CA certificate and key do not match: %v", err)
	}
	block, _ := pem.Decode([]byte(a.CACert))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || !ok || pub.Curve != elliptic.P256() ||
		cert.NotAfter.Before(now.AddDate(9, 11, 0)) || !cert.NotBefore.Before(now) || cert.Subject.CommonName != "proxcloud-seed-server-ca@pcplat-pi-0123abcd-g2" {
		t.Errorf("CA: ca %v usage %v key %T notBefore %v notAfter %v cn %q", cert.IsCA, cert.KeyUsage, cert.PublicKey, cert.NotBefore, cert.NotAfter, cert.Subject.CommonName)
	}
	if !bootstrapTokenRE.MatchString(a.BootstrapToken) || len(a.K3sToken) != 26 {
		t.Errorf("tokens %q %q", a.BootstrapToken, a.K3sToken)
	}
	if a.CAKey == b.CAKey || a.K3sToken == b.K3sToken || a.BootstrapToken == b.BootstrapToken {
		t.Error("two generations share a secret")
	}
	if _, err := BootstrapManifest("ABCDEF.0123456789abcdef", now); err == nil {
		t.Error("a bootstrap token outside [a-z0-9] must be refused")
	}
}

// The user-data is a cloud-config cloud-init can read, and the files it writes are what k3s and kubectl read.
func TestSeedUserDataParses(t *testing.T) {
	ud, err := SeedUserData(embedded(t), renderInput(), plantBoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Fatalf("no #cloud-config header: %.40q", ud)
	}
	var cc struct {
		WriteFiles []struct{ Path, Content, Permissions string } `yaml:"write_files"`
		Runcmd     []string                                      `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(ud), &cc); err != nil {
		t.Fatal(err)
	}
	if len(cc.WriteFiles) != 4 || cc.Runcmd[0] != "set -eu" || cc.Runcmd[len(cc.Runcmd)-1] != "touch /etc/cloud/cloud-init.disabled" {
		t.Fatalf("write_files %d, runcmd %q", len(cc.WriteFiles), cc.Runcmd)
	}
	for _, f := range cc.WriteFiles {
		var v any
		if strings.HasSuffix(f.Path, ".yaml") {
			if err := yaml.Unmarshal([]byte(f.Content), &v); err != nil {
				t.Errorf("%s: %v", f.Path, err)
			}
		}
		if strings.HasSuffix(f.Path, ".key") || strings.HasSuffix(f.Path, ".yaml") {
			if f.Permissions != "0600" {
				t.Errorf("%s: mode %s", f.Path, f.Permissions)
			}
		}
	}
}
