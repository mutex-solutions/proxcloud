//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// prereqSite is a Proxmox provider with the given network config, the installation routes over PostgreSQL, and a fake
// Proxmox that answers the SDN zones from *zones ("" answers 500).
func prereqSite(t *testing.T, network pgdoc.M, zones *string) (call func(method, path, body string, want int) (json.RawMessage, string, string), svc *externalservice.Service, store *platformlife.Store) {
	t.Helper()
	db := freshPG(t)
	fakePVE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a save wrote to Proxmox: %s %s", r.Method, r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/cluster/sdn/zones") && *zones != "" {
			fmt.Fprintf(w, `{"data":%s}`, *zones)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errors":{"fake":"nothing here"}}`))
	}))
	t.Cleanup(fakePVE.Close)
	enc := textcrypt.New("test-key")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!m0", "importStorage": "nas-templates", "network": network},
		"secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	store = platformlife.NewStore(db, enc)
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc = externalservice.NewService(externalservice.NewRepo(db), enc)
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()), svc, "", "", "")
	h.SetPlatform(store)
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		c, err := pve.New(es.PVEConfig())
		return &pve.Service{C: c, ServiceID: es.ID}, err
	})
	router := chi.NewRouter()
	h.Routes(router)
	call = func(method, path, body string, want int) (json.RawMessage, string, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		raw, _ := io.ReadAll(w.Body)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, raw, want)
		}
		var a struct {
			Data   json.RawMessage `json:"data"`
			Errors struct {
				Details map[string]any `json:"details"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(raw, &a)
		code, _ := a.Errors.Details["code"].(string)
		return a.Data, code, string(raw)
	}
	return call, svc, store
}

type savedView struct {
	ID          string
	Rev         int64
	SiteChanges *struct {
		BlockedCIDRs    []string `json:"blockedCidrs"`
		ZoneWithoutDHCP string   `json:"zoneWithoutDhcp"`
		Problem         string
	}
}

// A save fills the provider's blocked networks from the installation's own addresses (the seed, the API address and
// the node range; never the MetalLB range), adds only what is missing, keeps the operator's entries, and says what it
// added. A hand step of the first production install.
func TestPlatformSaveFillsTheBlockedNetworks(t *testing.T) {
	zones := `[]`
	call, svc, _ := prereqSite(t, pgdoc.M{"cidrPool": "10.100.0.0/16", "blockedCidrs": []string{"192.168.50.0/24", "10.200.200.10/32"}}, &zones)
	const base = "/admin/platform/installations"
	var v savedView
	data, _, _ := call(http.MethodPost, base, platformBody("10.200.200.1", 2, 0), http.StatusOK)
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	want := []string{"10.200.200.20/32", "10.200.200.30/31", "10.200.200.32/28", "10.200.200.48/31"} // the seed was blocked already
	if v.SiteChanges == nil || !slices.Equal(v.SiteChanges.BlockedCIDRs, want) || v.SiteChanges.ZoneWithoutDHCP != "" || v.SiteChanges.Problem != "" {
		t.Fatalf("site changes of the create: %+v", v.SiteChanges)
	}
	es, err := svc.Get(context.Background(), "svc-pve")
	if err != nil {
		t.Fatal(err)
	}
	n := es.PVENetworkConfig()
	if !slices.Equal(n.BlockedCIDRs, append([]string{"192.168.50.0/24", "10.200.200.10/32"}, want...)) || n.CIDRPool != "10.100.0.0/16" {
		t.Fatalf("the provider's network after the save: %+v", n)
	}
	in := &platformlife.Installation{}
	_ = json.Unmarshal(data, in)
	if unf := in.Unfenced(n.BlockedCIDRs); len(unf) != 0 {
		t.Fatalf("still unfenced: %v", unf)
	}
	for _, c := range n.BlockedCIDRs {
		if strings.HasPrefix(c, "10.200.200.2") && c != "10.200.200.20/32" { // .200-.220 is the MetalLB range
			t.Fatalf("the MetalLB range is blocked: %v", n.BlockedCIDRs)
		}
	}
	if len(in.SiteProblems(platformlife.SiteOf(es))) != 0 {
		t.Fatalf("the filled list refuses the installation: %v", in.SiteProblems(platformlife.SiteOf(es)))
	}
	// A second save of the same settings changes nothing and says nothing.
	v = savedView{}
	data, _, _ = call(http.MethodPut, base+"/"+in.ID, platformBody("10.200.200.1", 2, 1), http.StatusOK)
	_ = json.Unmarshal(data, &v)
	if es, _ = svc.Get(context.Background(), "svc-pve"); v.SiteChanges != nil || len(es.PVENetworkConfig().BlockedCIDRs) != len(n.BlockedCIDRs) {
		t.Fatalf("second save: %+v, blocked %v", v.SiteChanges, es.PVENetworkConfig().BlockedCIDRs)
	}
}

// The default tenant zone: a simple zone that does not exist yet and whose config says nothing about DHCP gets
// "dhcp": false at the save, so it is created without DHCP. A zone that exists, a config that says "dhcp": true, a
// zone of another kind and a zone list that cannot be read are left alone.
func TestPlatformSaveMakesANewTenantZoneWithoutDHCP(t *testing.T) {
	blocked := []string{"10.200.200.0/25"}
	for _, tt := range []struct {
		name    string
		zone    pgdoc.M
		live    string
		noDHCP  bool
		problem bool
	}{
		{"a new simple zone", pgdoc.M{"kind": "simple"}, `[{"zone":"other","type":"simple","dhcp":"dnsmasq"}]`, true, false},
		{"a zone that exists is never changed", pgdoc.M{"kind": "simple"}, `[{"zone":"pc","type":"simple","dhcp":"dnsmasq"}]`, false, false},
		{"the config says dhcp true", pgdoc.M{"kind": "simple", "dhcp": true}, `[]`, false, false},
		{"a vlan zone", pgdoc.M{"kind": "vlan", "bridge": "vmbr0"}, `[]`, false, false},
		{"the zones cannot be read", pgdoc.M{"kind": "simple"}, "", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			live := tt.live
			call, svc, _ := prereqSite(t, pgdoc.M{"cidrPool": "10.100.0.0/16", "blockedCidrs": blocked, "defaultZone": "pc", "zones": pgdoc.M{"pc": tt.zone}}, &live)
			var v savedView
			data, _, _ := call(http.MethodPost, "/admin/platform/installations", platformBody("10.200.200.1", 2, 0), http.StatusOK)
			_ = json.Unmarshal(data, &v)
			es, err := svc.Get(context.Background(), "svc-pve")
			if err != nil {
				t.Fatal(err)
			}
			z := es.PVENetworkConfig().Zones["pc"]
			if z.NoDHCP != tt.noDHCP || z.Kind != tt.zone["kind"] {
				t.Fatalf("zone config after the save: %+v", z)
			}
			said := v.SiteChanges != nil && v.SiteChanges.ZoneWithoutDHCP == "pc"
			if said != tt.noDHCP || (v.SiteChanges != nil && v.SiteChanges.Problem != "") != tt.problem {
				t.Fatalf("site changes: %+v", v.SiteChanges)
			}
		})
	}
}

// The pasted enrollment line stores both secrets once: a line that is not one, or of another installation, is a 400
// that does not quote it; the same line again, and an older one, are a 409 and leave the stored secrets as they are.
func TestPlatformEnrollLine(t *testing.T) {
	zones := `[]`
	call, _, store := prereqSite(t, pgdoc.M{"cidrPool": "10.100.0.0/16", "blockedCidrs": []string{"10.200.200.0/25"}}, &zones)
	var in platformlife.Installation
	data, _, _ := call(http.MethodPost, "/admin/platform/installations", platformBody("10.200.200.1", 2, 0), http.StatusOK)
	_ = json.Unmarshal(data, &in)
	creds := "/admin/platform/installations/" + in.ID + "/credentials"
	line := func(id string, issued int64, capi, csi string) string {
		b, _ := json.Marshal(map[string]string{"enroll": pve.EnrollPrefix + base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "1:%s:%d:%s:%s", id, issued, capi, csi))})
		return string(b)
	}
	const newerCSI = "5e1f4c7a-0d2b-4c3e-9a8b-c0ffee000009"
	secrets := func() (string, string) {
		sec, err := store.Secrets(context.Background(), in.ID)
		if err != nil || sec == nil || sec.CAPI == nil || sec.CSI == nil {
			return "", ""
		}
		return sec.CAPI.Secret, sec.CSI.Secret
	}

	for name, body := range map[string]string{
		"not a line":           `{"enroll":"` + plantedCAPI + `"}`,
		"another installation": line("pi-ffffffff", 1795000000, plantedCAPI, plantedCSI),
	} {
		_, code, raw := call(http.MethodPut, creds, body, http.StatusBadRequest)
		if code != "invalid_enrollment" || strings.Contains(raw, plantedCAPI) || strings.Contains(raw, plantedCSI) {
			t.Fatalf("%s: %s %s", name, code, raw)
		}
	}
	if _, code, _ := call(http.MethodPut, creds, strings.Replace(line(in.ID, 1795000000, plantedCAPI, plantedCSI), `{`, `{"capi":{"tokenId":"capmox@pve!capi","secret":"`+plantedCAPI+`"},`, 1), http.StatusBadRequest); code != "invalid_credentials" {
		t.Fatalf("enroll with capi: %s", code)
	}
	if a, b := secrets(); a != "" || b != "" {
		t.Fatalf("a refused line stored something")
	}

	data, _, raw := call(http.MethodPut, creds, line(in.ID, 1795000000, plantedCAPI, plantedCSI), http.StatusOK)
	var view struct {
		Credentials struct{ CAPI, CSI struct{ TokenID string } }
	}
	_ = json.Unmarshal(data, &view)
	if a, b := secrets(); a != plantedCAPI || b != plantedCSI || view.Credentials.CAPI.TokenID != pve.CAPITokenID || view.Credentials.CSI.TokenID != pve.CSITokenID ||
		strings.Contains(raw, plantedCAPI) {
		t.Fatalf("the line was not stored, or the answer holds a secret: %s", raw)
	}
	// A newer line for one token replaces that one only.
	call(http.MethodPut, creds, line(in.ID, 1795000100, "", newerCSI), http.StatusOK)
	if a, b := secrets(); a != plantedCAPI || b != newerCSI {
		t.Fatalf("after the newer line: %s %s", a, b)
	}
	// Replays: the newest line again, and the first one, which would put the old CSI secret back.
	for name, body := range map[string]string{"the same line": line(in.ID, 1795000100, "", newerCSI), "an older line": line(in.ID, 1795000000, plantedCAPI, plantedCSI)} {
		_, code, raw := call(http.MethodPut, creds, body, http.StatusConflict)
		if a, b := secrets(); code != "enrollment_replayed" || a != plantedCAPI || b != newerCSI || strings.Contains(raw, plantedCSI) {
			t.Fatalf("%s: %s, stored %s %s, %s", name, code, a, b, raw)
		}
	}

	// The root command of the installation: its own stores and the provider's token, and it knows both secrets are held.
	script := func(q string, want int) string {
		_, _, raw := call(http.MethodGet, "/admin/setup/proxmox-script?"+q, "", want)
		return raw
	}
	s := script("installation="+in.ID, http.StatusOK)
	for _, want := range []string{"PC_TOKEN='proxcloud@pve!m0'", "PC_INSTALL='" + in.ID + "'", "'/storage/local-lvm'", "'/storage/nas-templates'", "trap pc_enroll EXIT"} {
		if !strings.Contains(s, want) {
			t.Errorf("the root command lacks %q", want)
		}
	}
	if strings.Contains(s, "STOP: the token") {
		t.Errorf("the root command stops on a token whose secret the portal holds:\n%s", s)
	}
	script("installation="+in.ID+"&bootStorage=other", http.StatusBadRequest)
	script("installation=pi-ffffffff", http.StatusNotFound)
}
