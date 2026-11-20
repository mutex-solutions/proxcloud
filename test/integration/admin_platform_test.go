//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/pvetest"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	plantedCAPI = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee000001"
	plantedCSI  = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee000002"
	portalToken = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee000003"
)

func platformBody(gateway string, seedCores int, rev int64) string {
	b, _ := json.Marshal(map[string]any{
		"name": "lab", "serviceId": "svc-pve", "rev": rev,
		"spec": map[string]any{
			"pveApiUrl": "https://10.200.200.2:8006",
			"storage":   map[string]any{"boot": "local-lvm", "iso": "local"},
			"network":   map[string]any{"bridge": "vmbr1", "prefix": "10.200.200.0/24", "gateway": gateway, "dns": []string{"10.200.200.1"}},
			"seed": map[string]any{"node": "pve1", "address": "10.200.200.10", "cores": seedCores, "memoryMiB": 4096, "diskGiB": 30,
				"apiSources": []string{"192.168.33.0/24"}},
			"clusters": []any{map[string]any{
				"endpoint": "10.200.200.20", "nodeRange": "10.200.200.30-10.200.200.49", "metallbRange": "10.200.200.200-10.200.200.220",
				"controlPlane": map[string]any{"node": "pve1", "cores": 2, "memoryMiB": 4096, "diskGiB": 30},
				"nodeGroups": []any{
					map[string]any{"name": "system", "nodes": []string{"pve1"}, "count": 1, "cores": 2, "memoryMiB": 4096, "diskGiB": 30},
					map[string]any{"name": "kamaji", "nodes": []string{"pve1"}, "count": 1, "cores": 2, "memoryMiB": 4096, "diskGiB": 30},
					map[string]any{"name": "dbaas", "nodes": []string{"pve1"}, "count": 1, "cores": 2, "memoryMiB": 4096, "diskGiB": 30},
				},
			}},
		},
	})
	return string(b)
}

