//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Image adoption writes a template's image only while the row still holds the image it read: an operator's edit in
// the meantime wins, and the rest of the template stays.
func TestSetTemplateImage(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "s1", "config": pgdoc.M{"sandbox": pgdoc.M{"templates": pgdoc.M{
		"base": pgdoc.M{"image": "r/library/sandbox-base@sha256:1", "cpu": 2}}}}})
	r := externalservice.NewRepo(db)
	if ok, err := r.SetTemplateImage(ctx, "s1", "base", "r/library/sandbox-base@sha256:0", "r/library/sandbox-base@sha256:2"); err != nil || ok {
		t.Fatalf("a stale from must not write: %v %v", ok, err)
	}
	if ok, err := r.SetTemplateImage(ctx, "s1", "base", "r/library/sandbox-base@sha256:1", "r/library/sandbox-base@sha256:2"); err != nil || !ok {
		t.Fatalf("write: %v %v", ok, err)
	}
	es, err := r.FindByID(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	tpl := es.Config["sandbox"].(map[string]any)["templates"].(map[string]any)["base"].(map[string]any)
	if tpl["image"] != "r/library/sandbox-base@sha256:2" || tpl["cpu"] == nil {
		t.Fatalf("template after adoption: %v", tpl)
	}
}
