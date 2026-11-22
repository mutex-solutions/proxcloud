package kamaji

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func renderedObjects(t *testing.T, out string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader([]byte(out)))
	var objs []map[string]any
	for {
		var o map[string]any
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			return objs
		} else if err != nil {
			t.Fatal(err)
		}
		if o != nil {
			objs = append(objs, o)
		}
	}
}

// templateNames is every machine and bootstrap template name of a render, by kind.
func templateNames(t *testing.T, out string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, o := range renderedObjects(t, out) {
		if k := o["kind"]; k == "ProxmoxMachineTemplate" || k == "KubeadmConfigTemplate" {
			names[k.(string)+" "+o["metadata"].(map[string]any)["name"].(string)] = true
		}
	}
	return names
}

const groupNetworkBase = `
nodeGroups:
  - name: workers
    machineFlavor: s
    machineImageId: "pve1=9210"
    machineCores: 2
    machineMemoryMiB: 2048
    machineRootVolume: {diskSize: 30}
    machineTags: [proxcloud]
    machineCount: 1
`

const sandboxGroup = `
  - name: sandbox
    machineFlavor: sandbox
    machineImageId: "pve1=9210"
    machineCores: 2
    machineMemoryMiB: 4096
    machineRootVolume: {diskSize: 40}
    machineTags: [proxcloud, pc-sandbox]
    machineCount: 1
    pool: proxcloud-sbx
    addresses: ["192.0.2.64/28"]
    dnsServers: ["1.1.1.1", "8.8.8.8"]
`

// Chart 0.1.18: a group with pool, addresses and dnsServers gets its own pool in its machine template, an
// InClusterIPPool of its range and net0 pointed at it with defaultIPv4 false; a group without them, in the same render,
// keeps the machine and bootstrap template names it had without the sandbox group.
func TestChartRendersGroupPoolRangeAndDNS(t *testing.T) {
	plain := helmTemplate(t, "pc-platform", "pc-platform", groupNetworkBase)
	out := helmTemplate(t, "pc-platform", "pc-platform", groupNetworkBase+sandboxGroup)

	for name := range templateNames(t, plain) {
		if !templateNames(t, out)[name] {
			t.Errorf("adding a sandbox group renamed %s of the workers group", name)
		}
	}
	var pool map[string]any
	var machine map[string]any
	for _, o := range renderedObjects(t, out) {
		name, _ := o["metadata"].(map[string]any)["name"].(string)
		switch {
		case o["kind"] == "InClusterIPPool":
			if pool != nil {
				t.Fatalf("two InClusterIPPools: only the sandbox group sets addresses")
			}
			pool = o
		case o["kind"] == "ProxmoxMachineTemplate" && strings.HasPrefix(name, "pc-platform-sandbox-pve1-"):
			machine = o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		case o["kind"] == "ProxmoxMachineTemplate":
			spec := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			if spec["pool"] != "pc-example" {
				t.Errorf("%s: pool %v, want the cluster's pc-example", name, spec["pool"])
			}
			if dev := spec["network"].(map[string]any)["networkDevices"].([]any)[0].(map[string]any); dev["ipPoolRef"] != nil || dev["defaultIPv4"] != nil || dev["dnsServers"] != nil {
				t.Errorf("%s: a group without the keys got %v", name, dev)
			}
		}
	}
	if pool == nil || machine == nil {
		t.Fatalf("no InClusterIPPool or sandbox machine template:\n%s", out)
	}
	if pool["apiVersion"] != "ipam.cluster.x-k8s.io/v1alpha2" || pool["metadata"].(map[string]any)["name"] != "pc-platform-sandbox" {
		t.Errorf("pool: %v", pool)
	}
	if got, want := pool["spec"], map[string]any{"addresses": []any{"192.0.2.64/28"}, "prefix": 24, "gateway": "192.0.2.1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pool spec %v, want %v", got, want)
	}
	if machine["pool"] != "proxcloud-sbx" {
		t.Errorf("sandbox machine pool %v", machine["pool"])
	}
	dev, _ := json.Marshal(machine["network"].(map[string]any)["networkDevices"])
	want := `[{"bridge":"vexample","defaultIPv4":false,"dnsServers":["1.1.1.1","8.8.8.8"],"ipPoolRef":[{"apiGroup":"ipam.cluster.x-k8s.io","kind":"InClusterIPPool","name":"pc-platform-sandbox"}],"model":"virtio","name":"net0"}]`
	if string(dev) != want {
		t.Errorf("sandbox net0\n got %s\nwant %s", dev, want)
	}
}

// addons.sandbox (0.1.18) renders the sandbox chart's HelmChartProxy in mode separate whatever the values say, with the
// values passed through; without it no tenant cluster gets one (the tenant goldens hold that).
func TestChartRendersTheSandboxAddon(t *testing.T) {
	out := helmTemplate(t, "pc-sandbox", "pc-sandbox", `addons:
  sandbox:
    enabled: true
    values: {mode: shared, proxy: {address: 192.0.2.150, domain: sbx.example.org}}
`)
	var hcp map[string]any
	for _, o := range renderedObjects(t, out) {
		if o["kind"] == "HelmChartProxy" && o["metadata"].(map[string]any)["name"] == "pc-sandbox-sandbox" {
			hcp = o["spec"].(map[string]any)
		}
	}
	if hcp == nil {
		t.Fatalf("no sandbox HelmChartProxy:\n%s", out)
	}
	if hcp["chartName"] != "proxcloud-sandbox" || hcp["version"] != "0.1.0" || hcp["namespace"] != "sandbox-system" || hcp["repoURL"] != "oci://ghcr.io/mutex-solutions/charts" {
		t.Errorf("chart: %v", hcp)
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(hcp["valuesTemplate"].(string)), &values); err != nil {
		t.Fatal(err)
	}
	if values["mode"] != "separate" || values["proxy"].(map[string]any)["domain"] != "sbx.example.org" {
		t.Errorf("values %v: mode must be separate, the rest passed through", values)
	}
}
