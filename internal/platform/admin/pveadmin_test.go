package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestCheckPublicNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/cluster/status":
			fmt.Fprint(w, `{"data":[{"type":"node","name":"pve1","ip":"10.0.0.11"}]}`)
		case "/api2/json/nodes/pve1/network":
			fmt.Fprint(w, `{"data":[{"iface":"vmbr0","address":"10.0.0.11","gateway":"10.0.0.1"},{"iface":"vmbr1"}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		pns     []externalservice.PVEPublicNetwork
		wantErr string
	}{
		{"none configured", nil, ""},
		{"dedicated bridge", []externalservice.PVEPublicNetwork{{ID: "vmbr1"}}, ""},
		{"management bridge", []externalservice.PVEPublicNetwork{{ID: "vmbr1"}, {ID: "vmbr0"}}, `public network "vmbr0" is the management bridge on node pve1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			herr := checkPublicNetworks(context.Background(), cc, tt.pns)
			switch {
			case tt.wantErr == "" && herr != nil:
				t.Fatalf("unexpected error %v", herr)
			case tt.wantErr != "" && (herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr)):
				t.Fatalf("got %+v, want 400 containing %q", herr, tt.wantErr)
			}
		})
	}
}

func TestTokenChecks(t *testing.T) {
	full := map[string]bool{}
	for _, chk := range pve.PlatformPrivileges {
		for _, p := range chk.Privs {
			full[p] = true
		}
	}
	mint := without(full, "") // a copy
	mint["User.Modify"] = true
	pool := without(full, "")
	pool["Permissions.Modify"] = true
	tests := []struct {
		name    string
		tokenID string
		status  int             // non-200 = /access/permissions fails
		privs   map[string]bool // granted on every path
		zones   map[string]bool // granted on /sdn/zones instead, when set: an ACL there replaces what /sdn passes down
		want    []string        // substrings, in order
	}{
		{"complete platform token", "proxcloud@pve!platform", 200, full, nil, nil},
		{"root token", "root@pam!x", 200, full, nil, []string{"belongs to root@pam"}},
		{"missing privilege", "proxcloud@pve!platform", 200, without(full, "VM.Audit"), nil, []string{"Token lacks VM.Audit on /pool"}},
		{"permissions unreadable", "proxcloud@pve!platform", 500, nil, nil, []string{"Could not read the token's permissions"}},
		{"mint grant on all of /access/groups", "proxcloud@pve!platform", 200, mint, nil, []string{"Token holds User.Modify on /; User.Modify on /access; User.Modify on /access/groups: with User.Modify or Permissions.Modify on /, /access or /access/groups a stolen portal token can create an API token for any user"}},
		{"mint role on /pool", "proxcloud@pve!platform", 200, pool, nil, []string{"Permissions.Modify on /pool: with"}},
		{"where the script is", "proxcloud@pve!platform", 200, mint, nil, []string{"GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>, or step 2 of the Managed services page; an installer site in split mode: make upgrade"}},
		{"sdn rights shadowed on zones", "proxcloud@pve!platform", 200, full, map[string]bool{"User.Modify": true}, []string{"Token lacks SDN.Allocate, SDN.Audit, SDN.Use on /sdn/zones"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api2/json/access/permissions" || tt.status != 200 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				privs := tt.privs
				if tt.zones != nil && r.URL.Query().Get("path") == "/sdn/zones" {
					privs = tt.zones
				}
				var ps []string
				for p := range privs {
					ps = append(ps, fmt.Sprintf("%q:1", p))
				}
				fmt.Fprintf(w, `{"data":{%q:{%s}}}`, r.URL.Query().Get("path"), strings.Join(ps, ","))
			}))
			defer srv.Close()
			cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: tt.tokenID, Secret: "sec"})
			if err != nil {
				t.Fatal(err)
			}
			var got []string // what the route returns as warnings
			for _, c := range tokenChecks(context.Background(), cc, tt.tokenID) {
				if c.Status != checkOK {
					got = append(got, c.Message)
				}
			}
			if len(got) != len(tt.want) {
				t.Fatalf("warnings = %q, want %d matching %q", got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("warning %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func without(m map[string]bool, key string) map[string]bool {
	out := map[string]bool{}
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// TestCheckNodeFirewalls: an online node on the iptables firewall (nftables 0 or
// absent) fails the preflight — the vnet isolation rules would be ignored there.
func TestCheckNodeFirewalls(t *testing.T) {
	tests := []struct {
		name    string
		pve2    string // pve2's firewall options body; "" = request fails
		wantErr string
	}{
		{"all nftables", `{"data":{"nftables":1}}`, ""},
		{"iptables node", `{"data":{"enable":1}}`, "node pve2 runs the legacy iptables firewall"},
		{"options unreadable", "", "cannot read the node firewall options"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api2/json/nodes":
					fmt.Fprint(w, `{"data":[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"},{"node":"pve3","status":"offline"}]}`)
				case "/api2/json/nodes/pve1/firewall/options":
					fmt.Fprint(w, `{"data":{"nftables":1}}`)
				case "/api2/json/nodes/pve2/firewall/options":
					if tt.pve2 == "" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					fmt.Fprint(w, tt.pve2)
				default: // the offline pve3 must never be asked
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
			if err != nil {
				t.Fatal(err)
			}
			herr := checkNodeFirewalls(context.Background(), cc)
			switch {
			case tt.wantErr == "" && herr != nil:
				t.Fatalf("unexpected error %v", herr)
			case tt.wantErr != "" && (herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr)):
				t.Fatalf("got %+v, want 400 containing %q", herr, tt.wantErr)
			}
		})
	}
}

func TestApplyStorageClasses(t *testing.T) {
	stored := func() pgdoc.M {
		return pgdoc.M{"config": map[string]any{"storageClasses": map[string]any{
			"nas": map[string]any{"class": "capacity", "suggestedClass": "standard"},
		}}}
	}
	tests := []struct {
		name    string
		body    string
		wantErr string
		want    map[string]any // storageClasses after apply
	}{
		{"set override keeps suggestion", `{"nas":"performance"}`, "",
			map[string]any{"nas": pgdoc.M{"class": "performance", "suggestedClass": "standard"}}},
		{"clear override keeps suggestion", `{"nas":""}`, "",
			map[string]any{"nas": pgdoc.M{"suggestedClass": "standard"}}},
		{"new storage", `{"local-zfs":"performance"}`, "",
			map[string]any{"nas": pgdoc.M{"class": "capacity", "suggestedClass": "standard"}, "local-zfs": pgdoc.M{"class": "performance"}}},
		{"invalid class", `{"nas":"fast"}`, "must be one of", nil},
		{"bad body", `[1]`, "Invalid request body", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := stored()
			req := httptest.NewRequest(http.MethodPut, "/service/s1/storage-classes", strings.NewReader(tt.body))
			herr := applyStorageClasses(req, doc)
			if tt.wantErr != "" {
				if herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr) {
					t.Fatalf("got %+v, want 400 containing %q", herr, tt.wantErr)
				}
				return
			}
			if herr != nil {
				t.Fatalf("unexpected error %+v", herr)
			}
			got := map[string]any(doc["config"].(pgdoc.M)["storageClasses"].(pgdoc.M))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

// A tenant network leaves through a node's NAT, so a file store is out of its reach only inside the networks the
// isolation rules drop: the nodes' management networks and blockedCidrs.
func TestStoreReachWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/cluster/status":
			fmt.Fprint(w, `{"data":[{"type":"node","name":"pve1","ip":"10.0.0.11"}]}`)
		case "/api2/json/nodes/pve1/network":
			fmt.Fprint(w, `{"data":[{"iface":"vmbr0","address":"10.0.0.11","gateway":"10.0.0.1","cidr":"10.0.0.11/24"}]}`)
		case "/api2/json/storage":
			fmt.Fprint(w, `{"data":[
				{"storage":"local","type":"dir","content":"iso"},
				{"storage":"nas-templates","type":"nfs","server":"10.0.0.20","content":"iso,images"},
				{"storage":"nas-capacity","type":"nfs","server":"10.50.0.20","content":"images"},
				{"storage":"smb","type":"cifs","server":"10.60.0.5","content":"iso"},
				{"storage":"old","type":"nfs","server":"10.70.0.5","content":"iso","disable":1}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	got := storeReachWarnings(context.Background(), cc, []string{"10.60.0.5"})
	if len(got) != 1 || !strings.Contains(got[0], "Storage nas-capacity (nfs at 10.50.0.20)") {
		t.Fatalf("warnings = %q", got)
	}
	if got := storeReachWarnings(context.Background(), cc, []string{"10.50.0.0/16", "10.60.0.5/32"}); len(got) != 0 {
		t.Fatalf("all blocked, warnings = %q", got)
	}
	// A server given by name is checked at the addresses it resolves to.
	defs := []pve.StorageDef{{Storage: "nas", Type: "nfs", Server: "nas.example"}}
	inside := func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.20")}, nil }
	if got := unblockedStores(defs, []string{"10.0.0.0/24"}, inside); len(got) != 0 {
		t.Fatalf("resolved inside: %q", got)
	}
	gone := func(string) ([]net.IP, error) { return nil, fmt.Errorf("no such host") }
	if got := unblockedStores(defs, nil, gone); len(got) != 1 || !strings.Contains(got[0], "Could not resolve nas.example") {
		t.Fatalf("unresolvable: %q", got)
	}
}

// Ceph monitors, iSCSI portals and gluster servers are network stores too, in every spelling PVE takes.
func TestStoreHostsAndCephReach(t *testing.T) {
	for _, c := range []struct {
		def  pve.StorageDef
		want []string
	}{
		{pve.StorageDef{Type: "rbd", Monhost: "10.0.0.1 10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2"}},
		{pve.StorageDef{Type: "cephfs", Monhost: "10.0.0.1:6789;10.0.0.2,ceph3"}, []string{"10.0.0.1", "10.0.0.2", "ceph3"}},
		{pve.StorageDef{Type: "cephfs", Monhost: "[v2:10.0.0.1:3300/0,v1:10.0.0.1:6789/0]"}, []string{"10.0.0.1"}},
		{pve.StorageDef{Type: "rbd"}, nil}, // the nodes' own Ceph
		{pve.StorageDef{Type: "iscsi", Portal: "10.0.0.20:3260"}, []string{"10.0.0.20"}},
		{pve.StorageDef{Type: "glusterfs", Server: "gl1"}, []string{"gl1"}},
		{pve.StorageDef{Type: "dir", Server: "10.0.0.9"}, nil},
	} {
		if got := storeHosts(c.def); !reflect.DeepEqual(got, c.want) {
			t.Errorf("storeHosts(%+v) = %q, want %q", c.def, got, c.want)
		}
	}
	defs := []pve.StorageDef{
		{Storage: "ext-ceph", Type: "rbd", Monhost: "10.0.0.5 10.70.0.5"},
		{Storage: "p8-iscsi", Type: "iscsi", Portal: "10.80.0.20"},
		{Storage: "own-ceph", Type: "cephfs"},
	}
	got := unblockedStores(defs, []string{"10.0.0.0/24"}, nil)
	if len(got) != 2 || !strings.Contains(got[0], "Storage ext-ceph (rbd at 10.70.0.5)") || !strings.Contains(got[1], "Storage p8-iscsi (iscsi at 10.80.0.20)") {
		t.Fatalf("warnings = %q", got)
	}
}

// A shared or ISO-less bootstrap ISO store is said at Test connection, not only at the next create.
func TestBootISOWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/storage":
			fmt.Fprint(w, `{"data":[{"storage":"local","type":"dir","content":"iso"},{"storage":"nas-templates","type":"nfs","content":"iso","shared":1}]}`)
		case "/api2/json/cluster/resources":
			fmt.Fprint(w, `{"data":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	if got := bootISOWarnings(context.Background(), cc, "local"); len(got) != 0 {
		t.Fatalf("local: %q", got)
	}
	if got := bootISOWarnings(context.Background(), cc, "nas-templates"); len(got) != 1 || !strings.Contains(got[0], "is shared") {
		t.Fatalf("nas-templates: %q", got)
	}
}

// fakeCluster serves a healthy one-node PVE 9.2 cluster whose token holds every platform privilege but lacks, and
// returns the config that reaches it. over replaces the answer of a path: a JSON body, or "!<status>" to fail it.
func fakeCluster(t *testing.T, over map[string]string, lacks ...string) pve.Config {
	t.Helper()
	answers := map[string]string{
		"/version":                     `{"data":{"version":"9.2.1","release":"9.2"}}`,
		"/cluster/firewall/options":    `{"data":{"enable":1}}`,
		"/nodes":                       `{"data":[{"node":"pve1","status":"online"}]}`,
		"/nodes/pve1/firewall/options": `{"data":{"nftables":1}}`,
		"/cluster/status":              `{"data":[{"type":"node","name":"pve1","ip":"10.0.0.11"}]}`,
		"/nodes/pve1/network":          `{"data":[{"iface":"vmbr0","address":"10.0.0.11","gateway":"10.0.0.1","cidr":"10.0.0.11/24"},{"iface":"vmbr1"}]}`,
		"/storage":                     `{"data":[{"storage":"local","type":"dir","content":"iso,images,import"}]}`,
		"/cluster/resources":           `{"data":[{"type":"storage","storage":"local","node":"pve1","status":"available"}]}`,
		"/cluster/sdn/zones":           `{"data":[]}`,
		"/cluster/mapping/pci":         `{"data":[]}`,
		"/pools/pc-templates":          `{"data":{"members":[]}}`,
		"/nodes/pve1/apt/versions":     `{"data":[{"Package":"frr-pythontools","CurrentState":"Installed"},{"Package":"ceph","CurrentState":"NotInstalled"}]}`,
	}
	for path, body := range over {
		answers[path] = body
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		body, ok := answers[path]
		switch {
		case !ok && path == "/access/permissions":
			var ps []string
			for _, chk := range pve.PlatformPrivileges {
				for _, p := range chk.Privs {
					if !slices.Contains(lacks, p) {
						ps = append(ps, fmt.Sprintf("%q:1", p))
					}
				}
			}
			fmt.Fprintf(w, `{"data":{%q:{%s}}}`, r.URL.Query().Get("path"), strings.Join(ps, ","))
		case !ok:
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasPrefix(body, "!"):
			status, _ := strconv.Atoi(body[1:])
			w.WriteHeader(status)
		default:
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	return pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "sec"}
}

// checkRow finds a row of the connection test by id.
func checkRow(checks []PVECheck, id string) *PVECheck {
	for i := range checks {
		if checks[i].ID == id {
			return &checks[i]
		}
	}
	return nil
}

// The default call stops at the first failed check with the 400 it has always answered (installer stage 60 reads
// it); ?checks=all answers every check it could still run.
func TestPVEConnectionTest(t *testing.T) {
	const fwOff = "Proxmox preflight failed: the datacenter firewall is disabled, so security groups and tenant network isolation are not enforced. Enable it under Datacenter > Firewall > Options."
	tests := []struct {
		name    string
		over    map[string]string
		lacks   []string
		all     bool
		wantErr string            // the 400 message, exactly
		want    map[string]string // check id -> status
		wantMsg map[string]string // check id -> substring of its message
	}{
		{name: "healthy, default call", want: map[string]string{}},
		{name: "healthy, all checks", all: true, want: map[string]string{
			"connect": checkOK, "firewall-datacenter": checkOK, "firewall-nodes": checkOK, "token": checkOK,
			"privileges:/": checkOK, "privileges:/pool": checkOK, "privileges:/mapping/pci": checkOK}},
		{name: "firewall off, default call", over: map[string]string{"/cluster/firewall/options": `{"data":{"enable":0}}`}, lacks: []string{"VM.Audit"}, wantErr: fwOff},
		{name: "firewall off still lists privilege gaps", over: map[string]string{"/cluster/firewall/options": `{"data":{"enable":0}}`}, lacks: []string{"VM.Audit"}, all: true,
			want:    map[string]string{"connect": checkOK, "firewall-datacenter": checkFail, "firewall-nodes": checkOK, "privileges:/pool": checkWarn, "privileges:/sdn": checkOK},
			wantMsg: map[string]string{"firewall-datacenter": fwOff, "privileges:/pool": "Token lacks VM.Audit on /pool"}},
		{name: "firewall unreadable, default call", over: map[string]string{"/cluster/firewall/options": "!403"},
			wantErr: "Proxmox preflight failed: cannot read the datacenter firewall options: pve: 403 Forbidden"},
		{name: "iptables node, default call", over: map[string]string{"/nodes/pve1/firewall/options": `{"data":{"enable":1}}`},
			wantErr: "Proxmox preflight failed: node pve1 runs the legacy iptables firewall, which ignores the FORWARD rules that isolate tenant networks. Set nftables: 1 under Node > Firewall > Options on every node, then restart its guests."},
		{name: "iptables node, all checks", over: map[string]string{"/nodes/pve1/firewall/options": `{"data":{"enable":1}}`}, all: true,
			want: map[string]string{"firewall-datacenter": checkOK, "firewall-nodes": checkFail, "token": checkOK}},
		{name: "token refused, default call", over: map[string]string{"/version": "!401"}, wantErr: "Proxmox authentication failed: pve: 401 Unauthorized"},
		{name: "token refused, all checks", over: map[string]string{"/version": "!401"}, all: true,
			want:    map[string]string{"connect": checkOK, "token": checkFail},
			wantMsg: map[string]string{"token": "Proxmox authentication failed: pve: 401 Unauthorized"}},
		{name: "permissions unreadable, all checks", over: map[string]string{"/access/permissions": "!500"}, all: true,
			want: map[string]string{"token": checkOK, "privileges": checkWarn}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, herr := pveConnectionTest(context.Background(), fakeCluster(t, tt.over, tt.lacks...), nil, tt.all, true)
			if tt.wantErr != "" {
				if herr == nil || herr.Status != http.StatusBadRequest || herr.Msg != tt.wantErr {
					t.Fatalf("got %+v, want 400 %q", herr, tt.wantErr)
				}
				return
			}
			if herr != nil {
				t.Fatalf("unexpected error %+v", herr)
			}
			if !tt.all && out.Checks != nil {
				t.Fatalf("default call returned checks: %+v", out.Checks)
			}
			for id, status := range tt.want {
				if c := checkRow(out.Checks, id); c == nil || c.Status != status {
					t.Errorf("check %s = %+v, want status %s (all: %+v)", id, c, status, out.Checks)
				}
			}
			for id, msg := range tt.wantMsg {
				if c := checkRow(out.Checks, id); c == nil || !strings.Contains(c.Message, msg) {
					t.Errorf("check %s = %+v, want message containing %q", id, c, msg)
				}
			}
		})
	}
	t.Run("unreachable, all checks", func(t *testing.T) {
		cfg := fakeCluster(t, nil)
		cfg.APIURL = "http://127.0.0.1:1"
		out, herr := pveConnectionTest(context.Background(), cfg, nil, true, true)
		if herr != nil || len(out.Checks) != 1 || out.Checks[0].ID != "connect" || out.Checks[0].Status != checkFail {
			t.Fatalf("got %+v %+v, want one failed connect row", herr, out.Checks)
		}
	})
	t.Run("healthy call keeps the response shape", func(t *testing.T) {
		out, herr := pveConnectionTest(context.Background(), fakeCluster(t, nil, "Mapping.Use"), nil, false, true)
		if herr != nil || out.Version != "9.2.1" || len(out.Nodes) != 1 || len(out.Storages) != 1 {
			t.Fatalf("got %+v %+v", herr, out)
		}
		if want := []string{"Token lacks Mapping.Use on /mapping/pci (see docs/proxmox-permissions.md)"}; !reflect.DeepEqual(out.Warnings, want) {
			t.Fatalf("warnings = %q, want %q", out.Warnings, want)
		}
	})
}

// A stored provider adds its own checks; each finding is a warning and a row.
func TestPVEConnectionTestStoredProvider(t *testing.T) {
	es := &externalservice.ExternalService{ID: "svc1", Config: map[string]any{
		"provider": "proxmox", "isoStorage": "local",
		"network": map[string]any{"publicNetworks": []any{map[string]any{"id": "vmbr0"}}},
	}}
	_, herr := pveConnectionTest(context.Background(), fakeCluster(t, nil), es, false, true)
	if herr == nil || !strings.Contains(herr.Msg, `public network "vmbr0" is the management bridge on node pve1`) {
		t.Fatalf("default call: %+v", herr)
	}
	out, herr := pveConnectionTest(context.Background(), fakeCluster(t, nil), es, true, true)
	if herr != nil {
		t.Fatalf("all checks: %+v", herr)
	}
	for id, status := range map[string]string{"public-networks": checkFail, "token": checkOK, "store-reach": checkOK, "boot-iso": checkOK} {
		if c := checkRow(out.Checks, id); c == nil || c.Status != status {
			t.Errorf("check %s = %+v, want status %s", id, c, status)
		}
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "/admin/service/svc1/seed-cleanup") {
		t.Errorf("warnings = %q", out.Warnings)
	}
}

func TestPVEAuthConfig(t *testing.T) {
	p := func(s string) *string { return &s }
	yes, no := true, false
	stored := func() *externalservice.ExternalService {
		return &externalservice.ExternalService{
			Config: map[string]any{"provider": "proxmox", "apiUrl": "https://pve1:8006", "apiUrls": []any{"https://pve2:8006"},
				"tokenId": "proxcloud@pve!platform", "fingerprint": "AA:BB", "insecureTls": true},
			Secret: map[string]any{"tokenSecret": "stored-secret"},
		}
	}
	storedCfg := pve.Config{APIURL: "https://pve1:8006", APIURLs: []string{"https://pve2:8006"}, TokenID: "proxcloud@pve!platform",
		Secret: "stored-secret", Fingerprint: "AA:BB", InsecureTLS: true}
	edited := pveAuthReq{APIURL: p("https://vip:8006"), APIURLs: []string{}, CACert: p("PEM"), Fingerprint: p(""), InsecureTLS: &no}
	tests := []struct {
		name    string
		req     pveAuthReq
		es      *externalservice.ExternalService
		overlay bool
		want    pve.Config
		wantOK  bool
	}{
		{"body credentials", pveAuthReq{APIURL: p("https://a:8006"), APIURLs: []string{"https://b:8006"}, TokenID: p("t@pve!x"), TokenSecret: "s", InsecureTLS: &yes}, nil, true,
			pve.Config{APIURL: "https://a:8006", APIURLs: []string{"https://b:8006"}, TokenID: "t@pve!x", Secret: "s", InsecureTLS: true}, true},
		{"body without a secret", pveAuthReq{APIURL: p("https://a:8006"), TokenID: p("t@pve!x")}, nil, true, pve.Config{}, false},
		{"body without a url", pveAuthReq{TokenID: p("t@pve!x"), TokenSecret: "s"}, nil, true, pve.Config{}, false},
		{"stored, empty body", pveAuthReq{}, stored(), true, storedCfg, true},
		{"stored, edited fields keep the stored secret", edited, stored(), true,
			pve.Config{APIURL: "https://vip:8006", APIURLs: []string{}, CACert: "PEM", TokenID: "proxcloud@pve!platform", Secret: "stored-secret"}, true},
		{"stored, edited fields ignored without manage", edited, stored(), false, storedCfg, true},
		{"stored, body secret wins", pveAuthReq{APIURL: p("https://a:8006"), TokenID: p("t@pve!x"), TokenSecret: "new"}, stored(), true,
			pve.Config{APIURL: "https://a:8006", TokenID: "t@pve!x", Secret: "new"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pveAuthConfig(tt.req, tt.es, tt.overlay)
			if ok != tt.wantOK || (ok && !reflect.DeepEqual(got, tt.want)) {
				t.Fatalf("got %+v %v, want %+v %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// A test that sends the stored token secret to another address, or under another trust, is written to the audit log:
// the route is otherwise not audited and the stored provider does not change, so nothing else would say where the
// secret went.
func TestPVEAuthAuditEvent(t *testing.T) {
	p := func(s string) *string { return &s }
	yes := true
	stored := &externalservice.ExternalService{ID: "pve1", Name: "Proxmox",
		Config: map[string]any{"provider": "proxmox", "apiUrl": "https://pve1:8006", "tokenId": "proxcloud@pve!platform", "fingerprint": "AA:BB"},
		Secret: map[string]any{"tokenSecret": "stored-secret"}}
	tests := []struct {
		name    string
		req     pveAuthReq
		manage  bool
		herr    *httpx.HTTPError
		want    []audit.PropertyChange // nil = no event
		outcome string
	}{
		{"the stored connection", pveAuthReq{}, true, nil, nil, ""},
		{"the stored address sent again", pveAuthReq{APIURL: p("https://pve1:8006"), Fingerprint: p("AA:BB")}, true, nil, nil, ""},
		{"an edited token id", pveAuthReq{TokenID: p("other@pve!t")}, true, nil, nil, ""},
		{"its own secret", pveAuthReq{APIURL: p("https://elsewhere:8006"), TokenID: p("t@pve!x"), TokenSecret: "own"}, true, nil, nil, ""},
		{"edits ignored without manage", pveAuthReq{APIURL: p("https://elsewhere:8006"), InsecureTLS: &yes}, false, nil, nil, ""},
		{"another address", pveAuthReq{APIURL: p("https://elsewhere:8006")}, true, nil,
			[]audit.PropertyChange{{Field: "apiUrl", OldValue: "https://pve1:8006", NewValue: "https://elsewhere:8006"}}, audit.OutcomeSuccess},
		{"a failover address", pveAuthReq{APIURLs: []string{"https://pve2:8006"}}, true, nil,
			[]audit.PropertyChange{{Field: "apiUrls", OldValue: "[]", NewValue: `["https://pve2:8006"]`}}, audit.OutcomeSuccess},
		{"no certificate check", pveAuthReq{InsecureTLS: &yes, Fingerprint: p("")}, true, httpx.BadRequest("Proxmox authentication failed: x"),
			[]audit.PropertyChange{{Field: "fingerprint", OldValue: "AA:BB", NewValue: ""}, {Field: "insecureTls", OldValue: "false", NewValue: "true"}},
			audit.OutcomeFailure},
		{"another CA", pveAuthReq{CACert: p("PEM")}, true, nil, []audit.PropertyChange{{Field: "caCert", OldValue: "", NewValue: "PEM"}}, audit.OutcomeSuccess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, ok := pveAuthConfig(tt.req, stored, tt.manage)
			if !ok {
				t.Fatal("pveAuthConfig refused")
			}
			r := httptest.NewRequest(http.MethodPost, "/service/proxmox/auth?externalServiceId=pve1", nil)
			r = r.WithContext(httpx.WithRC(r.Context(), &httpx.RequestContext{Sub: "op-1"}))
			ev, ok := pveAuthAuditEvent(r, tt.req, stored, cfg, tt.herr)
			if tt.want == nil {
				if ok {
					t.Fatalf("unexpected event %+v", ev)
				}
				return
			}
			if !ok || !reflect.DeepEqual(ev.Changes, tt.want) || ev.Outcome != tt.outcome || ev.ResourceID != "pve1" ||
				ev.ResourceType != "EXTERNAL_SERVICE" || ev.Actor == nil || ev.Actor.ID != "op-1" || ev.ResourceMetadata["apiUrl"] != cfg.APIURL {
				t.Fatalf("event %v %+v, want changes %+v outcome %s", ok, ev, tt.want, tt.outcome)
			}
			if tt.herr != nil && ev.ErrorMessage != tt.herr.Msg {
				t.Fatalf("error message %q", ev.ErrorMessage)
			}
			if b, _ := json.Marshal(ev); strings.Contains(string(b), "stored-secret") {
				t.Fatalf("the secret is in the event: %s", b)
			}
		})
	}
}

// The route itself: the default call answers the first failure as a 400, ?checks=all as a 200 with rows, and
// apiUrls in the body is a failover address.
func TestProxmoxAuthRoute(t *testing.T) {
	cfg := fakeCluster(t, map[string]string{"/cluster/firewall/options": `{"data":{"enable":0}}`})
	body := fmt.Sprintf(`{"apiUrl":"http://127.0.0.1:1","apiUrls":[%q],"tokenId":%q,"tokenSecret":"sec"}`, cfg.APIURL, cfg.TokenID)
	call := func(query string, rc *httpx.RequestContext) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/service/proxmox/auth"+query, strings.NewReader(body))
		rec := httptest.NewRecorder()
		(&Handler{adminIssuer: "operator-realm"}).proxmoxAuth(rec, req.WithContext(httpx.WithRC(req.Context(), rc)))
		return rec
	}
	op := &httpx.RequestContext{SigV4KeyID: "pk-test"}
	if rec := call("", &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("", op); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "the datacenter firewall is disabled") {
		t.Fatalf("default call: %d %s", rec.Code, rec.Body.String())
	}
	rec := call("?checks=all", op)
	var got struct {
		Data PVEAuthResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("checks=all: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if c := checkRow(got.Data.Checks, "firewall-datacenter"); c == nil || c.Status != checkFail {
		t.Fatalf("checks = %+v", got.Data.Checks)
	}
	if c := checkRow(got.Data.Checks, "privileges:/pool"); c == nil || c.Status != checkOK {
		t.Fatalf("checks = %+v", got.Data.Checks)
	}
}

// What only ?checks=all reports: one case per check against the fake cluster. The same provider passes the default
// call whatever these find, because none of them was ever part of it.
func TestPVESetupChecks(t *testing.T) {
	provider := func(edit func(cfg map[string]any)) *externalservice.ExternalService {
		cfg := map[string]any{
			"provider": "proxmox", "isoStorage": "local",
			"network": map[string]any{"defaultZone": "overlay", "cidrPool": "10.100.0.0/16",
				"zones": map[string]any{"overlay": map[string]any{"kind": "evpn"}}},
			"storageProfiles":       map[string]any{"std": map[string]any{"storages": map[string]any{"*": "local"}}},
			"defaultStorageProfile": "std",
		}
		if edit != nil {
			edit(cfg)
		}
		return &externalservice.ExternalService{ID: "svc1", Config: cfg}
	}
	network := func(key string, value any) func(map[string]any) {
		return func(cfg map[string]any) {
			n := cfg["network"].(map[string]any)
			if value == nil {
				delete(n, key)
			} else {
				n[key] = value
			}
		}
	}
	const noImport = `{"data":[{"storage":"local","type":"dir","content":"iso,images"}]}`
	tests := []struct {
		name    string
		over    map[string]string
		es      *externalservice.ExternalService
		id      string
		want    string // status; "" = no such row
		wantMsg string
	}{
		{"version 9.2", nil, nil, "version", checkOK, "9.2.1"},
		{"version 9.1", map[string]string{"/version": `{"data":{"version":"9.1.4"}}`}, nil, "version", checkFail, "needs 9.2 or later"},
		{"version 8", map[string]string{"/version": `{"data":{"version":"8.4.1"}}`}, nil, "version", checkFail, "Proxmox VE 8.4.1 found"},
		{"version 10", map[string]string{"/version": `{"data":{"version":"10.0"}}`}, nil, "version", checkOK, ""},
		{"version unreadable", map[string]string{"/version": `{"data":{"version":"next"}}`}, nil, "version", checkWarn, "Could not read"},

		{"template pool exists", nil, nil, "template-pool", checkOK, ""},
		{"template pool missing", map[string]string{"/pools/pc-templates": "!404"}, nil, "template-pool", checkWarn, "The first template import creates it"},
		{"template pool missing: the manual step stays the fallback", map[string]string{"/pools/pc-templates": "!404"}, nil, "template-pool", checkWarn, "for a token without Pool.Allocate on /pool, create it with `pveum pool add pc-templates`"},
		{"template pool unreadable", map[string]string{"/pools/pc-templates": "!403"}, nil, "template-pool", checkWarn, "Could not read the pool"},

		{"zone made later", nil, provider(nil), "zone", checkOK, "created with the first tenant network"},
		{"zone exists", map[string]string{"/cluster/sdn/zones": `{"data":[{"zone":"overlay","type":"evpn"}]}`}, provider(nil), "zone", checkOK, "Zone overlay (evpn) exists"},
		{"zone of another type", map[string]string{"/cluster/sdn/zones": `{"data":[{"zone":"overlay","type":"vlan"}]}`}, provider(nil), "zone", checkFail, "exists in Proxmox as a vlan zone"},
		{"zone not configured", nil, provider(network("zones", nil)), "zone", checkFail, "No tenant network zone is configured"},
		{"zones unreadable", map[string]string{"/cluster/sdn/zones": "!403"}, provider(nil), "zone", checkWarn, "Could not read the SDN zones"},
		{"no zone row without a stored provider", nil, nil, "zone", "", ""},

		{"cidrPool clear", nil, provider(nil), "cidr-pool", checkOK, ""},
		{"cidrPool missing", nil, provider(network("cidrPool", nil)), "cidr-pool", checkFail, "No cidrPool is configured"},
		{"cidrPool too small", nil, provider(network("cidrPool", "10.100.0.0/25")), "cidr-pool", checkFail, "IPv4 range of /24 or larger"},
		{"cidrPool IPv6", nil, provider(network("cidrPool", "fd00::/48")), "cidr-pool", checkFail, "IPv4 range of /24 or larger"},
		{"cidrPool over the management network", nil, provider(network("cidrPool", "10.0.0.0/16")), "cidr-pool", checkWarn, "overlaps a management network"},
		{"cidrPool with a node network unread", map[string]string{"/nodes/pve1/network": "!500"}, provider(nil), "cidr-pool", checkWarn,
			"Could not read the networks of every node; cidrPool 10.100.0.0/16 was compared with the nodes that answered only"},
		{"cidrPool overlap found among the nodes that answered", map[string]string{
			"/cluster/status":     `{"data":[{"type":"node","name":"pve1","ip":"10.0.0.11"},{"type":"node","name":"pve2","ip":"10.0.5.12"}]}`,
			"/nodes/pve2/network": "!500",
		}, provider(network("cidrPool", "10.0.0.0/16")), "cidr-pool", checkWarn, "overlaps a management network"},

		{"default storage profile", nil, provider(nil), "storage-profile", checkOK, "std"},
		{"no storage profile", nil, provider(func(cfg map[string]any) { delete(cfg, "storageProfiles"); delete(cfg, "defaultStorageProfile") }), "storage-profile", checkFail, "No default storage profile"},
		{"default profile unknown", nil, provider(func(cfg map[string]any) { cfg["defaultStorageProfile"] = "fast" }), "storage-profile", checkFail, "fast is not one of the storage profiles"},

		{"import content present", nil, nil, "import-store", checkOK, ""},
		{"no import content", map[string]string{"/storage": noImport}, nil, "import-store", checkWarn, "pvesm set <storage> --content <its content>,import"},
		{"import store without import content", map[string]string{"/storage": noImport}, provider(func(cfg map[string]any) { cfg["importStorage"] = "local" }), "import-store", checkWarn, "pvesm set local --content iso,images,import"},
		{"import store unknown", nil, provider(func(cfg map[string]any) { cfg["importStorage"] = "nas" }), "import-store", checkWarn, "importStorage nas is not a storage of this cluster"},
		{"storages unreadable", map[string]string{"/storage": "!403"}, nil, "import-store", checkWarn, "Could not read the storages"},

		{"frr installed", nil, provider(nil), "frr", checkOK, ""},
		{"frr missing", map[string]string{"/nodes/pve1/apt/versions": `{"data":[{"Package":"frr-pythontools","CurrentState":"NotInstalled"}]}`}, provider(nil), "frr", checkWarn, "not installed on node pve1"},
		{"frr unreadable", map[string]string{"/nodes/pve1/apt/versions": "!403"}, provider(nil), "frr", checkWarn, "Could not read the node packages"},
		{"no frr row on a simple zone", nil, provider(network("zones", map[string]any{"overlay": map[string]any{"kind": "simple"}})), "frr", "", ""},
		{"no frr row without a zone", nil, provider(network("zones", nil)), "frr", "", ""},
		{"frr row with a zone per project", nil, provider(func(cfg map[string]any) {
			network("zones", map[string]any{"overlay": map[string]any{"kind": "simple"}})(cfg)
			network("isolation", "evpn-per-project")(cfg)
		}), "frr", checkOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fakeCluster(t, tt.over)
			out, herr := pveConnectionTest(context.Background(), cfg, tt.es, true, true)
			if herr != nil {
				t.Fatalf("unexpected error %+v", herr)
			}
			c := checkRow(out.Checks, tt.id)
			if tt.want == "" {
				if c != nil {
					t.Fatalf("unexpected row %+v", c)
				}
			} else if c == nil || c.Status != tt.want || !strings.Contains(c.Message, tt.wantMsg) {
				t.Fatalf("check %s = %+v, want %s containing %q", tt.id, c, tt.want, tt.wantMsg)
			}
			plain, herr := pveConnectionTest(context.Background(), cfg, tt.es, false, true)
			if herr != nil || plain.Checks != nil || plain.Bridges != nil {
				t.Fatalf("default call: %+v %+v", herr, plain)
			}
			for _, w := range plain.Warnings {
				if tt.wantMsg != "" && strings.Contains(w, tt.wantMsg) {
					t.Fatalf("default call gained the warning %q", w)
				}
			}
		})
	}
}

// Bridges and the fingerprint of a refused certificate say how the site is wired: only a caller who may change the
// provider gets them.
func TestPVEConnectionTestDetails(t *testing.T) {
	cfg := fakeCluster(t, nil)
	out, herr := pveConnectionTest(context.Background(), cfg, nil, true, true)
	want := []pve.NodeBridge{
		{Node: "pve1", Iface: "vmbr0", CIDR: "10.0.0.11/24", Management: true},
		{Node: "pve1", Iface: "vmbr1"},
	}
	if herr != nil || !reflect.DeepEqual(out.Bridges, want) {
		t.Fatalf("bridges = %+v (%+v), want %+v", out.Bridges, herr, want)
	}
	if out, _ := pveConnectionTest(context.Background(), cfg, nil, true, false); out.Bridges != nil || len(out.Checks) == 0 {
		t.Fatalf("without manage: bridges %+v, %d checks", out.Bridges, len(out.Checks))
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"version":"9.2.1"}}`)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes below
	srv.StartTLS()
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	presented := strings.ToUpper(strings.ReplaceAll(fmt.Sprintf("% x", sum[:]), " ", ":"))
	for _, tls := range []struct {
		name string
		cfg  pve.Config
	}{
		{"system CA pool", pve.Config{}},
		{"another fingerprint pinned", pve.Config{Fingerprint: "00" + presented[2:]}},
		{"another CA pinned", pve.Config{CACert: "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----"}},
	} {
		t.Run(tls.name, func(t *testing.T) {
			cfg := tls.cfg
			cfg.APIURL, cfg.TokenID, cfg.Secret = srv.URL, "proxcloud@pve!platform", "sec"
			out, herr := pveConnectionTest(context.Background(), cfg, nil, true, true)
			if herr != nil || out.PresentedFingerprint != presented {
				t.Fatalf("fingerprint = %q (%+v), want %q", out.PresentedFingerprint, herr, presented)
			}
			if c := checkRow(out.Checks, "connect"); c == nil || c.Status != checkFail || len(out.Checks) != 1 {
				t.Fatalf("checks = %+v", out.Checks)
			}
			if out, _ := pveConnectionTest(context.Background(), cfg, nil, true, false); out.PresentedFingerprint != "" {
				t.Fatalf("without manage: fingerprint %q", out.PresentedFingerprint)
			}
			// Pinning what was presented is accepted, and the default call says nothing new.
			pinned := cfg
			pinned.CACert, pinned.Fingerprint = "", presented
			if c, err := pve.New(pinned); err != nil {
				t.Fatal(err)
			} else if _, err := c.Version(context.Background()); err != nil {
				t.Fatalf("pinning the presented fingerprint: %v", err)
			}
			if _, herr := pveConnectionTest(context.Background(), cfg, nil, false, true); herr == nil || !strings.HasPrefix(herr.Msg, "Proxmox authentication failed: ") || strings.Contains(herr.Msg, presented) {
				t.Fatalf("default call: %+v", herr)
			}
		})
	}
	t.Run("refused token is not a certificate refusal", func(t *testing.T) {
		out, _ := pveConnectionTest(context.Background(), fakeCluster(t, map[string]string{"/version": "!401"}), nil, true, true)
		if out.PresentedFingerprint != "" {
			t.Fatalf("fingerprint = %q", out.PresentedFingerprint)
		}
	})
}

// GET /admin/setup/proxmox-script answers the generated script as text, and refuses anything that is not a plain id.
func TestProxmoxSetupScriptRoute(t *testing.T) {
	router := chi.NewRouter()
	(&Handler{adminIssuer: "operator-realm"}).Routes(router)
	get := func(query string, rc *httpx.RequestContext) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/setup/proxmox-script"+query, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req.WithContext(httpx.WithRC(req.Context(), rc)))
		return rec
	}
	op := &httpx.RequestContext{SigV4KeyID: "pk-test"}
	if rec := get("", &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}); rec.Code != http.StatusForbidden {
		t.Fatalf("tenant: %d %s", rec.Code, rec.Body.String())
	}
	script := func(tokenID string, gpu bool, importStorage string) string {
		t.Helper()
		s, err := pve.PlatformSetupScript(tokenID, gpu, importStorage)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	managed := func(tokenID string, gpu bool, importStorage, boot, iso string) string {
		t.Helper()
		s, err := pve.ManagedSetupScript(tokenID, gpu, importStorage, boot, iso)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for query, want := range map[string]string{
		"": script(pve.DefaultPlatformTokenID, false, ""),
		"?tokenId=portal%40pve%21cloud&gpu=true&importStorage=nas-templates":                     script("portal@pve!cloud", true, "nas-templates"),
		"?tokenId=portal@pve!cloud&gpu=0":                                                        script("portal@pve!cloud", false, ""),
		"?managedServices=false":                                                                 script(pve.DefaultPlatformTokenID, false, ""),
		"?managedServices=1&bootStorage=local-lvm":                                               managed(pve.DefaultPlatformTokenID, false, "", "local-lvm", "local"),
		"?managedServices=true&bootStorage=local-zfs&isoStorage=iso1&gpu=true&importStorage=nas": managed(pve.DefaultPlatformTokenID, true, "nas", "local-zfs", "iso1"),
	} {
		rec := get(query, op)
		if rec.Code != http.StatusOK || rec.Body.String() != want || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("GET %q: %d %s\n%s", query, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	for _, query := range []string{
		"?tokenId=" + url.QueryEscape("proxcloud@pve!platform; rm -rf /"),
		"?tokenId=" + url.QueryEscape("proxcloud@pve!platform'$(id)'"),
		"?tokenId=" + url.QueryEscape("proxcloud@pve!platform\nreboot"),
		"?tokenId=proxcloud",
		"?importStorage=" + url.QueryEscape("nas`id`"),
		"?importStorage=" + url.QueryEscape("nas' 'x"),
		"?gpu=maybe",
		"?managedServices=maybe&bootStorage=local-lvm",
		"?managedServices=true",
		"?managedServices=true&bootStorage=" + url.QueryEscape("local-lvm; reboot"),
		"?managedServices=true&bootStorage=local-lvm&isoStorage=" + url.QueryEscape("local'$(id)'"),
		"?bootStorage=local-lvm",
		"?isoStorage=local",
	} {
		if rec := get(query, op); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "pveum") {
			t.Errorf("GET %q: %d %s", query, rec.Code, rec.Body.String())
		}
	}
}

// The detect answer keeps the stored capabilities at the top level (installer stages 60 and 70 read it) and adds the
// proposal next to them.
func TestPVEDetectResponseShape(t *testing.T) {
	caps := externalservice.PVECapabilities{DetectedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Storages: map[string]pve.StorageCapability{
		"local-zfs": {Storage: "local-zfs", Type: "zfspool", Content: []string{"images"}, Snapshots: true, Nodes: map[string]pve.StorageNodeState{"pve1": {Active: true}}},
	}}
	raw, err := json.Marshal(pveDetectResponse{caps, pve.ProposeProfiles(caps.Storages, nil)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got["detectedAt"]) != `"2026-11-01T00:00:00Z"` || !strings.Contains(string(got["storages"]), `"local-zfs"`) ||
		string(got["proposedProfiles"]) != `{"std":{"snapshots":true,"storages":{"*":"local-zfs"}}}` {
		t.Fatalf("detect answer: %s", raw)
	}
}
