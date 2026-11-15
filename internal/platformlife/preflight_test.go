package platformlife

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// preflightPVE is a one-node PVE that answers each token's effective permissions from perms (token id → path → privs)
// and records every request that is not a GET.
type preflightPVE struct {
	perms  map[string]map[string][]string
	writes []string
}

func (f *preflightPVE) client(t *testing.T, url, token string) *pve.Client {
	t.Helper()
	c, err := pve.New(pve.Config{APIURL: url, TokenID: token, Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *preflightPVE) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "PVEAPIToken=")
	token = token[:strings.LastIndex(token, "=")]
	data := map[string]string{
		"/cluster/status":           `[{"type":"node","name":"pve1","ip":"192.0.2.11"}]`,
		"/nodes/pve1/network":       `[{"iface":"vmbr0","address":"192.0.2.11","gateway":"192.0.2.1","cidr":"192.0.2.11/24"}]`,
		"/cluster/firewall/options": `{"enable":1}`,
		"/cluster/firewall/rules":   `[]`,
		"/cluster/sdn/vnets":        `[]`,
		"/storage": `[{"storage":"local","type":"dir","content":"iso,import"},{"storage":"local-lvm","type":"lvmthin","content":"images"},
			{"storage":"nas-iso","type":"nfs","content":"iso","shared":1}]`,
		"/cluster/resources": `[{"type":"storage","storage":"local","node":"pve1","status":"available"},
			{"type":"storage","storage":"local-lvm","node":"pve1","status":"available"},{"type":"storage","storage":"nas-iso","node":"pve1","status":"available"}]`,
		"/access/acl":              `[{"path":"/storage/local","type":"user","ugid":"capmox@pve","roleid":"PVEDatastoreAdmin","propagate":0}]`,
		"/access/users/capmox@pve": `{"groups":[]}`,
		"/access/roles":            `[{"roleid":"PVEDatastoreAdmin","privs":"Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit"}]`,
	}
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	body, ok := data[path]
	if path == "/access/permissions" {
		p := r.URL.Query().Get("path")
		privs := map[string]int{}
		for _, v := range f.perms[token][p] {
			privs[v] = 1
		}
		b, _ := json.Marshal(map[string]any{p: privs})
		body, ok = string(b), true
	}
	if !ok {
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, `{"errors":{"path":"no fake for %s"}}`, path)
		return
	}
	fmt.Fprintf(w, `{"data":%s}`, body)
}

func checkMap(checks []Check) map[string]Check {
	out := map[string]Check{}
	for _, c := range checks {
		out[c.ID] = c
	}
	return out
}

// A site set up by the managed-services script passes every check, writing nothing; a missing grant, a portal token
// on the seed's pool and platform addresses outside blockedCidrs each fail their own row.
func TestPreflight(t *testing.T) {
	in := validInstallation(t)
	in.Spec.Storage.Import = "local"
	for i := range in.Spec.Clusters[0].NodeGroups {
		in.Spec.Clusters[0].NodeGroups[i].Nodes = []string{"pve1"}
	}
	s := in.Spec.Storage
	grant := func(checks []pve.PrivCheck) map[string][]string {
		out := map[string][]string{}
		for _, c := range checks {
			out[c.Path] = append(out[c.Path], c.Privs...)
		}
		return out
	}
	capi := grant(pve.CAPIPrivileges(s.Boot, s.ISO))
	f := &preflightPVE{perms: map[string]map[string][]string{
		pve.CAPITokenID:            capi,
		pve.CSITokenID:             grant(pve.CSIPrivileges(s.Boot)),
		pve.DefaultPlatformTokenID: grant(pve.KubeMintPrivileges("nas-templates")),
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	pf := Preflight{
		Installation:      in,
		Site:              Site{CIDRPool: "10.100.0.0/16", Blocked: []string{"10.200.200.0/25"}},
		ProviderImport:    "nas-templates",
		Portal:            &pve.Service{C: f.client(t, srv.URL, pve.DefaultPlatformTokenID), ServiceID: "svc-pve"},
		CAPI:              f.client(t, srv.URL, pve.CAPITokenID),
		CSI:               f.client(t, srv.URL, pve.CSITokenID),
		TemplateUpdatesVM: true, GitHub: true,
		Blocked: func(context.Context, string) (bool, error) { return false, nil },
	}
	ctx := context.Background()
	checks := pf.Run(ctx)
	if !OK(checks) {
		t.Fatalf("a set-up site: %+v", checks)
	}
	if got := len(checks); got != 14 {
		t.Errorf("%d rows, want 14: %+v", got, checks)
	}
	if c := checkMap(checks)["isolation"]; !strings.Contains(c.Message, "first tenant network") {
		t.Errorf("no tenant network yet: %+v", c)
	}
	if len(f.writes) != 0 {
		t.Fatalf("the preflight wrote to PVE: %v", f.writes)
	}

	capi["/pool/"+pve.SeedPool] = []string{"VM.Audit"}
	f.perms[pve.DefaultPlatformTokenID]["/pool/"+pve.SeedPool] = []string{"VM.Audit"}
	f.perms[pve.CAPITokenID]["/storage/nas-iso"] = []string{"Datastore.Audit"}
	pf.Site.Blocked = nil
	pf.CSI = nil
	pf.Blocked = func(context.Context, string) (bool, error) { return true, nil }
	got := checkMap(pf.Run(ctx))
	for id, want := range map[string]string{
		"privileges:capi":      "on /pool/" + pve.SeedPool,
		"pool-fence":           "the portal token holds 1 privileges on /pool/" + pve.SeedPool,
		"iso-store-visibility": "[local, nas-iso]",
		"isolation":            "add 10.200.200.10/32, 10.200.200.20/32, 10.200.200.30/31",
		"credentials":          pve.CSITokenID + " is not stored",
		"images":               "is on the blocklist",
	} {
		if c := got[id]; c.Status != CheckFail || !strings.Contains(c.Message, want) {
			t.Errorf("%s: want a failure naming %q, got %+v", id, want, c)
		}
	}
	if _, ok := got["privileges:csi"]; ok {
		t.Error("no CSI token, yet its privileges were checked")
	}
}
