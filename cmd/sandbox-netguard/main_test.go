package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCNIRejectsUnknownCommand(t *testing.T) {
	t.Setenv("CNI_COMMAND", "FORCE")
	if process([]byte(`{"cniVersion":"0.3.1"}`)) == nil {
		t.Fatal("unknown CNI command succeeded")
	}
}
func TestCNIUIDAndNodeBinding(t *testing.T) {
	for _, a := range [][4]string{{"n", "old", "new", "n"}, {"n", "", "uid", "n"}, {"n", "uid", "uid", "other"}, {"", "uid", "uid", "n"}} {
		if bindPod(a[0], a[1], a[2], a[3]) == nil {
			t.Fatal(a)
		}
	}
	if err := bindPod("n", "uid", "uid", "n"); err != nil {
		t.Fatal(err)
	}
}
func TestRateValidation(t *testing.T) {
	for _, s := range []string{"", "0M", "-1M", "1G", "100001M", "1M;reboot"} {
		if _, e := parseRate(s); e == nil {
			t.Fatal(s)
		}
	}
	if n, e := parseRate("100M"); e != nil || n != 100 {
		t.Fatal(n, e)
	}
}
func TestPrevResultRequiresCiliumHostVeth(t *testing.T) {
	if _, e := hostVeth(json.RawMessage(`{"interfaces":[{"name":"eth0","sandbox":"/run/netns/a"}]}`)); e == nil {
		t.Fatal("accepted no host veth")
	}
	n, e := hostVeth(json.RawMessage(`{"interfaces":[{"name":"lxc1234"},{"name":"eth0","sandbox":"/run/netns/a"}]}`))
	if e != nil || n != "lxc1234" {
		t.Fatal(n, e)
	}
}
func TestHostOwnedTBFNoCiliumFilterClobber(t *testing.T) {
	old := run
	defer func() { run = old }()
	var args string
	run = func(name string, a ...string) ([]byte, error) {
		args = name + " " + strings.Join(a, " ")
		return nil, nil
	}
	if e := police("/run/netns/cni-a", 1); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(args, "nsenter --net=/run/netns/cni-a -- tc qdisc replace dev eth0 root tbf rate 1mbit burst 65536") {
		t.Fatal(args)
	}
	if strings.Contains(args, "filter") {
		t.Fatal("touches Cilium filters")
	}
	run = func(string, ...string) ([]byte, error) { return nil, errors.New("tc missing") }
	if police("/run/netns/a", 1) == nil {
		t.Fatal("must fail closed")
	}
}

// fakeCiliumAPI serves cilium-config, the CiliumNodeConfig list (status ncsStatus when not 200) and Node n1.
func fakeCiliumAPI(t *testing.T, cm, ncs string, ncsStatus int) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/kube-system/configmaps/cilium-config":
			fmt.Fprintf(w, `{"data":%s}`, cm)
		case "/apis/cilium.io/v2/namespaces/kube-system/ciliumnodeconfigs":
			if ncsStatus != 200 {
				w.WriteHeader(ncsStatus)
				return
			}
			fmt.Fprintf(w, `{"items":%s}`, ncs)
		case "/api/v1/nodes/n1":
			fmt.Fprint(w, `{"metadata":{"labels":{"proxcloud.io/node-group":"sandbox"}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestInstallWaitsForCiliumToLeaveTheConflist(t *testing.T) {
	platform := `{"custom-cni-conf":"false","write-cni-conf-when-ready":"/host/etc/cni/net.d/05-cilium.conflist"}`
	cnc := func(defaults string) string {
		return `[{"metadata":{"name":"sandbox-nodes"},"spec":{"nodeSelector":{"matchLabels":{"proxcloud.io/node-group":"sandbox"}},"defaults":` + defaults + `}}]`
	}
	for _, c := range []struct {
		name, cm, ncs string
		status        int
		ok            bool
	}{
		{"cluster-wide", `{"custom-cni-conf":"true"}`, `[]`, 200, true},
		{"cluster-wide, CRD absent", `{"custom-cni-conf":"true"}`, ``, 404, true},
		{"cluster-wide, RBAC without ciliumnodeconfigs", `{"custom-cni-conf":"true"}`, ``, 403, true},
		{"per-node", platform, cnc(`{"custom-cni-conf":"true","write-cni-conf-when-ready":""}`), 200, true},
		{"neither", platform, `[]`, 200, false},
		{"neither, RBAC without ciliumnodeconfigs", platform, ``, 403, false},
		{"per-node without write-cni-conf-when-ready", platform, cnc(`{"custom-cni-conf":"true"}`), 200, false},
		{"list error", `{"custom-cni-conf":"true"}`, ``, 500, false},
	} {
		err := ciliumLeavesConf(http.DefaultClient, "t", fakeCiliumAPI(t, c.cm, c.ncs, c.status), "n1")
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
		}
	}
}
