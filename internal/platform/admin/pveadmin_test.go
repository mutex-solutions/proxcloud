package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
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

func TestTokenWarnings(t *testing.T) {
	full := map[string]bool{}
	for _, chk := range pve.PlatformPrivileges {
		for _, p := range chk.Privs {
			full[p] = true
		}
	}
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
			got := tokenWarnings(context.Background(), cc, tt.tokenID)
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
			out, herr := pveConnectionTest(context.Background(), fakeCluster(t, tt.over, tt.lacks...), nil, tt.all)
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
		out, herr := pveConnectionTest(context.Background(), cfg, nil, true)
		if herr != nil || len(out.Checks) != 1 || out.Checks[0].ID != "connect" || out.Checks[0].Status != checkFail {
			t.Fatalf("got %+v %+v, want one failed connect row", herr, out.Checks)
		}
	})
	t.Run("healthy call keeps the response shape", func(t *testing.T) {
		out, herr := pveConnectionTest(context.Background(), fakeCluster(t, nil, "Mapping.Use"), nil, false)
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
	_, herr := pveConnectionTest(context.Background(), fakeCluster(t, nil), es, false)
	if herr == nil || !strings.Contains(herr.Msg, `public network "vmbr0" is the management bridge on node pve1`) {
		t.Fatalf("default call: %+v", herr)
	}
	out, herr := pveConnectionTest(context.Background(), fakeCluster(t, nil), es, true)
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