// TestAdminPlatformInstallations drives the installation routes against PostgreSQL: the role each route needs, the
// rev and fixed-field and run refusals, and that a planted token secret appears in no response, no stored
// installation, no audit event and not in plain text in platformSecret, while the preflight still sends it to
// Proxmox.
func TestAdminPlatformInstallations(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	var mu sync.Mutex
	var auths []string
	fakePVE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		// Every token holds nothing anywhere: the privilege rows name each path they read.
		if strings.HasSuffix(r.URL.Path, "/access/permissions") {
			fmt.Fprintf(w, `{"data":{%q:{}}}`, r.URL.Query().Get("path"))
			return
		}
		// The CAPI token reads the nodes' memory and the VMs for the plan of a larger control plane: 8 GiB each, no VM.
		if strings.Contains(r.Header.Get("Authorization"), "capmox@pve!capi") {
			switch r.URL.Path {
			case "/api2/json/nodes":
				fmt.Fprint(w, `{"data":[{"node":"pve1","status":"online","maxmem":8589934592},{"node":"pve2","status":"online","maxmem":8589934592},{"node":"pve3","status":"online","maxmem":8589934592}]}`)
				return
			case "/api2/json/cluster/resources":
				fmt.Fprint(w, `{"data":[]}`)
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errors":{"fake":"nothing here"}}`))
	}))
	t.Cleanup(fakePVE.Close)
	enc := textcrypt.New("test-key")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform", "importStorage": "nas-templates",
		"storageProfiles": pgdoc.M{"fast": pgdoc.M{"storages": pgdoc.M{"*": "nvme-u2"}}, "archive": pgdoc.M{"bootDisk": false, "storages": pgdoc.M{"*": "truenas"}}},
		"network":         pgdoc.M{"cidrPool": "10.100.0.0/16", "blockedCidrs": []string{"10.200.200.0/25"}}},
		"secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	// LOCAL_IDP posture: the role comes from the adminPermission doc of the sub.
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		c, err := pve.New(es.PVEConfig())
		return &pve.Service{C: c, ServiceID: es.ID}, err
	})
	router := chi.NewRouter()
	h.Routes(router)

	var bodies []string
	type answer struct {
		Data   json.RawMessage `json:"data"`
		Errors struct {
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"errors"`
	}
	call := func(sub, method, path, body string, want int) answer {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		bodies = append(bodies, w.Body.String())
		if w.Code != want {
			t.Fatalf("%s %s %s: %d %s, want %d", sub, method, path, w.Code, w.Body.String(), want)
		}
		var a answer
		_ = json.Unmarshal(w.Body.Bytes(), &a)
		return a
	}
	code := func(a answer) string { s, _ := a.Errors.Details["code"].(string); return s }

	// Create: service:manage only.
	base := "/admin/platform/installations"
	call("support-sub", http.MethodPost, base, platformBody("10.200.200.1", 2, 0), http.StatusForbidden)
	var in platformlife.Installation
	if err := json.Unmarshal(call("admin-sub", http.MethodPost, base, platformBody("10.200.200.1", 2, 0), http.StatusOK).Data, &in); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(in.ID, "pi-") || in.Status.Phase != platformlife.PhaseDraft || in.Rev != 1 || in.CreatedBy != "admin-sub" ||
		in.Spec.Storage.Import != "nas-templates" || in.Spec.Seed.Pool != pve.SeedPool {
		t.Fatalf("created: %+v", in)
	}
	one := base + "/" + in.ID
	if a := call("admin-sub", http.MethodPost, base, platformBody("10.200.200.1", 2, 0), http.StatusConflict); code(a) != "installation_exists" {
		t.Fatalf("a second installation on the provider: %+v", a)
	}
	if a := call("admin-sub", http.MethodPost, base, platformBody("10.9.9.1", 2, 0), http.StatusBadRequest); code(a) != "invalid_installation" {
		t.Fatalf("a gateway outside the network: %+v", a)
	}
	// A preset fills what the body leaves out, and the stored spec names all of it: on a second provider, a body with
	// the addresses and one node becomes the homelab layout on a network that node routes.
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-home", "name": "home", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "home", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform", "importStorage": "nas-templates",
		"network": pgdoc.M{"cidrPool": "10.100.0.0/16"}}, "secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	presetBody := func(preset string) string {
		return `{"name":"home","serviceId":"svc-home","preset":"` + preset + `","spec":{"pveApiUrl":"https://10.90.0.2:8006","storage":{"boot":"local-lvm","iso":"local"},
			"network":{"prefix":"10.90.0.0/24","gateway":"10.90.0.1","dns":["10.90.0.1"]},"seed":{"node":"pve1","address":"10.90.0.5","apiSources":["192.168.33.0/24"]},
			"clusters":[{"endpoint":"10.90.0.10","nodeRange":"10.90.0.20-10.90.0.60","metallbRange":"10.90.0.100-10.90.0.150"}]}}`
	}
	if a := call("admin-sub", http.MethodPost, base, presetBody("production"), http.StatusBadRequest); code(a) != "invalid_installation" ||
		!strings.Contains(strings.Join(anyStrings(a.Errors.Details["problems"]), ","), "homelab, standard or custom") {
		t.Fatalf("an unknown preset: %+v", a)
	}
	if a := call("admin-sub", http.MethodPost, base, presetBody("custom"), http.StatusBadRequest); code(a) != "invalid_installation" {
		t.Fatalf("custom fills nothing, so the same body is not complete: %+v", a)
	}
	// Standard asks for three control plane machines only where the release builds them: a count the preset filled in
	// is never the reason a body is refused (this one names no bridge, which is).
	if a := call("admin-sub", http.MethodPost, base, presetBody("standard"), http.StatusBadRequest); code(a) != "invalid_installation" ||
		strings.Contains(strings.Join(anyStrings(a.Errors.Details["problems"]), ","), "control plane machines") {
		t.Fatalf("the standard preset without a bridge: %+v", a)
	}
	var home platformlife.Installation
	if err := json.Unmarshal(call("admin-sub", http.MethodPost, base, presetBody("homelab"), http.StatusOK).Data, &home); err != nil {
		t.Fatal(err)
	}
	if hc := home.Spec.Clusters[0]; home.Spec.Network.Mode != platformlife.NetworkRouted || home.Spec.Network.Bridge != "pcplat0" || home.Spec.Seed.MemoryMiB != 4096 ||
		hc.ControlPlane.Replicas != 1 || hc.ControlPlane.Node != "pve1" || len(hc.NodeGroups) != 3 || hc.NodeGroups[0].Count != 1 || hc.NodeGroups[0].Nodes[0] != "pve1" {
		t.Fatalf("the homelab preset: %+v", home.Spec)
	}
	// Reads: service:read.
	call("support-sub", http.MethodGet, base, "", http.StatusOK)
	call("support-sub", http.MethodGet, one, "", http.StatusOK)
	call("support-sub", http.MethodGet, one+"/plan", "", http.StatusOK)
	call("tenant-sub", http.MethodGet, one, "", http.StatusForbidden)

	// Credentials: write-only, service:manage, exact token ids.
	creds := `{"capi":{"tokenId":"capmox@pve!capi","secret":"` + plantedCAPI + `"},"csi":{"tokenId":"csi@pve!csi","secret":"` + plantedCSI + `"}}`
	call("support-sub", http.MethodPut, one+"/credentials", creds, http.StatusForbidden)
	call("admin-sub", http.MethodPut, one+"/credentials", `{"capi":{"tokenId":"root@pam!x","secret":"`+plantedCAPI+`"}}`, http.StatusBadRequest)
	call("admin-sub", http.MethodPut, one+"/credentials", `{"capi":{"tokenId":"capmox@pve!capi","secret":"not-a-uuid"}}`, http.StatusBadRequest)
	call("admin-sub", http.MethodPut, one+"/credentials", creds, http.StatusOK)
	var view struct {
		Credentials struct{ CAPI, CSI struct{ TokenID string } }
	}
	_ = json.Unmarshal(call("support-sub", http.MethodGet, one, "", http.StatusOK).Data, &view)
	if view.Credentials.CAPI.TokenID != pve.CAPITokenID || view.Credentials.CSI.TokenID != pve.CSITokenID {
		t.Fatalf("the view names the stored token ids: %+v", view)
	}
	sec, err := store.Secrets(ctx, in.ID)
	if err != nil || sec.CAPI.Secret != plantedCAPI || sec.CSI.Secret != plantedCSI {
		t.Fatalf("secrets do not open: %+v %v", sec, err)
	}

	// Preflight: service:manage; it reaches Proxmox with the stored tokens (a fake that refuses everything, so every
	// row that reads it fails) and answers no secret.
	call("support-sub", http.MethodPost, one+"/preflight", "", http.StatusForbidden)
	var pf struct {
		OK     bool
		Checks []platformlife.Check
	}
	_ = json.Unmarshal(call("admin-sub", http.MethodPost, one+"/preflight", "", http.StatusOK).Data, &pf)
	if pf.OK || len(pf.Checks) == 0 {
		t.Fatalf("preflight against a refusing PVE: %+v", pf)
	}
	// The mint role is read on the provider's own stores and zones, not on /storage alone: a boot store with a row of
	// the portal's own reads complete on the parent (production, 2026-11-17). A capacity store gets no cluster row.
	mintRows := 0
	for _, c := range pf.Checks {
		if c.ID != "privileges:kubemint" {
			continue
		}
		mintRows++
		for _, path := range []string{" on /storage/nvme-u2 ", " on /storage/nas-templates ", " on /sdn/zones/localnetwork "} {
			if !strings.Contains(c.Message, path) {
				t.Errorf("privileges:kubemint does not read%s: %s", path, c.Message)
			}
		}
		if strings.Contains(c.Message, "truenas") || !strings.Contains(c.Message, pve.KubeMintShadowed) {
			t.Errorf("privileges:kubemint: %s", c.Message)
		}
	}
	if mintRows != 1 {
		t.Errorf("%d privileges:kubemint rows", mintRows)
	}
	mu.Lock()
	sent := strings.Join(auths, "\n")
	mu.Unlock()
	if !strings.Contains(sent, "PVEAPIToken=proxcloud@pve!platform="+portalToken) || !strings.Contains(sent, "PVEAPIToken=capmox@pve!capi="+plantedCAPI) || !strings.Contains(sent, "PVEAPIToken=csi@pve!csi="+plantedCSI) {
		t.Fatal("the preflight did not use the stored platform tokens")
	}

	// An installation saved before v0.9.0 has no controlPlane.replicas: the answer names it (1), as docs/api.md says.
	var old map[string]any
	if found, err := db.C(platformlife.InstallationCollection).Get(ctx, in.ID, &old); err != nil || !found {
		t.Fatal(found, err)
	}
	delete(old["spec"].(map[string]any)["clusters"].([]any)[0].(map[string]any)["controlPlane"].(map[string]any), "replicas")
	if err := db.C(platformlife.InstallationCollection).Upsert(ctx, in.ID, old); err != nil {
		t.Fatal(err)
	}
	if a := call("support-sub", http.MethodGet, one, "", http.StatusOK); !strings.Contains(string(a.Data), `"replicas":1`) {
		t.Fatalf("a stored spec without the count is not answered with it: %s", a.Data)
	}
	// Update: a stale rev, a run in progress and, once installed, a fixed field are refused; a size change is not.
	call("support-sub", http.MethodPut, one, platformBody("10.200.200.1", 4, 1), http.StatusForbidden)
	if a := call("admin-sub", http.MethodPut, one, platformBody("10.200.200.1", 4, 1), http.StatusOK); !strings.Contains(string(a.Data), `"rev":2`) {
		t.Fatalf("update: %s", a.Data)
	}
	if a := call("admin-sub", http.MethodPut, one, platformBody("10.200.200.1", 4, 1), http.StatusConflict); code(a) != "rev_conflict" {
		t.Fatalf("stale rev: %+v", a)
	}
	if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{"status": pgdoc.M{"phase": "ready",
		"bom": pgdoc.M{"release": "0.7.0-dev", "sha256": "x"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if a := call("admin-sub", http.MethodPut, one, platformBody("10.200.200.254", 4, 2), http.StatusConflict); code(a) != "fixed_fields" ||
		!strings.Contains(strings.Join(anyStrings(a.Errors.Details["problems"]), ","), "spec.network") {
		t.Fatalf("network after install: %+v", a)
	}
	call("admin-sub", http.MethodPut, one, platformBody("10.200.200.1", 6, 2), http.StatusOK)
	// The control plane count of an installed platform. Where the platform's release builds one machine, 3 is refused
	// at the save: stored, it would fail every apply and upgrade.
	replicas := func(n int, rev int64) string {
		return strings.Replace(platformBody("10.200.200.1", 6, rev), `"controlPlane":{`, fmt.Sprintf(`"controlPlane":{"replicas":%d,`, n), 1)
	}
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !kamaji.ChartHasControlPlaneReplicas(platformlife.ClusterChartVersion(cur.BOM)) {
		if a := call("admin-sub", http.MethodPut, one, replicas(3, 3), http.StatusBadRequest); code(a) != "invalid_installation" ||
			!strings.Contains(strings.Join(anyStrings(a.Errors.Details["problems"]), ","), "which builds one") {
			t.Fatalf("3 under a release that builds one: %+v", a)
		}
	}
	// With a release that builds three: 1 to 3 is saved, and goes back to 1 for as long as no run built three.
	savedBOM := *cur.BOM
	defer func() { *cur.BOM = savedBOM }()
	cur.Components = slices.Clone(cur.Components)
	for i := range cur.Components {
		if cur.Components[i].Name == platformlife.ClusterName {
			cur.Components[i].Chart.Version = kamaji.ControlPlaneReplicasChartVersion
		}
	}
	if a := call("admin-sub", http.MethodPut, one, replicas(3, 3), http.StatusOK); !strings.Contains(string(a.Data), `"replicas":3`) {
		t.Fatalf("1 to 3 after install: %s", a.Data)
	}
	call("admin-sub", http.MethodPut, one, replicas(1, 4), http.StatusOK)
	call("admin-sub", http.MethodPut, one, replicas(3, 5), http.StatusOK)
	// The plan of that 3 on a platform whose run built one (lab round 2, G2F-2): the count is a change, and the nodes
	// of 8 GiB have no room for it, so the plan blocks with the numbers.
	if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{"status": pgdoc.M{"phase": "ready",
		"bom": pgdoc.M{"release": "0.7.0-dev", "sha256": "x"}, "clusters": []pgdoc.M{{"name": platformlife.ClusterName}}}}, nil); err != nil {
		t.Fatal(err)
	}
	if a := string(call("admin-sub", http.MethodGet, one+"/plan", "", http.StatusOK).Data); !strings.Contains(a, `"name":"controlPlane.replicas","from":"1","to":"3"`) ||
		!strings.Contains(a, "the settings place") || !strings.Contains(a, "from 1 to 3 machines there") {
		t.Fatalf("the plan of 1 to 3 without room: %s", a)
	}
	// A run handed three to the cluster (stage 7 records it before the sync): from here 3 to 1 is refused.
	if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{"status": pgdoc.M{"phase": "ready",
		"bom": pgdoc.M{"release": "0.7.0-dev", "sha256": "x"}, "clusters": []pgdoc.M{{"name": platformlife.ClusterName, "controlPlaneReplicas": 3}}}}, nil); err != nil {
		t.Fatal(err)
	}
	if a := call("admin-sub", http.MethodPut, one, replicas(1, 6), http.StatusConflict); code(a) != "fixed_fields" ||
		!strings.Contains(strings.Join(anyStrings(a.Errors.Details["problems"]), ","), "does not go from 3 machines to 1") {
		t.Fatalf("3 to 1 after a run built three: %+v", a)
	}
	// A body without the count keeps meaning one machine, so it is the same refusal, not a silent shrink.
	if a := call("admin-sub", http.MethodPut, one, platformBody("10.200.200.1", 6, 6), http.StatusConflict); code(a) != "fixed_fields" {
		t.Fatalf("3 to a body without the count: %+v", a)
	}
	call("admin-sub", http.MethodPut, one, replicas(3, 6), http.StatusOK)
	// A preset on an installed platform is refused, whatever the spec leaves out, and nothing is stored (lab round of
	// 2026-11-19, P1: it was a 200 that moved the control plane and resized every group).
	specOf := func() string {
		var v struct{ Spec json.RawMessage }
		_ = json.Unmarshal(call("admin-sub", http.MethodGet, one, "", http.StatusOK).Data, &v)
		return string(v.Spec)
	}
	noMachines := func(preset string, rev int64) string {
		var b map[string]any
		_ = json.Unmarshal([]byte(platformBody("10.200.200.1", 6, rev)), &b)
		cl := b["spec"].(map[string]any)["clusters"].([]any)[0].(map[string]any)
		delete(cl, "controlPlane")
		delete(cl, "nodeGroups")
		if preset != "" {
			b["preset"] = preset
		}
		out, _ := json.Marshal(b)
		return string(out)
	}
	stored := specOf()
	for _, body := range []string{noMachines("standard", 7), strings.Replace(platformBody("10.200.200.1", 6, 7), `"name":"lab"`, `"name":"lab","preset":"homelab"`, 1)} {
		if a := call("admin-sub", http.MethodPut, one, body, http.StatusConflict); code(a) != "fixed_fields" || !strings.Contains(a.Errors.Message, "A preset fills") {
			t.Fatalf("a preset on an installed platform: %+v", a)
		}
	}
	if now := specOf(); now != stored {
		t.Fatalf("a refused preset changed the stored spec:\n%s\n%s", stored, now)
	}
	// custom fills nothing and stays accepted; a spec without controlPlane and nodeGroups keeps the stored machines
	// (three control plane machines on pve1, one machine of 2 cores per group), never a default.
	call("admin-sub", http.MethodPut, one, noMachines("custom", 7), http.StatusOK)
	if now := specOf(); now != stored {
		t.Fatalf("a spec without machines did not keep the stored ones:\n%s\n%s", stored, now)
	}
	// The control plane's node is fixed once installed: another node is every control plane machine made anew.
	if a := call("admin-sub", http.MethodPut, one, strings.Replace(replicas(3, 8), `"controlPlane":{"replicas":3,"cores":2,"diskGiB":30,"memoryMiB":4096,"node":"pve1"`,
		`"controlPlane":{"replicas":3,"cores":2,"diskGiB":30,"memoryMiB":4096,"node":"pve2"`, 1), http.StatusConflict); code(a) != "fixed_fields" ||
		!strings.Contains(strings.Join(anyStrings(a.Errors.Details["problems"]), ","), "spec.clusters[0].controlPlane.node") {
		t.Fatalf("the control plane node after install: %+v", a)
	}
	if err := store.SaveRun(ctx, &platformlife.Run{ID: "pr-1", InstallationID: in.ID, Kind: platformlife.RunApply, State: platformlife.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if a := call("admin-sub", http.MethodPut, one, platformBody("10.200.200.1", 8, 8), http.StatusConflict); code(a) != "run_active" {
		t.Fatalf("during a run: %+v", a)
	}

	// No planted secret anywhere but sealed in platformSecret.
	var raw []pgdoc.M
	for _, col := range []string{platformlife.SecretCollection, platformlife.InstallationCollection} {
		var docs []pgdoc.M
		if err := db.C(col).Find(ctx, pgdoc.M{}, &docs); err != nil {
			t.Fatal(err)
		}
		raw = append(raw, docs...)
	}
	if stored, _ := json.Marshal(raw); strings.Contains(string(stored), "homelab") || !strings.Contains(string(stored), "pcplat0") {
		t.Fatalf("a stored installation names its preset, or lacks what the preset filled: %s", stored)
	}
	var events []pgdoc.M
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) { // the middleware writes asynchronously
		events = nil
		if err := db.C("auditEvent").Find(ctx, pgdoc.M{"resourceType": "PLATFORM_INSTALLATION"}, &events); err != nil {
			t.Fatal(err)
		}
		if len(events) >= 10 || time.Now().After(deadline) {
			break
		}
	}
	if len(events) != 10 { // two creates (the second from a preset), credentials, seven updates; the preflight and every refusal are not audited
		t.Fatalf("%d audit events, want 10: %v", len(events), events)
	}
	dump, _ := json.Marshal(map[string]any{"responses": bodies, "stored": raw, "audit": events})
	for _, s := range []string{plantedCAPI, plantedCSI, portalToken} {
		if strings.Contains(string(dump), s) {
			t.Fatalf("planted secret %s leaked: %s", s, dump)
		}
	}
	if !strings.Contains(string(dump), `"field":"spec.seed.cores"`) {
		t.Fatalf("the update events carry no field diff: %v", events)
	}
}

func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// TestPlatformSecretWritesSerialize: a credentials write waits for the installation's run lock, so one that races
// another write (or a run stage that writes the seed's identity) adds to it instead of putting back the copy it read.
func TestPlatformSecretWritesSerialize(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	store := platformlife.NewStore(db, textcrypt.New("test-key"))
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	capi := &platformlife.TokenCredential{TokenID: pve.CAPITokenID, Secret: plantedCAPI}
	csi := &platformlife.TokenCredential{TokenID: pve.CSITokenID, Secret: plantedCSI}
	done := make(chan error, 1)
	if err := db.WithTx(ctx, func(tctx context.Context) error {
		if err := db.LockXact(tctx, "platform-run:pi-1"); err != nil {
			return err
		}
		if err := store.SaveCredentials(tctx, "pi-1", nil, csi); err != nil {
			return err
		}
		go func() { done <- store.SaveCredentials(ctx, "pi-1", capi, nil) }()
		time.Sleep(300 * time.Millisecond) // the second writer reads while the first has not committed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sec, err := store.Secrets(ctx, "pi-1")
	if err != nil || sec == nil || sec.CAPI == nil || sec.CSI == nil {
		t.Fatalf("one of two racing writes was lost: %+v %v", sec, err)
	}
}

// TestPlatformRunStartCancelAndReplay drives the run routes against PostgreSQL: a run and its job are written together
// (key platform-run-<runId>, 20 attempts), a second run of the installation is refused while the first is unfinished,
// a cancel is shown as cancelling, the job is never replayed through /admin/durable-jobs, and a run whose job died is
// shown failed/job_dead and stops blocking a new run. A Defer gives the attempt back.
func TestPlatformRunStartCancelAndReplay(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	in := &platformlife.Installation{Name: "lab", ServiceID: "svc-pve"}
	if err := store.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	h.SetDurableJobs(jobs, nil, func(*durablejobs.Job) error { return nil })
	router := chi.NewRouter()
	h.Routes(router)
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	runs := "/admin/platform/installations/" + in.ID + "/runs"

	// Without the runner the start is refused; reads still answer.
	if a := call(http.MethodPost, runs, `{"kind":"apply"}`, http.StatusConflict); !strings.Contains(fmt.Sprint(a), platformlife.CodePlatformDisabled) {
		t.Fatalf("runner off: %v", a)
	}
	call(http.MethodGet, runs, "", http.StatusOK)
	h.SetPlatformRuns(jobs)

	call(http.MethodPost, runs, `{"kind":"upgrade"}`, http.StatusBadRequest)
	call(http.MethodPost, runs, `{"kind":"apply","extra":1}`, http.StatusBadRequest)
	data := call(http.MethodPost, runs, `{"kind":"apply"}`, http.StatusAccepted)["data"].(map[string]any)
	runID, _ := data["runId"].(string)
	run, err := store.Run(ctx, runID)
	if err != nil || run == nil || run.State != platformlife.RunQueued || len(run.Stages) != 12 || run.RequestedBy != "admin-sub" {
		t.Fatalf("run %+v %v", run, err)
	}
	job, err := jobs.Get(ctx, run.JobID)
	if err != nil || job.Kind != durablejobs.PlatformRun || job.Key != "platform-run-"+runID || job.MaxAttempts != 20 {
		t.Fatalf("job %+v %v", job, err)
	}
	if p, err := platformlife.DecodePayload(job.Payload); err != nil || p.RunID != runID || p.InstallationID != in.ID {
		t.Fatalf("payload %s %v", job.Payload, err)
	}
	got, _ := store.Get(ctx, in.ID)
	if got.Status.Phase != platformlife.PhaseApplying || got.LastRunID != runID {
		t.Fatalf("installation %+v", got.Status)
	}
	// One unfinished run per installation, and no spec change during it.
	if a := call(http.MethodPost, runs, `{"kind":"remove"}`, http.StatusConflict); !strings.Contains(fmt.Sprint(a), "run_active") {
		t.Fatalf("second run: %v", a)
	}
	// The job: a Defer gives back the attempt the claim took.
	j, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.PlatformRun})
	if err != nil || j == nil || j.Attempts != 1 {
		t.Fatalf("claim %+v %v", j, err)
	}
	if err := jobs.Defer(ctx, j, 0, "waiting_seed_api"); err != nil {
		t.Fatal(err)
	}
	if j, _ = jobs.Get(ctx, run.JobID); j.Attempts != 0 || j.State != "pending" {
		t.Fatalf("after Defer: %+v", j)
	}
	// Cancel: shown as cancelling until the runner honours it.
	v := call(http.MethodPost, runs+"/"+runID+"/cancel", "", http.StatusOK)["data"].(map[string]any)
	if v["state"] != "cancelling" || v["cancelRequested"] != true {
		t.Fatalf("cancel view %v", v)
	}
	// Never replayed, whatever its state.
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),owner=NULL,lease_until=NULL WHERE id=$1`, run.JobID); err != nil {
		t.Fatal(err)
	}
	if a := call(http.MethodPost, "/admin/durable-jobs/"+run.JobID+"/replay", `{"reason":"try it again"}`, http.StatusConflict); !strings.Contains(fmt.Sprint(a),
		"use Retry on the Managed services page") {
		t.Fatalf("replay: %v", a)
	}
	// A dead job: the run reads failed/job_dead and a new run may start.
	v = call(http.MethodGet, runs+"/"+runID, "", http.StatusOK)["data"].(map[string]any)
	if v["state"] != platformlife.RunFailed || v["code"] != platformlife.CodeJobDead {
		t.Fatalf("dead job view %v", v)
	}
	call(http.MethodPost, runs, `{"kind":"remove"}`, http.StatusAccepted)
	if old, _ := store.Run(ctx, runID); old.State != platformlife.RunFailed || old.Code != platformlife.CodeJobDead || old.FinishedAt == nil {
		t.Fatalf("old run %+v", old)
	}
	list := call(http.MethodGet, runs, "", http.StatusOK)["data"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["kind"] != platformlife.RunRemove {
		t.Fatalf("runs %v", list)
	}
	// Cancel on a run whose job died: the run ends failed/job_dead and the installation failed, as after any other run
	// that did not succeed, not left removing with no run.
	second, _ := store.Run(ctx, list[0].(map[string]any)["id"].(string))
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),owner=NULL,lease_until=NULL WHERE id=$1`, second.JobID); err != nil {
		t.Fatal(err)
	}
	v = call(http.MethodPost, runs+"/"+second.ID+"/cancel", "", http.StatusOK)["data"].(map[string]any)
	if got, _ := store.Get(ctx, in.ID); v["state"] != platformlife.RunFailed || v["code"] != platformlife.CodeJobDead || got.Status.Phase != platformlife.PhaseFailed {
		t.Fatalf("cancel on a dead job: view %v, phase %s", v, got.Status.Phase)
	}
}

// A platform the release v0.8.0 installed, on this build: the plan stays on the installed BOM and names this build's
// own as newer, and an apply starts with the 0.8.0 BOM. A platform a build of main installed (a BOM no release
// embeds) is still refused with bom_unsupported, and can still be removed.
func TestPlatformRunOnAPreviousBOM(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	h.SetDurableJobs(jobs, nil, func(*durablejobs.Job) error { return nil })
	h.SetPlatformRuns(jobs)
	router := chi.NewRouter()
	h.Routes(router)
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	installed := func(service, release, sha string) string {
		t.Helper()
		in := &platformlife.Installation{Name: "lab", ServiceID: service}
		if err := store.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{"status": pgdoc.M{"phase": "ready",
			"bom": pgdoc.M{"release": release, "sha256": sha}}}, nil); err != nil {
			t.Fatal(err)
		}
		return "/admin/platform/installations/" + in.ID
	}
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	const sha080 = "8eb874987d3bbaf55f309295ee241ec320c0cf9e0805b3bd1812ef908b6600c6" // status.bom of a platform v0.8.0 installed
	if cur.SHA256 == sha080 {
		t.Fatal("this build's own BOM is the 0.8.0 one")
	}

	one := installed("svc-pve", "0.8.0", sha080)
	plan := call(http.MethodGet, one+"/plan", "", http.StatusOK)["data"].(map[string]any)
	target, _ := plan["target"].(map[string]any)
	newer, _ := plan["newer"].(map[string]any)
	if target["sha256"] != sha080 || newer["sha256"] != cur.SHA256 || newer["release"] != cur.Release ||
		len(plan["changes"].([]any)) != 0 || len(plan["blocking"].([]any)) != 0 {
		t.Fatalf("plan of a 0.8.0 platform: %v", plan)
	}
	if b := call(http.MethodGet, "/admin/platform/bom?sha256="+sha080, "", http.StatusOK)["data"].(map[string]any); b["release"] != "0.8.0" {
		t.Fatalf("the installed BOM: %v", b["release"])
	}
	data := call(http.MethodPost, one+"/runs", `{"kind":"apply"}`, http.StatusAccepted)["data"].(map[string]any)
	if run, err := store.Run(ctx, data["runId"].(string)); err != nil || run.BOM != (platformlife.BOMRef{Release: "0.8.0", SHA256: sha080}) {
		t.Fatalf("the apply does not run with the installed BOM: %+v %v", run, err)
	}

	dev := installed("svc-pve-2", "0.8.0-dev", strings.Repeat("1", 64))
	plan = call(http.MethodGet, dev+"/plan", "", http.StatusOK)["data"].(map[string]any)
	if blocking := fmt.Sprint(plan["blocking"]); !strings.Contains(blocking, platformlife.CodeBOMUnsupported) || plan["newer"] != nil {
		t.Fatalf("plan of a platform a build of main installed: %v", plan)
	}
	if a := call(http.MethodPost, dev+"/runs", `{"kind":"apply"}`, http.StatusConflict); !strings.Contains(fmt.Sprint(a), platformlife.CodeBOMUnsupported) {
		t.Fatalf("apply on an unknown BOM: %v", a)
	}
	call(http.MethodGet, "/admin/platform/bom?sha256="+strings.Repeat("1", 64), "", http.StatusNotFound)
	call(http.MethodPost, dev+"/runs", `{"kind":"remove"}`, http.StatusAccepted)

	// The move to this build's BOM. A development BOM (main's) is no target: the plan says so and the run is refused
	// with the same reasons. Another BOM than this build's is not a target at all.
	up := installed("svc-pve-3", "0.8.0", sha080)
	id := strings.TrimPrefix(up, "/admin/platform/installations/")
	apply := fmt.Sprintf(`{"kind":"apply","targetBom":%q,"confirm":"upgrade lab to 0.9.0"}`, cur.SHA256)
	if strings.HasSuffix(cur.Release, "-dev") {
		upgrade, _ := call(http.MethodGet, up+"/plan", "", http.StatusOK)["data"].(map[string]any)["upgrade"].(map[string]any)
		if upgrade["allowed"] != false || !strings.Contains(fmt.Sprint(upgrade["blocking"]), "development BOM") {
			t.Fatalf("the plan of a move to a development BOM: %v", upgrade)
		}
		if a := fmt.Sprint(call(http.MethodPost, up+"/runs", apply, http.StatusConflict)); !strings.Contains(a, platformlife.CodeUpgradeRefused) || !strings.Contains(a, "development BOM") {
			t.Fatalf("an upgrade to a development BOM: %v", a)
		}
	}
	call(http.MethodGet, up+"/plan?target="+strings.Repeat("3", 64), "", http.StatusBadRequest)
	call(http.MethodPost, up+"/runs", fmt.Sprintf(`{"kind":"apply","targetBom":%q}`, strings.Repeat("3", 64)), http.StatusBadRequest)

	// The same build as a release: 0.8.0 and main differ in the release line and the product's own charts, each of which
	// lists the installed version in upgradeFrom, so the move is allowed: the release, both tenantCharts, and the
	// charts of capi, pc-platform, capi-tenant and platform. It wants the typed confirmation, records the target on
	// the installation and where it comes from on the run.
	release := cur.Release
	cur.Release = "0.9.0"
	defer func() { cur.Release = release }()
	upgrade, _ := call(http.MethodGet, up+"/plan?target="+cur.SHA256, "", http.StatusOK)["data"].(map[string]any)["upgrade"].(map[string]any)
	if upgrade["allowed"] != true || upgrade["confirm"] != "upgrade lab to 0.9.0" || len(upgrade["changes"].([]any)) != 7 || len(upgrade["blocking"].([]any)) != 0 {
		t.Fatalf("the plan of the move: %v", upgrade)
	}
	call(http.MethodPost, up+"/runs", fmt.Sprintf(`{"kind":"apply","targetBom":%q}`, cur.SHA256), http.StatusBadRequest)
	from, to := platformlife.BOMRef{Release: "0.8.0", SHA256: sha080}, platformlife.BOMRef{Release: "0.9.0", SHA256: cur.SHA256}
	fail := func(runID any) {
		t.Helper()
		if err := store.Mutate(ctx, runID.(string), func(r *platformlife.Run, in *platformlife.Installation, _ *platformlife.Secrets) error {
			r.State, in.Status.Phase = platformlife.RunFailed, platformlife.PhaseFailed
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	data = call(http.MethodPost, up+"/runs", apply, http.StatusAccepted)["data"].(map[string]any)
	run, _ := store.Run(ctx, data["runId"].(string))
	in, _ := store.Get(ctx, id)
	if run.BOM != to || run.FromBOM == nil || *run.FromBOM != from || !run.Confirmed || in.Status.TargetBOM == nil || *in.Status.TargetBOM != to || *in.Status.BOM != from {
		t.Fatalf("the upgrade's run %+v, status %+v", run, in.Status)
	}
	// It failed. Retry is an apply without a target: it continues to the target. The way back is refused.
	fail(data["runId"])
	data = call(http.MethodPost, up+"/runs", `{"kind":"apply"}`, http.StatusAccepted)["data"].(map[string]any)
	if run, _ := store.Run(ctx, data["runId"].(string)); run.BOM != to || run.FromBOM == nil || *run.FromBOM != from {
		t.Fatalf("Retry of a failed upgrade runs with %+v", run)
	}
	fail(data["runId"])
	if a := fmt.Sprint(call(http.MethodPost, up+"/runs", fmt.Sprintf(`{"kind":"apply","targetBom":%q}`, sha080), http.StatusConflict)); !strings.Contains(a, platformlife.CodeUpgradeRefused) {
		t.Fatalf("back to the installed BOM: %v", a)
	}
	plan = call(http.MethodGet, up+"/plan", "", http.StatusOK)["data"].(map[string]any)
	if target, _ := plan["target"].(map[string]any); target["sha256"] != cur.SHA256 || plan["upgrade"].(map[string]any)["inProgress"] != true {
		t.Fatalf("the plan of a started upgrade: %v", plan)
	}
}

// TestPlatformProbeEndsWithThePlatform: the stored tenant path test stays over an apply of an installed platform, is
// deleted when a Remove wipes the status, and an install starts without one (a verdict saved after the Remove, or left
// by a release that kept it; lab round 3 of v0.9.0, R3-2: a fresh platform answered the removed one's verdict).
func TestPlatformProbeEndsWithThePlatform(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, textcrypt.New("test-key"))
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	in := &platformlife.Installation{Name: "lab", ServiceID: "svc-pve"}
	if err := store.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{"status": pgdoc.M{"phase": "ready",
		"bom": pgdoc.M{"release": cur.Release, "sha256": cur.SHA256}}}, nil); err != nil {
		t.Fatal(err)
	}
	save := func() {
		t.Helper()
		if err := store.SaveProbe(ctx, &platformlife.TenantProbe{ID: in.ID, State: platformlife.ProbeFailed, Node: "pve1"}); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() bool {
		t.Helper()
		p, err := store.Probe(ctx, in.ID)
		if err != nil {
			t.Fatal(err)
		}
		return p != nil
	}
	// run starts a run of kind and ends it; wipe is what the Remove's last stage does to the status.
	run := func(kind string, wipe bool) {
		t.Helper()
		r, err := store.StartRun(ctx, jobs, in.ID, kind, "admin-sub", "", "", "", platformlife.SeedBackup{}, platformlife.RollRead{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Mutate(ctx, r.ID, func(r *platformlife.Run, in *platformlife.Installation, _ *platformlife.Secrets) error {
			r.State = platformlife.RunSucceeded
			if wipe {
				in.Status = platformlife.Status{Phase: platformlife.PhaseRemoved}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	save()
	if run(platformlife.RunApply, false); !stored() {
		t.Fatal("an apply of an installed platform deleted its tenant path test")
	}
	if run(platformlife.RunRemove, false); !stored() {
		t.Fatal("a Remove that has not wiped the status deleted the tenant path test")
	}
	if run(platformlife.RunRemove, true); stored() {
		t.Fatal("the tenant path test outlives the Remove")
	}
	save() // a test that was still running saved its verdict after the Remove
	if run(platformlife.RunApply, false); stored() {
		t.Fatal("an install starts with the removed platform's tenant path test")
	}
}

// TestPlatformRunMutate: the runner's write seals the secrets, deletes a secrets document left empty, and keeps the
// run and the installation together.
func TestPlatformRunMutate(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, textcrypt.New("test-key"))
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	in := &platformlife.Installation{Name: "lab", ServiceID: "svc-pve"}
	if err := store.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunApply, "admin-sub", "", "", "", platformlife.SeedBackup{}, platformlife.RollRead{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Mutate(ctx, run.ID, func(r *platformlife.Run, in *platformlife.Installation, sec *platformlife.Secrets) error {
		r.State, sec.BootstrapToken = platformlife.RunRunning, "abcdef.0123456789abcdef"
		in.Status.Seed = &platformlife.SeedStatus{VMID: 120}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var raw []pgdoc.M
	if err := db.C(platformlife.SecretCollection).Find(ctx, pgdoc.M{}, &raw); err != nil || len(raw) != 1 || strings.Contains(fmt.Sprint(raw), "0123456789abcdef") {
		t.Fatalf("secrets %v %v", raw, err)
	}
	r, got, sec, err := store.Load(ctx, run.ID)
	if err != nil || r.State != platformlife.RunRunning || got.Status.Seed.VMID != 120 || sec.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("load %+v %+v %+v %v", r, got, sec, err)
	}
	if err := store.Mutate(ctx, run.ID, func(_ *platformlife.Run, _ *platformlife.Installation, sec *platformlife.Secrets) error {
		*sec = platformlife.Secrets{ID: sec.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.C(platformlife.SecretCollection).Count(ctx, pgdoc.M{}); n != 0 {
		t.Fatalf("%d secrets documents after a wipe", n)
	}

	// A rotated seed credential is stored sealed, only outside a run and only over the token Secret it replaces.
	seeded := func(r *platformlife.Run, in *platformlife.Installation, sec *platformlife.Secrets) error {
		r.State = platformlife.RunSucceeded
		in.Status.Seed = &platformlife.SeedStatus{VMID: 120, TokenSecret: "proxcloud-portal-token-g1"}
		sec.SeedKubeconfig = &platformlife.SeedKubeconfig{Server: "https://10.0.0.1:6443", CA: "ca", Token: "old-token-0123456789"}
		return nil
	}
	if err := store.SetSeedToken(ctx, in.ID, "proxcloud-portal-token-g1", "proxcloud-portal-token-g2", "new-token-0123456789"); !errors.Is(err, platformlife.ErrRunActive) {
		t.Fatalf("rotation during a run: %v", err)
	}
	if err := store.Mutate(ctx, run.ID, seeded); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSeedToken(ctx, in.ID, "proxcloud-portal-token-g7", "proxcloud-portal-token-g8", "x"); !errors.Is(err, platformlife.ErrRevConflict) {
		t.Fatalf("rotation over another Secret: %v", err)
	}
	if err := store.SetSeedToken(ctx, in.ID, "proxcloud-portal-token-g1", "proxcloud-portal-token-g2", "new-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	raw = nil
	_ = db.C(platformlife.SecretCollection).Find(ctx, pgdoc.M{}, &raw)
	got, _ = store.Get(ctx, in.ID)
	if sec, _ := store.Secrets(ctx, in.ID); sec.SeedKubeconfig.Token != "new-token-0123456789" || got.Status.Seed.TokenSecret != "proxcloud-portal-token-g2" ||
		strings.Contains(fmt.Sprint(raw), "new-token") {
		t.Fatalf("stored %+v, raw %v", got.Status.Seed, raw)
	}
	// The typed confirmation belongs to a remove and must name the installation.
	if _, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunApply, "admin-sub", "remove lab", "", "", platformlife.SeedBackup{}, platformlife.RollRead{}); !errors.Is(err, platformlife.ErrBadConfirm) {
		t.Fatalf("confirm on an apply: %v", err)
	}
	if _, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunRemove, "admin-sub", "remove other", "", "", platformlife.SeedBackup{}, platformlife.RollRead{}); !errors.Is(err, platformlife.ErrBadConfirm) {
		t.Fatalf("a wrong confirmation: %v", err)
	}
	if r, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunRemove, "admin-sub", "remove lab", "", "", platformlife.SeedBackup{}, platformlife.RollRead{}); err != nil || !r.Confirmed {
		t.Fatalf("confirmed remove: %+v %v", r, err)
	}
}

// TestPlatformRegisterAndDisable runs the code that writes provider rows for a platform run against PostgreSQL: a row
// is created with its kubeconfig sealed, adopted by marker at the next call (name and status left alone, the owned
// keys and the kubeconfig rewritten, still sealed), a foreign row under the name and a body the validation refuses
// are ErrProviderConflict, racing calls create one row, and Disable touches only the installation's rows.
func TestPlatformRegisterAndDisable(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	repo := admin.NewRepo(db)
	h := admin.NewHandler(repo, nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	// An installation always has its Proxmox provider row.
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "proxmox", "region": "lab"}})
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	bodies := func(id, name, kubeconfig string) (k8s, database map[string]any) {
		return platformlife.ProviderBodies(cur.BOM, platformlife.RenderInput{ID: id, Name: name, Region: "lab", RegionName: "Lab", PVEAPIURL: "https://10.200.200.2:8006"}, kubeconfig)
	}
	row := func(id string) pgdoc.M {
		t.Helper()
		rows, err := repo.ListRaw(ctx, "externalService")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r["_id"] == id {
				return r
			}
		}
		t.Fatalf("no row %s", id)
		return nil
	}
	sealed := func(r pgdoc.M, plain string) bool {
		stored, _ := r["secret"].(map[string]any)["kubeconfig"].(string)
		return stored != "" && !strings.Contains(stored, plain) && enc.Decrypt(stored) == plain
	}

	k8s, database := bodies("pi-1", "lab", "kubeconfig-one-0123456789")
	id, err := h.PlatformRegister(ctx, k8s)
	if err != nil || id == "" {
		t.Fatalf("create: %q %v", id, err)
	}
	if r := row(id); r["name"] != "lab Kubernetes" || r["status"] != "PUBLIC" || !sealed(r, "kubeconfig-one-0123456789") {
		t.Fatalf("created row: %v", r)
	}
	// The operator renames and hides the row; a chart version from an older BOM is on it.
	r := row(id)
	r["name"], r["status"] = "renamed", "PRIVATE"
	r["config"].(map[string]any)["argocd"].(map[string]any)["chartVersion"] = "0.0.1"
	if err := repo.ReplaceDoc(ctx, "externalService", id, r); err != nil {
		t.Fatal(err)
	}
	k8s, _ = bodies("pi-1", "lab", "kubeconfig-two-0123456789")
	again, err := h.PlatformRegister(ctx, k8s)
	r = row(id)
	if err != nil || again != id || r["name"] != "renamed" || r["status"] != "PRIVATE" || !sealed(r, "kubeconfig-two-0123456789") ||
		r["config"].(map[string]any)["argocd"].(map[string]any)["chartVersion"] != cur.TenantCharts.Kubernetes.Version {
		t.Fatalf("adopted row: %q %v %v", again, err, r)
	}

	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-foreign", "name": "lab Databases", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{"provider": "dbaas"}})
	if _, err := h.PlatformRegister(ctx, database); !errors.Is(err, platformlife.ErrProviderConflict) {
		t.Fatalf("a foreign row under the name: %v", err)
	}
	// A refusal of the provider validation is a conflict too (a run fails on it instead of spending its attempts).
	refused, _ := bodies("pi-2", "", "kubeconfig-0123456789")
	refused["name"] = ""
	if _, err := h.PlatformRegister(ctx, refused); !errors.Is(err, platformlife.ErrProviderConflict) {
		t.Fatalf("a body the validation refuses: %v", err)
	}

	// Two runners of one installation (one that lost its lease while paused, and its successor): the second waits for
	// the first under the installation's run lock and adopts its row, instead of listing none and creating another.
	racing, _ := bodies("pi-race", "race", "kubeconfig-0123456789")
	second := make(chan string, 1)
	var first string
	if err := db.WithTx(ctx, func(tctx context.Context) error {
		var err error
		if first, err = h.PlatformRegister(tctx, racing); err != nil {
			return err
		}
		go func() {
			body, _ := bodies("pi-race", "race", "kubeconfig-0123456789")
			id, err := h.PlatformRegister(ctx, body)
			if err != nil {
				id = "error: " + err.Error()
			}
			second <- id
		}()
		time.Sleep(300 * time.Millisecond) // the second lists while the first has not committed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-second; got != first {
		t.Fatalf("racing registrations answered %q and %q", first, got)
	}

	if err := h.PlatformDisable(ctx, "pi-1"); err != nil {
		t.Fatal(err)
	}
	if row(id)["status"] != "DISABLED" || row(first)["status"] != "PUBLIC" || row("svc-foreign")["status"] != "PUBLIC" || !sealed(row(id), "kubeconfig-two-0123456789") {
		t.Fatalf("after Disable: %v %v %v", row(id), row(first), row("svc-foreign"))
	}
}

// TestPlatformClusterKubeconfig drives GET …/platform-kubeconfig against PostgreSQL with the seed and the platform
// cluster on fakes: SUPPORT is refused before anything is asked of a cluster; ADMIN gets a kubeconfig whose token is the
// one-hour token of the break-glass account, not the admin credential the seed holds; the audit log gets one
// BREAK_GLASS row with the operation and the same expiry and no token; a cluster that grants a longer life, or an
// installation without a cluster, gets no kubeconfig and no row; neither does a call whose audit row cannot be written.
func TestPlatformClusterKubeconfig(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	const seedToken, adminToken, minted = "planted-seed-token-0123456789", "planted-admin-credential-0123456789", "minted-break-glass-token-0123456789"
	pemOf := func(s *httptest.Server) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	}
	var mu sync.Mutex
	var wlCalls []string
	life := time.Hour
	wl := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+adminToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		wlCalls = append(wlCalls, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/token") {
			var req struct {
				Spec struct {
					ExpirationSeconds int64 `json:"expirationSeconds"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Spec.ExpirationSeconds != 3600 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"status":{"token":%q,"expirationTimestamp":%q}}`, minted, time.Now().Add(life).UTC().Format(time.RFC3339))
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(wl.Close)
	adminKubeconfig, err := kamajik8s.BuildKubeconfig("pc-platform", wl.URL, []byte(pemOf(wl)), adminToken)
	if err != nil {
		t.Fatal(err)
	}
	hasCluster := true
	seed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
		switch {
		case r.Header.Get("Authorization") != "Bearer "+seedToken || r.Method != http.MethodGet:
			w.WriteHeader(http.StatusUnauthorized)
		case !hasCluster:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/v1/namespaces/pc-platform/secrets/pc-platform-kubeconfig":
			fmt.Fprintf(w, `{"data":{"value":%q}}`, b64(adminKubeconfig))
		case r.URL.Path == "/api/v1/namespaces/pc-platform/secrets/pc-platform-ca":
			fmt.Fprintf(w, `{"data":{"tls.crt":%q}}`, b64(pemOf(wl)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(seed.Close)

	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, textcrypt.New("test-key"))
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	in := &platformlife.Installation{Name: "lab", ServiceID: "svc-pve"}
	if err := store.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunApply, "admin-sub", "", "", "", platformlife.SeedBackup{}, platformlife.RollRead{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Mutate(ctx, run.ID, func(_ *platformlife.Run, in *platformlife.Installation, sec *platformlife.Secrets) error {
		in.Status.Seed = &platformlife.SeedStatus{VMID: 120, APIServer: seed.URL, CACert: pemOf(seed)}
		sec.SeedKubeconfig = &platformlife.SeedKubeconfig{Server: seed.URL, CA: pemOf(seed), Token: seedToken}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key")), "", "", "")
	h.SetPlatform(store)
	router := chi.NewRouter()
	h.Routes(router)
	get := func(sub string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/platform/installations/"+in.ID+"/platform-kubeconfig", nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	rows := func() []pgdoc.M {
		var events []pgdoc.M
		if err := db.C("auditEvent").Find(ctx, pgdoc.M{"action": admin.ActionBreakGlass}, &events); err != nil {
			t.Fatal(err)
		}
		return events
	}

	if w := get("support-sub"); w.Code != http.StatusForbidden || len(wlCalls) != 0 || len(rows()) != 0 {
		t.Fatalf("SUPPORT: %d %s, platform cluster calls %v", w.Code, w.Body.String(), wlCalls)
	}

	w := get("admin-sub")
	var a struct {
		Data struct {
			Kubeconfig string    `json:"kubeconfig"`
			ExpiresAt  time.Time `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ADMIN: %d %q %s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	if left := time.Until(a.Data.ExpiresAt); left > time.Hour || left < 59*time.Minute {
		t.Fatalf("expires in %s, want one hour", left)
	}
	if kc := a.Data.Kubeconfig; !strings.Contains(kc, "token: "+minted) || !strings.Contains(kc, wl.URL) ||
		strings.Contains(w.Body.String(), adminToken) || strings.Contains(w.Body.String(), seedToken) {
		t.Fatalf("the kubeconfig is not the break-glass account's on the platform cluster: %s", w.Body.String())
	}
	if last := wlCalls[len(wlCalls)-1]; last != "POST /api/v1/namespaces/kube-system/serviceaccounts/proxcloud-break-glass/token" {
		t.Fatalf("the token is not the break-glass account's: %v", wlCalls)
	}
	events := rows()
	raw, _ := json.Marshal(events)
	if len(events) != 1 || !strings.Contains(string(raw), `"operation":"platform-kubeconfig"`) || !strings.Contains(string(raw), "admin-sub") ||
		!strings.Contains(string(raw), in.ID) || !strings.Contains(string(raw), `"expiresAt":"`+a.Data.ExpiresAt.UTC().Format(time.RFC3339)+`"`) ||
		strings.Contains(string(raw), minted) || strings.Contains(string(raw), adminToken) {
		t.Fatalf("audit rows: %s", raw)
	}

	// A platform cluster that grants a day: 502, no kubeconfig, no new row.
	mu.Lock()
	life = 24 * time.Hour
	mu.Unlock()
	if w := get("admin-sub"); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), platformlife.CodeWorkloadUnreachable) ||
		strings.Contains(w.Body.String(), minted) || len(rows()) != 1 {
		t.Fatalf("a 24 hour token: %d %s", w.Code, w.Body.String())
	}
	// No cluster on the seed: 409 no_cluster.
	mu.Lock()
	hasCluster = false
	mu.Unlock()
	if w := get("admin-sub"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_cluster") || len(rows()) != 1 {
		t.Fatalf("no cluster: %d %s", w.Code, w.Body.String())
	}
	// An audit entry that cannot be written: the token was asked for, and the kubeconfig still stays in.
	mu.Lock()
	life, hasCluster = time.Hour, true
	mu.Unlock()
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION audit_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$; CREATE TRIGGER audit_test_fail BEFORE INSERT ON "auditEvent" FOR EACH ROW EXECUTE FUNCTION audit_test_fail()`); err != nil {
		t.Fatal(err)
	}
	if w := get("admin-sub"); w.Code < 500 || strings.Contains(w.Body.String(), minted) || strings.Contains(w.Body.String(), `"kubeconfig"`) || len(rows()) != 1 {
		t.Fatalf("audit insert fails: %d %s", w.Code, w.Body.String())
	}
}

// An upgrade that replaces the seed's k3s needs a backup of the seed VM: the plan carries the seed-backup row and
// blocks with seed_backup_missing while the portal sees none younger than 24 hours, and the apply answers the same 409.
// The fake Proxmox lists a VM's backups as the real one does: only to a token that may read them. With the product's
// grants the portal token is shown none of the seed's, so the row says that it cannot see them (not that none exist),
// today's archive changes nothing, and the operator's seedBackupTaken starts the upgrade, which the run records.
// Where the token manages the store, a fresh archive is seen and the apply starts without the operator's word.
func TestPlatformUpgradeNeedsASeedBackup(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	site := &pvetest.BackupSite{ACL: pvetest.ProductACL("proxcloud@pve!platform", pve.CAPITokenID, 120, "local-lvm", "local", "", "pbs", "nas"),
		Stores: map[string]string{"local-lvm": "images", "local": "iso,vztmpl", "pbs": "backup", "nas": "backup,iso"}}
	archive := func(age time.Duration) { // the one archive of VM 120 on store pbs
		site.Mu.Lock()
		defer site.Mu.Unlock()
		site.Backups = map[string][]pvetest.Backup{"pbs": {{VMID: 120, CTime: time.Now().Add(-age).Unix()}}}
	}
	fakePVE := httptest.NewServer(site)
	t.Cleanup(fakePVE.Close)
	for _, id := range []string{"svc-pve", "svc-pve-2", "svc-pve-3"} { // a provider has one installation
		mustInsert(t, db, "externalService", pgdoc.M{"_id": id, "name": id, "type": "CLOUD", "status": "PUBLIC",
			"config": pgdoc.M{"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform"},
			"secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	}
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	h.SetDurableJobs(jobs, nil, func(*durablejobs.Job) error { return nil })
	h.SetPlatformRuns(jobs)
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		c, err := pve.New(es.PVEConfig())
		return &pve.Service{C: c, ServiceID: es.ID}, err
	})
	router := chi.NewRouter()
	h.Routes(router)
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	// This build as a release whose BOM is that of 0.8.0 with a newer k3s: the only kind of pair that
	// replaces the seed's k3s (no BOM on main names another k3s yet).
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	old := bom080(t)
	saved := *cur.BOM
	defer func() { *cur.BOM = saved }()
	*cur.BOM = *old.BOM
	cur.Release = "0.99.0"
	cur.Seed.K3s.Version = "v1.35.11+k3s1"
	installed := func(service string) string {
		t.Helper()
		in := &platformlife.Installation{Name: "lab", ServiceID: service}
		if err := store.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{"status": pgdoc.M{"phase": "ready",
			"bom": pgdoc.M{"release": old.Release, "sha256": old.SHA256}, "seed": pgdoc.M{"vmid": 120, "node": "pve1"}}}, nil); err != nil {
			t.Fatal(err)
		}
		return "/admin/platform/installations/" + in.ID
	}
	const sentence = "seed_backup_missing: the portal saw no backup of seed VM 120 younger than 24 hours, and on most stores it cannot see one (the seed-backup row says where). Check in Proxmox that there is an archive of this seed VM, made after the seed VM was created and younger than 24 hours (vzdump 120 makes one), then confirm it (seedBackupTaken)."
	apply := func(more string) string {
		return fmt.Sprintf(`{"kind":"apply","targetBom":%q,"confirm":"upgrade lab to 0.99.0"%s}`, cur.SHA256, more)
	}
	upgradeOf := func(path string) map[string]any {
		t.Helper()
		u, _ := call(http.MethodGet, path+"/plan", "", http.StatusOK)["data"].(map[string]any)["upgrade"].(map[string]any)
		return u
	}

	// No archive: the plan blocks and says where the portal cannot see; the apply is refused with the same sentence.
	one := installed("svc-pve")
	blind := func(what string) {
		t.Helper()
		u := upgradeOf(one)
		row, _ := u["seedBackup"].(map[string]any)
		if msg := fmt.Sprint(row["message"]); u["allowed"] != true || fmt.Sprint(u["blocking"]) != "["+sentence+"]" || row["status"] != "warn" ||
			!strings.Contains(msg, "The portal cannot see backups of seed VM 120 on stores nas, pbs of node pve1") || strings.Contains(msg, "hold none") {
			t.Fatalf("the plan %s: %v", what, u)
		}
		a := call(http.MethodPost, one+"/runs", apply(""), http.StatusConflict)
		if errs, _ := a["errors"].(map[string]any); errs["message"] != sentence || fmt.Sprint(errs["details"]) != "map[code:seed_backup_missing]" {
			t.Fatalf("the apply %s: %v", what, a)
		}
	}
	blind("without a backup")
	// Today's archive on a store whose backups of the seed Proxmox does not show the portal token: the same answer.
	archive(time.Hour)
	blind("with an archive Proxmox does not list to the portal token")
	// The operator's word starts it, and the run records it.
	data := call(http.MethodPost, one+"/runs", apply(`,"seedBackupTaken":true`), http.StatusAccepted)["data"].(map[string]any)
	if run, err := store.Run(ctx, data["runId"].(string)); err != nil || !run.SeedBackupTaken {
		t.Fatalf("the run of an apply with seedBackupTaken: %+v %v", run, err)
	}
	// Started: the plan asks for nothing more.
	if u := upgradeOf(one); u["inProgress"] != true || u["seedBackup"] != nil || len(u["blocking"].([]any)) != 0 {
		t.Fatalf("the plan of the started upgrade: %v", u)
	}

	// The portal token manages the store (as on the import store): an archive of two days ago is none, a fresh one
	// is seen, the plan is clear and the apply starts without the operator's word.
	site.Mu.Lock()
	site.ACL["proxcloud@pve!platform"]["/storage/pbs"] = []string{"Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.Audit"}
	site.Mu.Unlock()
	archive(48 * time.Hour)
	three := installed("svc-pve-3")
	if row, _ := upgradeOf(three)["seedBackup"].(map[string]any); row["status"] != "warn" || !strings.Contains(fmt.Sprint(row["message"]), "The newest it sees, on store pbs, is of ") {
		t.Fatalf("the plan with an old backup: %v", row)
	}
	call(http.MethodPost, three+"/runs", apply(""), http.StatusConflict)
	archive(time.Hour)
	two := installed("svc-pve-2")
	u := upgradeOf(two)
	if row, _ := u["seedBackup"].(map[string]any); len(u["blocking"].([]any)) != 0 || row["status"] != "ok" || !strings.Contains(fmt.Sprint(row["message"]), "on store pbs") {
		t.Fatalf("the plan with a fresh backup: %v", u)
	}
	data = call(http.MethodPost, two+"/runs", apply(""), http.StatusAccepted)["data"].(map[string]any)
	if run, err := store.Run(ctx, data["runId"].(string)); err != nil || run.SeedBackupTaken || run.FromBOM == nil {
		t.Fatalf("the run of an apply with a backup the portal saw: %+v %v", run, err)
	}
}

// An upgrade that replaces the platform cluster's machines has two parts. The plan lists them with their phrases and
// blocks the platform part with the pre-checks the site fails; POST …/runs refuses an unknown part (400 bad_part), the
// platform part with the first failed pre-check's code (409), a wrong phrase (400), and starts the seed part, whose
// run stops after the seed's own charts.
func TestPlatformUpgradeThatReplacesMachines(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	fakePVE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) }))
	t.Cleanup(fakePVE.Close)
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "svc-pve", "type": "CLOUD", "status": "PUBLIC",
		"config": pgdoc.M{"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform"},
		"secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	h.SetDurableJobs(jobs, nil, func(*durablejobs.Job) error { return nil })
	h.SetPlatformRuns(jobs)
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		c, err := pve.New(es.PVEConfig())
		return &pve.Service{C: c, ServiceID: es.ID}, err
	})
	router := chi.NewRouter()
	h.Routes(router)
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	code := func(a map[string]any) string {
		errs, _ := a["errors"].(map[string]any)
		d, _ := errs["details"].(map[string]any)
		return fmt.Sprint(d["code"])
	}

	// This build as a release whose BOM is that of 0.8.0 with the next Kubernetes patch, its node image and a
	// cluster chart that can order a rollout: no BOM on main names another Kubernetes yet.
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	old := bom080(t)
	saved := *cur.BOM
	defer func() { *cur.BOM = saved }()
	*cur.BOM = *old.BOM
	cur.Release, cur.Components = "0.99.0", slices.Clone(old.Components)
	cur.Kubernetes.Version = "1.35.11"
	cur.Kubernetes.NodeImage.Tag, cur.Kubernetes.NodeImage.Digest = "ubuntu-2604-k8s-1.35.11-20270101000000-000000000000", "sha256:"+strings.Repeat("e", 64)
	for i := range cur.Components {
		if c := &cur.Components[i]; c.Chart.Name == "proxmox-kamaji-cluster" {
			c.UpgradeFrom = []string{c.Chart.Version}
			c.Chart.Version, c.Chart.Digest = "0.1.15", "sha256:"+strings.Repeat("d", 64)
		}
	}
	in := &platformlife.Installation{Name: "lab", ServiceID: "svc-pve"}
	if err := store.Create(ctx, in); err != nil {
		t.Fatal(err)
	}
	machine := pgdoc.M{"node": "pve1", "cores": 2, "memoryMiB": 4096, "diskGiB": 30}
	group := func(name string) pgdoc.M {
		return pgdoc.M{"name": name, "nodes": []string{"pve1"}, "count": 1, "cores": 2, "memoryMiB": 4096, "diskGiB": 30}
	}
	if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, in.ID, pgdoc.M{
		"spec": pgdoc.M{"nodeImage": pgdoc.M{"mode": "bom"}, "storage": pgdoc.M{"boot": "local-lvm"},
			"clusters": []pgdoc.M{{"controlPlane": machine, "nodeGroups": []pgdoc.M{group("system"), group("kamaji"), group("dbaas")}}}},
		"status": pgdoc.M{"phase": "ready", "bom": pgdoc.M{"release": old.Release, "sha256": old.SHA256}, "seed": pgdoc.M{"vmid": 120, "node": "pve1"},
			"clusters": []pgdoc.M{{"name": "pc-platform", "kubernetesVersion": old.Kubernetes.Version, "templates": pgdoc.M{"pve1": 9101}}}}}, nil); err != nil {
		t.Fatal(err)
	}
	path := "/admin/platform/installations/" + in.ID
	apply := func(confirm, part string) string {
		return fmt.Sprintf(`{"kind":"apply","targetBom":%q,"confirm":%q,"part":%q}`, cur.SHA256, confirm, part)
	}

	u, _ := call(http.MethodGet, path+"/plan", "", http.StatusOK)["data"].(map[string]any)["upgrade"].(map[string]any)
	parts, _ := u["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("the plan has no parts: %v", u)
	}
	seed, platform := parts[0].(map[string]any), parts[1].(map[string]any)
	blocking := fmt.Sprint(platform["blocking"])
	if seed["allowed"] != true || seed["confirm"] != "upgrade lab to 0.99.0" || platform["allowed"] != false || u["allowed"] != false ||
		!strings.Contains(blocking, "node_image_missing: ") || !strings.Contains(blocking, "drain_blocked: ") || !strings.Contains(blocking, "capacity_short: ") ||
		u["changes"].([]any)[0].(map[string]any)["name"] != "roll" {
		t.Fatalf("the plan: seed %v, platform %v", seed, platform)
	}
	if a := call(http.MethodPost, path+"/runs", apply("upgrade lab to 0.99.0", "platform"), http.StatusBadRequest); code(a) != platformlife.CodeBadPart {
		t.Fatalf("an unknown part: %v", a)
	}
	// The platform part: the plan's read again, refused with the first pre-check's code before the phrase is looked at.
	if a := call(http.MethodPost, path+"/runs", apply("upgrade lab to 0.99.0 and replace its machines", ""), http.StatusConflict); code(a) != platformlife.CodeNodeImageMissing {
		t.Fatalf("the platform part on a site without the image: %v", a)
	}
	if a := call(http.MethodPost, path+"/runs", apply("upgrade lab to 0.99.0 and replace its machines", "seed"), http.StatusBadRequest); code(a) != "invalid_confirmation" {
		t.Fatalf("the seed part with the platform part's phrase: %v", a)
	}
	data := call(http.MethodPost, path+"/runs", apply("upgrade lab to 0.99.0", "seed"), http.StatusAccepted)["data"].(map[string]any)
	run, err := store.Run(ctx, data["runId"].(string))
	if err != nil || run.Part != platformlife.PartSeed || len(run.Stages) != 7 || run.Stages[6].ID != "seed-platform" {
		t.Fatalf("the seed part's run: %+v %v", run, err)
	}
}

// bom080 is the previous BOM of 0.8.0, by its release: the files under previous/ are read in the order of their names,
// and 0.10.0-rc.1 sorts before 0.8.0.
func bom080(t *testing.T) platformlife.Embedded {
	t.Helper()
	prev, err := platformlife.Previous()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range prev {
		if b.Release == "0.8.0" {
			return b
		}
	}
	t.Fatal("this build does not embed the 0.8.0 BOM")
	return platformlife.Embedded{}
}
