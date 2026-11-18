package kamaji

import (
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The platform's own workload cluster (managed services, docs/adr/0009) in the shape the portal will render: a shared
// cluster with node groups, in the platform pool. kubeVipImage is left to the chart default on purpose (see below). The
// CSI gateway and Gateway Fabric keys are set to show that kubeadm mode renders neither.
const kubeadmTestValues = `
kubernetesVersion: "1.35.10"
cloudCredentialsSecretName: pc-platform-pve
clusterNetwork: {pods: 10.244.0.0/16, services: 10.96.0.0/16}
controlPlane:
  kind: kubeadm
  kubeadm:
    endpoint: 192.0.2.10
    certSANs: [platform.example.com]
    machineImageId: "pve1=9100"
    machineCores: 2
    machineMemoryMiB: 4096
    machineRootVolume: {diskSize: 30}
    machineTags: [proxcloud, pc-platform, pc-platform-pi-0123abcd]
proxmox:
  region: az1
  pool: proxcloud-system
  storage: local-zfs
  network:
    bridge: vmbr0
    gateway: 192.0.2.1
    prefix: 24
    addresses: ["192.0.2.20-192.0.2.39"]
    dnsServers: ["192.0.2.53"]
  csi:
    gateway: {secretName: pc-platform-csi, kubeconfigSecretName: pc-platform-admin}
addons:
  nginxGatewayFabric: {enabled: true}
nodeGroups:
  - name: system
    machineFlavor: platform
    machineImageId: "pve1=9100,pve2=9101"
    machineCores: 2
    machineMemoryMiB: 4096
    machineRootVolume: {diskSize: 30}
    machineTags: [proxcloud, pc-platform, pc-platform-pi-0123abcd]
    machineCount: 2
    machineLabels: {node-restriction.kubernetes.io/proxcloud-node-group: system}
  - name: kamaji
    machineFlavor: platform
    machineImageId: "pve2=9101"
    machineCores: 2
    machineMemoryMiB: 4096
    machineRootVolume: {diskSize: 30}
    machineTags: [proxcloud, pc-platform, pc-platform-pi-0123abcd]
    machineCount: 1
    machineLabels: {node-restriction.kubernetes.io/proxcloud-node-group: kamaji}
    taints: [{key: proxcloud.io/node-group, value: kamaji, effect: NoSchedule}]
`

const installerServicesCluster = "../../../installer/playbooks/templates/services-cluster.yml.j2"

// kubeadm mode carries what installer stage 50 gives its services cluster's control plane: the kube-vip static pod and
// its prepare script (compared as data with the j2's files block, so either side drifting fails here), the
// preKubeadmCommands, the provider-id kubelet argument and the certSANs; plus the admission plugins that place
// namespaced pods on node groups. It renders no tenant-only object: no Kamaji control plane, no CAAPH add-on, no
// ClusterResourceSet, no services-side CSI controller, no Gateway API carrier.
func TestChartKubeadmControlPlaneMatchesTheInstaller(t *testing.T) {
	docs := gatewayChartDocuments(t, helmTemplate(t, "pc-platform", "pc-platform", kubeadmTestValues))
	for _, doc := range docs {
		switch doc["kind"] {
		case "KamajiControlPlane", "HelmChartProxy", "ClusterResourceSet", "Secret", "Deployment":
			t.Errorf("kubeadm mode renders tenant-only %s/%s", doc["kind"], digStr(doc, "metadata", "name"))
		}
	}

	cluster := gatewayFindChartDocument(t, docs, "Cluster", "pc-platform")
	if k, n := digStr(cluster, "spec", "controlPlaneRef", "kind"), digStr(cluster, "spec", "controlPlaneRef", "name"); k != "KubeadmControlPlane" || n != "pc-platform-cp" {
		t.Errorf("controlPlaneRef = %s/%s", k, n)
	}
	pc := gatewayFindChartDocument(t, docs, "ProxmoxCluster", "pc-platform")
	if dig(pc, "spec", "externalManagedControlPlane") != nil {
		t.Error("kubeadm mode must not mark the control plane as externally managed")
	}
	if h, p := dig(pc, "spec", "controlPlaneEndpoint", "host"), dig(pc, "spec", "controlPlaneEndpoint", "port"); h != "192.0.2.10" || p != 6443 {
		t.Errorf("controlPlaneEndpoint = %v:%v, want the kube-vip address on 6443", h, p)
	}

	kcp := gatewayFindChartDocument(t, docs, "KubeadmControlPlane", "pc-platform-cp")
	if dig(kcp, "spec", "replicas") != 1 || digStr(kcp, "spec", "version") != "v1.35.10" {
		t.Errorf("replicas/version = %v/%v", dig(kcp, "spec", "replicas"), dig(kcp, "spec", "version"))
	}
	spec, _ := dig(kcp, "spec", "kubeadmConfigSpec").(map[string]any)

	raw, err := os.ReadFile(installerServicesCluster)
	if err != nil {
		t.Fatal(err)
	}
	j2 := string(raw)
	// The j2's files and preKubeadmCommands, with its two variables set to what the chart was given: the endpoint, and
	// the kube-vip version of versions.yml (so the chart's default image stays the installer's).
	_, block, ok := strings.Cut(j2, "\n    files:\n")
	block, _, ok2 := strings.Cut(block, "\n    initConfiguration:\n")
	if !ok || !ok2 {
		t.Fatalf("no files block in the KubeadmControlPlane of %s", installerServicesCluster)
	}
	block = strings.NewReplacer(
		"{{ svc.network.endpoint }}", "192.0.2.10",
		"{{ pc_ver.kube_vip }}", installerKubeVipVersion(t),
	).Replace("    files:\n" + block)
	if strings.Contains(block, "{{") || strings.Contains(block, "{%") {
		t.Fatalf("the j2's control-plane files gained a template expression this test does not set:\n%s", block)
	}
	var want map[string]any
	if err := yaml.Unmarshal([]byte(block), &want); err != nil {
		t.Fatalf("parse the j2's files block: %v\n%s", err, block)
	}
	for _, key := range []string{"files", "preKubeadmCommands"} {
		if !reflect.DeepEqual(spec[key], want[key]) {
			t.Errorf("kubeadmConfigSpec.%s differs from %s\n got: %#v\nwant: %#v", key, installerServicesCluster, spec[key], want[key])
		}
	}

	m := regexp.MustCompile(`\{name: provider-id, value: "([^"]+)"\}`).FindStringSubmatch(j2)
	if m == nil {
		t.Fatalf("no provider-id kubelet argument in %s", installerServicesCluster)
	}
	providerID := strings.NewReplacer("{{ '{{' }}", "{{", "{{ '}}' }}", "}}").Replace(m[1])
	for _, phase := range []string{"initConfiguration", "joinConfiguration"} {
		args, _ := dig(spec, phase, "nodeRegistration", "kubeletExtraArgs").([]any)
		got := map[string]any{}
		for _, a := range args {
			if am, ok := a.(map[string]any); ok {
				got[digStr(am, "name")] = am["value"]
			}
		}
		if got["provider-id"] != providerID {
			t.Errorf("%s provider-id = %v, want the j2's %q", phase, got["provider-id"], providerID)
		}
		if got["node-labels"] != "topology.kubernetes.io/region=az1,topology.kubernetes.io/zone=pve1" {
			t.Errorf("%s node-labels = %v", phase, got["node-labels"])
		}
		// The machine's own address, not the kube-vip one: a placeholder the prepare script fills in on the machine.
		if got["node-ip"] != "__NODE_IP__" {
			t.Errorf("%s node-ip = %v, want the placeholder the prepare script replaces", phase, got["node-ip"])
		}
	}
	if n := strings.Count(j2, `{name: node-ip, value: "__NODE_IP__"}`); n != 2 {
		t.Errorf("%s names the node-ip placeholder %d times, want it in the control plane's init and join", installerServicesCluster, n)
	}
	// The files block is equal on both sides (above), so the chart's is read for what both must hold.
	var vip, prepare string
	files, _ := spec["files"].([]any)
	for _, f := range files {
		switch fm, _ := f.(map[string]any); digStr(fm, "path") {
		case "/etc/kubernetes/manifests/kube-vip.yaml":
			vip = digStr(fm, "content")
		case "/etc/kube-vip-prepare.sh":
			prepare = digStr(fm, "content")
		}
	}
	for _, want := range []string{`sed -i "s/__NODE_IP__/$ip/"`, "/run/kubeadm/kubeadm.yaml", "/run/kubeadm/kubeadm-join-config.yaml", "ip -4 -o route get"} {
		if !strings.Contains(prepare, want) {
			t.Errorf("the prepare script does not fill in the node-ip (%q is missing):\n%s", want, prepare)
		}
	}
	// Cluster API hands the script to cloud-init as a Jinja template: an expression in it would be rendered away.
	if strings.Contains(prepare, "{{") || strings.Contains(prepare, "{%") || strings.Contains(prepare, "{#") {
		t.Errorf("the prepare script holds a Jinja expression:\n%s", prepare)
	}
	// kube-vip tries a renewal again for 40 s (a lab measured a write of 19.5 s; with a renew deadline of 10 s
	// kube-vip dropped the API address five times), within client-go's rule lease > renew deadline > retry period.
	// Its client cuts each request at 10 s, so this covers a stall, not a write of 40 s.
	lease := map[string]int{}
	for _, m := range regexp.MustCompile(`\{name: (vip_\w+), value: "(\d+)"\}`).FindAllStringSubmatch(vip, -1) {
		lease[m[1]], _ = strconv.Atoi(m[2])
	}
	if d, r, p := lease["vip_leaseduration"], lease["vip_renewdeadline"], lease["vip_retryperiod"]; r < 30 || d <= r || p < 1 || 2*p >= r {
		t.Errorf("kube-vip lease duration/renew deadline/retry period = %d/%d/%d s", d, r, p)
	}

	// certSANs: the j2's composition (endpoint first, then the extra names) with the chart's inputs.
	if !strings.Contains(j2, "certSANs: {{ ([svc.network.endpoint] + svc.api_extra_sans) | to_json }}") {
		t.Fatalf("the j2's certSANs changed; update the chart and this test together")
	}
	if got := dig(spec, "clusterConfiguration", "apiServer", "certSANs"); !reflect.DeepEqual(got, []any{"192.0.2.10", "platform.example.com"}) {
		t.Errorf("certSANs = %v", got)
	}
	wantArgs := []any{map[string]any{"name": "enable-admission-plugins", "value": "NodeRestriction,PodNodeSelector,PodTolerationRestriction"}}
	if got := dig(spec, "clusterConfiguration", "apiServer", "extraArgs"); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("apiServer extraArgs = %v, want %v", got, wantArgs)
	}

	// The control-plane VM: the hashed template it names, pinned to its node in the platform pool.
	mt := gatewayFindChartDocument(t, docs, "ProxmoxMachineTemplate", digStr(kcp, "spec", "machineTemplate", "spec", "infrastructureRef", "name"))
	for key, want := range map[string]any{"sourceNode": "pve1", "templateID": 9100, "pool": "proxcloud-system", "storage": "local-zfs", "memoryMiB": 4096} {
		if got := dig(mt, "spec", "template", "spec", key); got != want {
			t.Errorf("control-plane machine %s = %v, want %v", key, got, want)
		}
	}
}

func installerKubeVipVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../../installer/playbooks/group_vars/all/versions.yml")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Versions struct {
			KubeVip string `yaml:"kube_vip"`
		} `yaml:"pc_versions"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil || v.Versions.KubeVip == "" {
		t.Fatalf("versions.yml kube_vip: %v", err)
	}
	return v.Versions.KubeVip
}

// The control-plane VM's template and tags have no default: a values document that leaves one out fails the render
// instead of cloning some other template (9210 is the docs' tenant node template) or leaving the VM without the
// pc-platform-<id> tag a Remove after a lost seed finds it by.
func TestChartKubeadmRequiresMachineImageAndTags(t *testing.T) {
	for _, key := range []string{"machineImageId", "machineTags"} {
		t.Run(key, func(t *testing.T) {
			var values map[string]any
			if err := yaml.Unmarshal([]byte(kubeadmTestValues), &values); err != nil {
				t.Fatal(err)
			}
			delete(dig(values, "controlPlane", "kubeadm").(map[string]any), key)
			out, err := gatewayRenderRawChart(t, "pc-platform", values)
			if want := "controlPlane.kubeadm." + key + " is required"; err == nil || !strings.Contains(out, want) {
				t.Errorf("render without %s must fail with %q, got err=%v\n%s", key, want, err, out)
			}
		})
	}
}

// proxmox.network.vlan (chart 0.1.14) becomes networkDevices[].vlan of the control plane's and of every node group's
// machine template, which CAPMOX writes as tag=<n> on the NIC. Without the value no template names a VLAN, so a
// cluster rendered by 0.1.13 keeps its templates (their names hash the machine spec).
func TestChartKubeadmVLAN(t *testing.T) {
	devices := func(vlan int) (out []map[string]any) {
		var values map[string]any
		if err := yaml.Unmarshal([]byte(kubeadmTestValues), &values); err != nil {
			t.Fatal(err)
		}
		if vlan != 0 {
			dig(values, "proxmox", "network").(map[string]any)["vlan"] = vlan
		}
		raw, err := gatewayRenderRawChart(t, "pc-platform", values)
		if err != nil {
			t.Fatalf("render: %v\n%s", err, raw)
		}
		for _, doc := range strings.Split(raw, "\n---") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || obj["kind"] != "ProxmoxMachineTemplate" {
				continue
			}
			out = append(out, dig(obj, "spec", "template", "spec", "network").(map[string]any)["networkDevices"].([]any)[0].(map[string]any))
		}
		return out
	}
	tagged := devices(140)
	if len(tagged) != 4 { // the control plane, system on pve1 and pve2, kamaji on pve2
		t.Fatalf("%d machine templates, want 4", len(tagged))
	}
	for _, d := range tagged {
		if d["vlan"] != 140 || d["bridge"] != "vmbr0" {
			t.Errorf("device %v: want vlan 140 on vmbr0", d)
		}
	}
	for _, d := range devices(0) {
		if _, ok := d["vlan"]; ok {
			t.Errorf("device %v names a VLAN without the value", d)
		}
	}
}

// controlPlane.kubeadm.replicas (chart 0.1.14) is the KubeadmControlPlane's replica count, 1 or 3, with one machine
// template on one node. The count is not in the template's name, so 1 to 3 adds machines and replaces none; the
// default is the golden's 1. vipLease reaches kube-vip's three lease variables.
func TestChartKubeadmReplicas(t *testing.T) {
	render := func(set func(cp map[string]any)) (string, error) {
		var values map[string]any
		if err := yaml.Unmarshal([]byte(kubeadmTestValues), &values); err != nil {
			t.Fatal(err)
		}
		set(dig(values, "controlPlane", "kubeadm").(map[string]any))
		return gatewayRenderRawChart(t, "pc-platform", values)
	}
	control := func(raw string) (replicas any, template, vip string) {
		docs := gatewayChartDocuments(t, raw)
		kcp := gatewayFindChartDocument(t, docs, "KubeadmControlPlane", "pc-platform-cp")
		for _, f := range dig(kcp, "spec", "kubeadmConfigSpec", "files").([]any) {
			if f := f.(map[string]any); f["path"] == "/etc/kubernetes/manifests/kube-vip.yaml" {
				vip = f["content"].(string)
			}
		}
		return dig(kcp, "spec", "replicas"), digStr(kcp, "spec", "machineTemplate", "spec", "infrastructureRef", "name"), vip
	}
	one, err := render(func(map[string]any) {})
	if err != nil {
		t.Fatalf("render: %v\n%s", err, one)
	}
	three, err := render(func(cp map[string]any) { cp["replicas"] = 3 })
	if err != nil {
		t.Fatalf("render with 3: %v\n%s", err, three)
	}
	n1, tpl1, vip1 := control(one)
	n3, tpl3, vip3 := control(three)
	if n1 != 1 || n3 != 3 {
		t.Errorf("replicas %v and %v, want 1 and 3", n1, n3)
	}
	if tpl1 == "" || tpl1 != tpl3 || vip1 != vip3 {
		t.Errorf("3 replicas change the machine template (%s, %s) or kube-vip: every machine would be replaced", tpl1, tpl3)
	}
	if strings.Count(three, "kind: ProxmoxMachineTemplate") != strings.Count(one, "kind: ProxmoxMachineTemplate") {
		t.Error("3 replicas render another number of machine templates")
	}
	for _, bad := range []int{0, 2, 5} {
		out, err := render(func(cp map[string]any) { cp["replicas"] = bad })
		// 0 is "not set": the chart's default.
		if bad == 0 {
			if n, _, _ := control(out); err != nil || n != 1 {
				t.Errorf("replicas 0: %v, %v", n, err)
			}
			continue
		}
		if err == nil || !strings.Contains(out, "is not 1 or 3") {
			t.Errorf("replicas %d must fail the render, got err=%v\n%s", bad, err, out)
		}
	}
	leased, err := render(func(cp map[string]any) {
		cp["vipLease"] = map[string]any{"duration": 15, "renewDeadline": 10, "retryPeriod": 2}
	})
	if err != nil {
		t.Fatalf("render with a lease: %v\n%s", err, leased)
	}
	_, _, vip := control(leased)
	for _, want := range []string{`{name: vip_leaseduration, value: "15"}`, `{name: vip_renewdeadline, value: "10"}`, `{name: vip_retryperiod, value: "2"}`} {
		if !strings.Contains(vip, want) {
			t.Errorf("kube-vip lacks %s:\n%s", want, vip)
		}
	}
}
