//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// noKube is a management cluster that takes every write and holds nothing.
type noKube struct{ kamaji.K8sAPI }

func (noKube) EnsureNamespace(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (noKube) ApplySecret(context.Context, string, string, map[string]string, map[string]string, map[string]string) error {
	return nil
}
func (noKube) DeleteSecret(context.Context, string, string) error { return nil }

// Production, 2026-11-17 (v0.8.0): every tenant Kubernetes create answered 502 "The compute backend is unavailable.
// Try again later." and left no audit event. PVE had refused the cluster token's ACL row on the boot store, where an
// older row of the portal's own hid the mint role /storage passes down. The create now answers 409 with a text that
// sends the tenant to the operator and holds none of PVE's words; the operator finds PVE's words in a FAILURE audit
// event that the organization's audit never shows; and a create refused for any other reason, of any resource type,
// leaves a FAILURE event too.
func TestKubernetesCreateRefusedMint(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")

	const refusal = "Permission check failed (/storage/nvme-u2, Permissions.Modify|Datastore.Allocate)"
	var mu sync.Mutex
	var minted, revoked int
	aclStatus := "403 " + refusal // what the fake answers the ACL write on the boot store
	proxmox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		_ = r.ParseForm()
		data := map[string]string{
			"/cluster/sdn/vnets":            `[{"vnet":"v1","zone":"pc"}]`,
			"/cluster/sdn/zones":            `[{"zone":"pc","type":"evpn"}]`,
			"/cluster/sdn/vnets/v1/subnets": `[{"cidr":"10.100.0.0/24","gateway":"10.100.0.1"}]`,
			"/storage":                      `[{"storage":"local","type":"dir","content":"iso,import"},{"storage":"nvme-u2","type":"zfspool","content":"images"}]`,
			"/cluster/resources": `[{"type":"storage","storage":"local","node":"pve1","status":"available"},
				{"type":"storage","storage":"nvme-u2","node":"pve1","status":"available"}]`,
			"/access/acl":              `[{"path":"/storage/local","type":"user","ugid":"capmox@pve","roleid":"PVEDatastoreAdmin","propagate":0}]`,
			"/access/users/capmox@pve": `{"groups":["proxcloud-kube"]}`,
			"/access/roles":            `[{"roleid":"PVEDatastoreAdmin","privs":"Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit"}]`,
			"/cluster/status":          `[{"type":"node","name":"pve1","online":1}]`,
			"/nodes":                   `[{"node":"pve1","status":"online"}]`,
		}
		switch body, ok := data[path]; {
		case r.Method == http.MethodGet && ok:
			fmt.Fprintf(w, `{"data":%s}`, body)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/access/users/capmox@pve/token/"):
			minted++
			fmt.Fprintf(w, `{"data":{"full-tokenid":"capmox@pve!%s","value":"secret"}}`, path[strings.LastIndex(path, "/")+1:])
		case r.Method == http.MethodDelete && strings.Contains(path, "/token/"):
			revoked++
			fmt.Fprint(w, `{"data":null}`)
		case r.Method == http.MethodPut && path == "/access/acl" && r.Form.Get("path") == "/storage/nvme-u2":
			// PVE's refusal is the status line.
			conn, _, _ := w.(http.Hijacker).Hijack()
			fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"data\":null}", aclStatus)
			_ = conn.Close()
		case r.Method == http.MethodPut && path == "/access/acl":
			fmt.Fprint(w, `{"data":null}`)
		default:
			t.Logf("the fake Proxmox has no answer for %s %s", r.Method, path)
			http.Error(w, "no fake for "+r.Method+" "+path, http.StatusNotImplemented)
		}
	}))
	t.Cleanup(proxmox.Close)

	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	auditSvc := audit.NewService(audit.NewRepo(db), slog.Default())
	h := project.NewHandler(project.NewService(project.NewRepo(db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, auditSvc, billingRepo, cloud.NewRepo(db), esSvc, nil, nil, "r1")
	h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(pve.Config{APIURL: proxmox.URL, TokenID: pve.DefaultPlatformTokenID, Secret: "s"}, "pve", "pc-")
	})
	h.SetKamaji(func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		return kamaji.NewWithAPI(noKube{}, es.KamajiConfig(), es.ID), nil
	})
	flavors := pveflavor.New(db)
	if err := flavors.Save(ctx, &pveflavor.Flavor{Slug: "s2-medium", VCPUs: 2, RAMMb: 4096, DiskGb: 30, Active: true}); err != nil {
		t.Fatal(err)
	}
	h.SetPVEFlavors(flavors)
	h.SetPublicIPs(project.NewPublicIPRepo(db))
	router := chi.NewRouter()
	h.Routes(router)

	const sub = "owner-sub"
	for coll, docs := range map[string][]pgdoc.M{
		"users":                {{"sub": sub, "email": "o@example.org"}},
		"organization":         {{"_id": "o1", "name": "o", "billingProfileId": "bp1"}},
		"organization_members": {{"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}}},
		"externalService": {
			{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{
				"provider": "proxmox", "regions": pgdoc.M{"r1": pgdoc.M{}},
				"storageProfiles": pgdoc.M{"fast": pgdoc.M{"storages": pgdoc.M{"*": "nvme-u2"}}}, "defaultStorageProfile": "fast",
				"network": pgdoc.M{"zones": pgdoc.M{"pc": pgdoc.M{}}}}},
			// A chart from before placement: the create goes straight to the grants.
			{"_id": "k8s", "name": "k8s", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{
				"provider": "kamaji", "region": "r1", "argocd": pgdoc.M{"chartVersion": "0.1.0"},
				"cluster": pgdoc.M{"versions": pgdoc.M{"1.35.10": "pve1=9200"}}}},
		},
		"project": {{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}},
			"services": []any{
				pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"},
				pgdoc.M{"serviceId": "k8s", "provider": "kamaji", "region": "r1"},
			}}},
		"cloudResource": {{"_id": "net1", "serviceId": "pve", "projectId": "p1", "type": cloud.TypeNetwork, "externalId": "v1",
			"data": pgdoc.M{"network": pgdoc.M{"vnet": "v1", "zone": "pc"}}}},
	} {
		for _, d := range docs {
			mustInsert(t, db, coll, d)
		}
	}
	create := func(service, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/project/p1/cloud", strings.NewReader(body))
		req.Header.Set("x-service-id", service)
		req.Header.Set("x-region-id", "r1")
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	// The audit write is asynchronous.
	events := func(f audit.Filter, want int) []audit.AuditEvent {
		t.Helper()
		f.Action, f.Outcome = "CLOUD_RESOURCE_CREATE", audit.OutcomeFailure
		var got []audit.AuditEvent
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			var err error
			if got, err = auditSvc.QueryAll(ctx, f, 100); err != nil {
				t.Fatal(err)
			}
			if len(got) >= want {
				break
			}
		}
		if len(got) != want {
			t.Fatalf("%d FAILURE events for %+v, want %d: %+v", len(got), f, want, got)
		}
		return got
	}

	code, body := create("k8s", `{"type":"KUBERNETES_CLUSTER","data":{"name":"demo-k8s","version":"1.35.10","ha":false,
		"nodeGroups":[{"name":"workers","flavorId":"s2-medium","count":2,"rootVolumeGiB":30}],"networkId":"v1"}}`)
	var env struct {
		Errors struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("%d %s: %v", code, body, err)
	}
	if code != http.StatusConflict || !strings.HasPrefix(env.Errors.Message, "Could not mint the cluster's Proxmox tokens: the operator must fix a permission") {
		t.Fatalf("a refused mint answered %d %s, want 409 and the text that names the operator", code, body)
	}
	for _, leak := range []string{"nvme-u2", "Permission check", "Datastore", "pve:", "unavailable", "Try again later"} {
		if strings.Contains(body, leak) {
			t.Errorf("the tenant's answer holds %q: %s", leak, body)
		}
	}
	mu.Lock()
	if minted != 1 || revoked < 1 {
		t.Errorf("tokens minted %d, revoked %d: a refused create must take its token back", minted, revoked)
	}
	mu.Unlock()
	if n, _ := db.C("cloudResource").Count(ctx, pgdoc.M{"type": cloud.TypeKubernetesCluster}); n != 0 {
		t.Errorf("a refused create left %d cluster rows", n)
	}

	// The operator's reader: what happened, to whom, and PVE's own words.
	ev := events(audit.Filter{ProjectID: "p1", ResourceType: cloud.TypeKubernetesCluster, Operator: true}, 1)[0]
	if ev.ResourceDisplayName != "demo-k8s" || ev.OrganizationID != "o1" || ev.Actor == nil || ev.Actor.ID != sub || ev.RequestInterface != audit.InterfaceClientArea {
		t.Errorf("the event does not say who was refused what: %+v", ev)
	}
	if ev.ErrorMessage != env.Errors.Message || !strings.Contains(ev.InternalReason, refusal) {
		t.Errorf("the event holds errorMessage %q and internalReason %q, want the tenant's answer and PVE's refusal", ev.ErrorMessage, ev.InternalReason)
	}
	// The organization's reader (GET /organization/{id}/audit asks with this filter): the event, without PVE's words.
	tenant := events(audit.Filter{OrganizationID: "o1", RequestInterface: audit.InterfaceClientArea, ResourceType: cloud.TypeKubernetesCluster}, 1)[0]
	if raw, _ := json.Marshal(tenant); tenant.InternalReason != "" || strings.Contains(string(raw), "nvme-u2") {
		t.Errorf("the organization's audit shows the backend's reason: %s", raw)
	}

	// A backend that fails otherwise stays a 502 that names nothing, and why still reaches the operator's event
	// (upstreamErr carries the cause).
	mu.Lock()
	aclStatus = "501 pve-storage-is-broken"
	mu.Unlock()
	code, body = create("k8s", `{"type":"KUBERNETES_CLUSTER","data":{"name":"demo-two","version":"1.35.10","ha":false,
		"nodeGroups":[{"name":"workers","flavorId":"s2-medium","count":2,"rootVolumeGiB":30}],"networkId":"v1"}}`)
	if code != http.StatusBadGateway || strings.Contains(body, "pve-storage-is-broken") {
		t.Fatalf("a failed mint answered %d %s, want 502 without the backend's words", code, body)
	}
	for _, e := range events(audit.Filter{ProjectID: "p1", ResourceType: cloud.TypeKubernetesCluster, Operator: true}, 2) {
		if e.ResourceDisplayName == "demo-two" && (!strings.Contains(e.InternalReason, "pve-storage-is-broken") || !strings.Contains(e.ErrorMessage, "unavailable")) {
			t.Errorf("the 502's event holds errorMessage %q and internalReason %q", e.ErrorMessage, e.InternalReason)
		}
	}

	// Every resource type, and a refusal as well as a backend failure: a server with no image is a 400.
	if code, body := create("pve", `{"type":"SERVER","data":{"name":"web"}}`); code/100 != 4 {
		t.Fatalf("a server without an image: %d %s", code, body)
	}
	srv := events(audit.Filter{ProjectID: "p1", ResourceType: cloud.TypeServer, Operator: true}, 1)[0]
	if srv.ResourceDisplayName != "web" || srv.ErrorMessage == "" || srv.InternalReason != "" {
		t.Errorf("a refused server create: %+v", srv)
	}

	// The type and the name come from the refused request: a type that is not the name of one is not stored, and
	// the name and the answer that quotes the type are cut, so a member cannot fill the audit log through creates that fail.
	long := strings.Repeat("x", 4000)
	if code, body := create("pve", `{"type":"`+long+`","data":{"name":"`+long+`é"}}`); code/100 != 4 {
		t.Fatalf("a create of an unknown type: %d %s", code, body)
	}
	odd := events(audit.Filter{ProjectID: "p1", ResourceType: "CLOUD_RESOURCE", Operator: true}, 1)[0]
	if raw, _ := json.Marshal(odd); len(odd.ResourceDisplayName) != 255 || len(raw) > 2500 || odd.ResourceMetadata["resourceType"] != "" {
		t.Errorf("the event of a create with a 4000 byte type and name is %d bytes, name %d: %s", len(raw), len(odd.ResourceDisplayName), raw)
	}
}
