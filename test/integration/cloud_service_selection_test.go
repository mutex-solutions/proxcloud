//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestCloudServiceSelection (#215): a project created with a Proxmox provider, with an S3 store registered afterwards and
// never attached to it. A request naming that store (x-service-id or ?serviceId=) is refused with 400 on every route that
// picks a service — the writes and the reads — and nothing reaches a provider: the Proxmox client and the store both point
// at a server that counts every call. Naming the attached service, or none, keeps today's behaviour.
func TestCloudServiceSelection(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "unexpected provider call "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}))
	t.Cleanup(provider.Close)

	repo := project.NewRepo(db)
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	h := project.NewHandler(project.NewService(repo, orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo,
		cloud.NewRepo(db), esSvc, nil, nil, "r1")
	h.SetCephCreds(cephcred.New(db, enc), cephcred.NewKeyRepo(db, enc))
	h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(pve.Config{APIURL: provider.URL, TokenID: "t", Secret: "s"}, "pve", "pc-")
	})
	router := chi.NewRouter()
	h.Routes(router)

	const sub = "owner-sub"
	pveConfig := pgdoc.M{"provider": "proxmox", "regions": pgdoc.M{"r1": pgdoc.M{}},
		"network": pgdoc.M{"publicNetworks": []any{pgdoc.M{"id": "vmbr1", "name": "public"}}}}
	s3Config := func() pgdoc.M {
		return pgdoc.M{"provider": "ceph-s3", "region": "r1", "endpoint": provider.URL, "adminEndpoint": provider.URL}
	}
	for coll, docs := range map[string][]pgdoc.M{
		"users":                {{"sub": sub, "email": "o@example.org"}},
		"organization":         {{"_id": "o1", "name": "o", "billingProfileId": "bp1"}},
		"organization_members": {{"organizationId": "o1", "sub": sub, "roles": []string{"OWNER"}}},
		"externalService": {
			{"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pveConfig},
			{"_id": "s3-attached", "name": "s3a", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": s3Config()},
			{"_id": "s3-new", "name": "s3b", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": s3Config()},
		},
		"project": {{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1", "gpuCapacityVisible": true,
			"memberships": []pgdoc.M{{"sub": sub, "role": project.RoleOwner}},
			"services": []any{
				pgdoc.M{"serviceId": "s3-attached", "provider": "ceph-s3", "region": "r1", "rgwUid": "p1"},
				pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"},
			}}},
	} {
		for _, d := range docs {
			mustInsert(t, db, coll, d)
		}
	}
	do := func(method, path, serviceHeader, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if serviceHeader != "" {
			req.Header.Set("x-service-id", serviceHeader)
			req.Header.Set("x-region-id", "r1")
		}
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	const refusal = "Project is not attached to service s3-new"

	t.Run("an unattached service is refused before any provider call", func(t *testing.T) {
		cases := []struct{ method, path, header, body string }{
			// The report's reproduction: a BUCKET create for the new store used to land on Proxmox.
			{http.MethodPost, "/project/p1/cloud", "s3-new", `{"type":"BUCKET","data":{"bucketName":"test-bucket"}}`},
			{http.MethodPost, "/project/p1/cloud?serviceId=s3-new", "", `{"type":"BUCKET","data":{"bucketName":"test-bucket"}}`},
			{http.MethodPost, "/project/p1/cloud", "s3-new", `{"type":"SANDBOX","data":{}}`},
			{http.MethodPost, "/project/p1/cloud/action", "s3-new", `{"type":"SERVER","action":"LIST_FLAVORS"}`},
			{http.MethodPost, "/project/p1/cloud/r1/action", "s3-new", `{"action":"GET_SETTINGS"}`},
			{http.MethodDelete, "/project/p1/cloud/r1", "s3-new", ""},
			{http.MethodGet, "/project/p1/quota-usage", "s3-new", ""},
			{http.MethodGet, "/project/p1/gpu-capacity?serviceId=s3-new", "", ""},
			{http.MethodGet, "/project/p1/public-networks", "s3-new", ""},
			{http.MethodGet, "/project/p1/s3-credentials", "s3-new", ""},
		}
		for _, c := range cases {
			code, body := do(c.method, c.path, c.header, c.body)
			if code != http.StatusBadRequest || !strings.Contains(body, refusal) {
				t.Errorf("%s %s (x-service-id %q): %d %s, want 400 %q", c.method, c.path, c.header, code, body, refusal)
			}
		}
		if n := providerCalls.Load(); n != 0 {
			t.Fatalf("a refused request reached a provider %d times", n)
		}
		if n, _ := db.C("cloudResource").Count(ctx, pgdoc.M{}); n != 0 {
			t.Fatalf("a refused request left %d cache rows", n)
		}
	})

	t.Run("no service named: the Proxmox default, as before", func(t *testing.T) {
		code, body := do(http.MethodGet, "/project/p1/quota-usage", "", "")
		var env struct {
			Data struct {
				ServiceID string `json:"serviceId"`
			} `json:"data"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &env) != nil || env.Data.ServiceID != "pve" {
			t.Fatalf("quota-usage: %d %s, want the Proxmox service", code, body)
		}
		code, body = do(http.MethodGet, "/project/p1/public-networks", "", "")
		if code != http.StatusOK || !strings.Contains(body, `"vmbr1"`) {
			t.Fatalf("public-networks: %d %s, want the Proxmox public network", code, body)
		}
	})

	t.Run("an attached service named: that service", func(t *testing.T) {
		for _, path := range []string{"/project/p1/quota-usage?serviceId=s3-attached", "/project/p1/quota-usage"} {
			header := ""
			if !strings.Contains(path, "?") {
				header = "s3-attached"
			}
			code, body := do(http.MethodGet, path, header, "")
			var env struct {
				Data struct {
					ServiceID string `json:"serviceId"`
				} `json:"data"`
			}
			if code != http.StatusOK || json.Unmarshal([]byte(body), &env) != nil || env.Data.ServiceID != "s3-attached" {
				t.Fatalf("%s: %d %s, want s3-attached", path, code, body)
			}
		}
		// The object store has no public networks: an empty list, not the Proxmox ones.
		if code, body := do(http.MethodGet, "/project/p1/public-networks", "s3-attached", ""); code != http.StatusOK || strings.Contains(body, "vmbr1") {
			t.Fatalf("public-networks for the store: %d %s", code, body)
		}
	})
}
