package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// GET /admin/platform/bom needs admin:service:read and answers the embedded BOM with its sha256.
func TestPlatformBOMRoute(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	r.Route("/admin", h.routePlatform)

	if w := tuDo(r, "GET", "/admin/platform/bom", "", &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}); w.Code != http.StatusForbidden {
		t.Fatalf("tenant: %d", w.Code)
	}
	w := tuDo(r, "GET", "/admin/platform/bom", "", &httpx.RequestContext{SigV4KeyID: "pk-test"})
	if w.Code != http.StatusOK {
		t.Fatalf("operator: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Data struct {
			SHA256     string `json:"sha256"`
			Release    string `json:"release"`
			Components []struct {
				Name, Cluster, Group string
				Chart                struct{ Version, Digest string }
			} `json:"components"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	if got.Data.SHA256 != cur.SHA256 || got.Data.Release != cur.Release || len(got.Data.Components) != len(cur.Components) {
		t.Fatalf("answer %+v, embedded %s %s with %d components", got.Data, cur.SHA256, cur.Release, len(cur.Components))
	}
	if c := got.Data.Components[0]; c.Name == "" || c.Cluster == "" || c.Chart.Version == "" || c.Chart.Digest == "" {
		t.Errorf("component without name, cluster, version or digest: %+v", c)
	}
}
