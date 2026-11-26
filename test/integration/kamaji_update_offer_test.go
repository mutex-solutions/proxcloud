//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// offerKube is a management cluster that holds the Applications of project p1's clusters and nothing else, and
// counts who is applied.
type offerKube struct {
	kamaji.K8sAPI
	mu      sync.Mutex
	apps    map[string]map[string]any
	applied []string
}

func (k *offerKube) GetApplication(_ context.Context, _, name string) (map[string]any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.apps[name], nil
}
func (k *offerKube) ListApplications(context.Context, string, string) ([]map[string]any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []map[string]any
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range k.apps {
			if !yield(n) {
				return
			}
		}
	}) {
		out = append(out, k.apps[name])
	}
	return out, nil
}
func (k *offerKube) ApplyApplication(_ context.Context, app map[string]any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	name := app["metadata"].(map[string]any)["name"].(string)
	k.apps[name] = app
	k.applied = append(k.applied, name)
	return nil
}
func (k *offerKube) ListCRs(context.Context, string, string, string, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *offerKube) GetTenantControlPlane(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}
func (k *offerKube) ListTenantControlPlanes(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *offerKube) ListMachineDeployments(context.Context, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (k *offerKube) image(name string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	lb, _ := k.apps[name]["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["valuesObject"].(map[string]any)["loadBalancer"].(map[string]any)
	s, _ := lb["image"].(string)
	return s
}
func (k *offerKube) chart(name string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, _ := k.apps[name]["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"].(string)
	return s
}
func (k *offerKube) appliedNow() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	defer func() { k.applied = nil }()
	slices.Sort(k.applied)
	return strings.Join(k.applied, ",")
}

// A release that changes only the load balancer controller, through the routes (lab drill of v0.11.1-rc.3, step F:
// the provider's chart equalled the cluster's, so the tenant was offered nothing and the controller stayed on the
// earlier candidate until the operator's per-cluster action). The portal's controller image is portal:v2 and the
// provider pins chart 0.1.19. Four clusters of project p1:
//
//	pc-c-behind    chart 0.1.19, controller on portal:v1   offered; the update moves the image alone
//	pc-c-current   chart 0.1.19, controller on portal:v2   not offered, never applied
//	pc-c-plain     chart 0.1.19, no controller             not offered, never applied
//	pc-c-oldchart  chart 0.1.18, controller on portal:v1   offered as before (the chart), image moved with it
//
// The management cluster is a fake that holds the four Applications: nothing is rendered or rolled here.
func TestClusterUpdateOfferedForTheControllerImage(t *testing.T) {
	db := freshPG(t)
	app := func(name, chart, image string) map[string]any {
		values := map[string]any{"kubernetesVersion": "1.35.10", "nodeGroups": []any{map[string]any{"name": "workers", "replicas": float64(2)}}}
		if image != "" {
			values["loadBalancer"] = kamaji.LBValues(name, kamaji.LBController{Image: image, GatewayURL: "https://192.0.2.7:8444", Default: true})
		}
		return map[string]any{
			"metadata": map[string]any{"name": name, "labels": map[string]any{
				kamaji.LabelManagedBy: kamaji.ManagedByValue, kamaji.LabelService: "k8s", kamaji.LabelProject: "p1"}},
			"spec": map[string]any{"project": "proxcloud-k8s", "destination": map[string]any{"namespace": "pc-p1"},
				"source": map[string]any{"targetRevision": chart, "helm": map[string]any{"valuesObject": values}}},
		}
	}
	kube := &offerKube{apps: map[string]map[string]any{
		"pc-c-behind":   app("pc-c-behind", "0.1.19", "portal:v1"),
		"pc-c-current":  app("pc-c-current", "0.1.19", "portal:v2"),
		"pc-c-plain":    app("pc-c-plain", "0.1.19", ""),
		"pc-c-oldchart": app("pc-c-oldchart", "0.1.18", "portal:v1"),
	}}

	orgRepo, billingRepo, users, cloudRepo := org.NewRepo(db), billing.NewRepo(db), user.NewRepo(db), cloud.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	// As project.KamajiFactory builds it: the provider's config, and the portal's own image for the controllers.
	kamajiFor := func(es *externalservice.ExternalService) (*kamaji.Service, error) {
		cfg := es.KamajiConfig()
		cfg.LBControllerImage = "portal:v2"
		return kamaji.NewWithAPI(kube, cfg, es.ID), nil
	}
	ph := project.NewHandler(project.NewService(project.NewRepo(db), orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, cloudRepo, esSvc, nil, nil, "r1")
	ph.SetKamaji(kamajiFor)
	ah := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, esSvc, "r1", "", "")
	ah.SetKamaji(kamajiFor)
	router := chi.NewRouter()
	ph.Routes(router)
	ah.Routes(router)

	const owner = "owner-sub"
	k8sSvc := pgdoc.M{"serviceId": "k8s", "provider": "kamaji", "region": "r1"}
	for coll, docs := range map[string][]pgdoc.M{
		"users":                {{"sub": owner, "email": "o@example.org"}},
		"organization":         {{"_id": "o1", "name": "o", "billingProfileId": "bp1"}},
		"organization_members": {{"organizationId": "o1", "sub": owner, "roles": []string{"OWNER"}}},
		"adminPermission":      {{"_id": "support-sub", "role": "SUPPORT"}, {"_id": "admin-sub", "role": "ADMIN"}},
		"externalService": {{"_id": "k8s", "name": "k8s", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{
			"provider": "kamaji", "region": "r1", "argocd": pgdoc.M{"namespace": "argocd", "chartVersion": "0.1.19"},
			"cluster": pgdoc.M{"versions": pgdoc.M{"1.35.10": "pve1=9200"}}}}},
		"project": {{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": owner, "role": project.RoleOwner}}, "services": []any{k8sSvc}}},
	} {
		for _, d := range docs {
			mustInsert(t, db, coll, d)
		}
	}
	call := func(sub, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("x-service-id", "k8s")
		req.Header.Set("x-region-id", "r1")
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	// tenant reads the project's clusters as the cluster page does: row id, chart and the controller offer by cluster.
	type row struct{ id, chart string }
	tenant := func() (rows map[string]row, offered []string) {
		t.Helper()
		code, body := call(owner, http.MethodPost, "/project/p1/resource?type="+cloud.TypeKubernetesCluster, "")
		var list struct {
			Data []struct {
				ID         string `json:"id"`
				ExternalID string `json:"externalId"`
				Data       struct {
					Cluster map[string]any `json:"cluster"`
				} `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil || code != http.StatusOK || len(list.Data) != 4 {
			t.Fatalf("the tenant's clusters: %d %s (%v)", code, body, err)
		}
		rows = map[string]row{}
		for _, r := range list.Data {
			chart, _ := r.Data.Cluster["chart_version"].(string)
			rows[r.ExternalID] = row{r.ID, chart}
			if r.Data.Cluster["lb_controller_update"] == true {
				offered = append(offered, r.ExternalID)
			}
		}
		slices.Sort(offered)
		return rows, offered
	}
	// operator reads the provider's cluster rows: who is behind the pin by the chart, who by the controller's image.
	operator := func() (chartBehind, imageBehind []string) {
		t.Helper()
		code, body := call("support-sub", http.MethodGet, "/admin/service/k8s/k8s-clusters", "")
		var view struct {
			Data struct {
				ProviderChartVersion string              `json:"providerChartVersion"`
				Clusters             []kamaji.ClusterPin `json:"clusters"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &view); err != nil || code != http.StatusOK || view.Data.ProviderChartVersion != "0.1.19" {
			t.Fatalf("the operator's clusters: %d %s (%v)", code, body, err)
		}
		for _, c := range view.Data.Clusters {
			if c.ChartVersion != view.Data.ProviderChartVersion {
				chartBehind = append(chartBehind, c.ID)
			}
			if c.LBControllerBehind {
				imageBehind = append(imageBehind, c.ID)
			}
		}
		slices.Sort(chartBehind)
		slices.Sort(imageBehind)
		return
	}

	// 1. What is offered. The provider's chart equals pc-c-behind's: the tenant's page had nothing to compare.
	rows, offered := tenant()
	if rows["pc-c-behind"].chart != "0.1.19" || !slices.Equal(offered, []string{"pc-c-behind", "pc-c-oldchart"}) {
		t.Fatalf("offered the controller update: %v, want pc-c-behind and pc-c-oldchart (rows %v)", offered, rows)
	}
	if chartBehind, imageBehind := operator(); !slices.Equal(chartBehind, []string{"pc-c-oldchart"}) || !slices.Equal(imageBehind, []string{"pc-c-behind", "pc-c-oldchart"}) {
		t.Fatalf("the operator's rows: chart behind %v, image behind %v", chartBehind, imageBehind)
	}
	if got := kube.appliedNow(); got != "" {
		t.Fatalf("reading applied %s", got)
	}

	// 2. The tenant's opt-in action on pc-c-behind: its controller's image, on the chart it had, and no other
	// cluster is touched. The page then offers it nothing.
	code, body := call(owner, http.MethodPost, "/project/p1/cloud/"+rows["pc-c-behind"].id+"/action", `{"action":"APPLY_PLATFORM_UPDATE"}`)
	if code != http.StatusOK || kube.image("pc-c-behind") != "portal:v2" || kube.chart("pc-c-behind") != "0.1.19" {
		t.Fatalf("the tenant's update: %d %s; image %s chart %s", code, body, kube.image("pc-c-behind"), kube.chart("pc-c-behind"))
	}
	if got := kube.appliedNow(); got != "pc-c-behind" {
		t.Fatalf("the tenant's update applied %q, want pc-c-behind alone", got)
	}
	if _, offered = tenant(); !slices.Equal(offered, []string{"pc-c-oldchart"}) {
		t.Fatalf("offered after the tenant's update: %v", offered)
	}

	// 3. The operator's "Bump all", with pc-c-behind behind again: it moves the cluster that is on the pin too, and
	// leaves alone the two that would not change. It skipped pc-c-behind before: its chart was the pin's.
	kube.mu.Lock()
	kube.apps["pc-c-behind"] = app("pc-c-behind", "0.1.19", "portal:v1")
	kube.mu.Unlock()
	code, body = call("support-sub", http.MethodPost, "/admin/service/k8s/k8s-clusters/bump-chart", "")
	if code != http.StatusForbidden || kube.appliedNow() != "" {
		t.Fatalf("admin:service:read bumped: %d %s", code, body)
	}
	code, body = call("admin-sub", http.MethodPost, "/admin/service/k8s/k8s-clusters/bump-chart", "")
	var bumped struct {
		Data struct {
			Bumped int               `json:"bumped"`
			Errors map[string]string `json:"errors"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &bumped); err != nil || code != http.StatusOK || bumped.Data.Bumped != 2 || len(bumped.Data.Errors) != 0 {
		t.Fatalf("bump all: %d %s (%v)", code, body, err)
	}
	if got := kube.appliedNow(); got != "pc-c-behind,pc-c-oldchart" {
		t.Fatalf("bump all applied %q, want pc-c-behind and pc-c-oldchart", got)
	}
	for _, name := range []string{"pc-c-behind", "pc-c-oldchart"} {
		if kube.image(name) != "portal:v2" || kube.chart(name) != "0.1.19" {
			t.Errorf("%s after bump all: image %s chart %s", name, kube.image(name), kube.chart(name))
		}
	}
	if kube.image("pc-c-plain") != "" {
		t.Errorf("a cluster without a controller got image %s", kube.image("pc-c-plain"))
	}
	// Nobody is behind any more: a second "Bump all" moves nothing, and the tenant is offered nothing.
	if chartBehind, imageBehind := operator(); len(chartBehind)+len(imageBehind) != 0 {
		t.Errorf("after bump all: chart behind %v, image behind %v", chartBehind, imageBehind)
	}
	if code, body = call("admin-sub", http.MethodPost, "/admin/service/k8s/k8s-clusters/bump-chart", ""); code != http.StatusOK || !strings.Contains(body, `"bumped":0`) || kube.appliedNow() != "" {
		t.Errorf("a second bump all: %d %s", code, body)
	}
	if _, offered = tenant(); len(offered) != 0 {
		t.Errorf("offered after bump all: %v", offered)
	}
}
