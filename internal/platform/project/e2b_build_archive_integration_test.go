//go:build integration

package project

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Embedding the API keeps this test focused on the methods syncBuild actually uses.
type archiveBuildAPI struct {
	sandbox.K8sAPI
	pod      map[string]any
	failLogs bool
	deleted  bool
}

func (f *archiveBuildAPI) GetCore(context.Context, string, string, string) (map[string]any, error) {
	return f.pod, nil
}
func (f *archiveBuildAPI) DeleteCore(_ context.Context, plural, _, _ string) error {
	if plural == "pods" {
		f.deleted, f.pod = true, nil
	}
	return nil
}
func (f *archiveBuildAPI) PodLogs(context.Context, string, string, string, int) (string, error) {
	return "deliberate stderr\n", nil
}
func (f *archiveBuildAPI) PodLogPrefix(context.Context, string, string, string, int) (string, bool, error) {
	if f.failLogs {
		return "", false, errors.New("internal cluster address")
	}
	return "2026-11-09T00:00:00Z RUN exit 2\n2026-11-09T00:00:01Z deliberate stderr\n", false, nil
}

func TestBuildArchiveSurvivesCleanupAndRestart(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("archive"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testcontainers.TerminateContainer(pgc) }()
	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	h := &Handler{sbTemplates: db.C("sandbox_templates"), cloud: cloud.NewRepo(db)}
	_ = h.sbTemplates.Ensure(ctx)
	_ = db.C("cloudResource").Ensure(ctx)
	f := &archiveBuildAPI{failLogs: true, pod: map[string]any{
		"metadata": map[string]any{"name": sandbox.BuildResourceID("b1"), "uid": "archive-worker", "labels": map[string]any{sandbox.LabelProject: "p1", sandbox.LabelManagedBy: sandbox.ManagedByValue, sandbox.LabelBuild: "b1", sandbox.LabelTemplateID: "t1"}},
		"status":   map[string]any{"phase": "Failed", "initContainerStatuses": []any{map[string]any{"name": "kaniko", "state": map[string]any{"terminated": map[string]any{"exitCode": 2, "reason": "Error"}}}}},
	}}
	c := &e2bCaller{proj: &Project{ID: "p1"}, es: &externalservice.ExternalService{ID: "s1"}, ss: sandbox.NewWithAPI(f, sandbox.Config{}, "s1")}
	now := time.Now().UTC()
	tpl := &sandboxTemplateDoc{ID: "t1", ProjectID: "p1", Name: "app", Tags: map[string]string{}, Builds: []templateBuild{{BuildID: "b1", Status: sandbox.BuildBuilding, CreatedAt: &now}}}
	if err := h.sbTemplates.Upsert(ctx, tpl.ID, tpl); err != nil {
		t.Fatal(err)
	}
	h.syncBuild(ctx, c, tpl, &tpl.Builds[0])
	if b := &tpl.Builds[0]; b.Status != sandbox.BuildError || !b.Diagnostics.LogsUnavailable || f.deleted {
		t.Fatalf("first failure: %+v", b)
	}
	past := now.Add(-3 * time.Minute)
	tpl.Builds[0].Diagnostics.LastCaptureAt, tpl.Builds[0].FinishedAt = &past, &past
	if err := h.saveTemplate(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	f.failLogs = false
	h.syncBuild(ctx, c, tpl, &tpl.Builds[0])
	if f.deleted {
		t.Fatal("recovered terminal output was not confirmed")
	}
	tpl.Builds[0].Diagnostics.LastCaptureAt = &past
	if err := h.saveTemplate(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	h.syncBuild(ctx, c, tpl, &tpl.Builds[0])
	if !f.deleted || tpl.Builds[0].Diagnostics.ArchivedAt == nil || tpl.Builds[0].Diagnostics.CleanedAt == nil || buildNeedsSync(&tpl.Builds[0]) {
		t.Fatal("successful durable recapture did not authorize cleanup")
	}
	// A newly constructed handler reads the archive, not a process-local buffer/pod.
	restarted := &Handler{sbTemplates: db.C("sandbox_templates")}
	r := httptest.NewRequest("GET", "/templates/t1/builds/b1/status?logsOffset=1", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("templateID", "t1")
	rctx.URLParams.Add("buildID", "b1")
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rctx), e2bCtxKey{}, c))
	w := httptest.NewRecorder()
	restarted.e2bTemplateBuildStatus(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "deliberate stderr") || strings.Contains(w.Body.String(), "RUN exit 2") {
		t.Fatalf("archive response: %d %s", w.Code, w.Body.String())
	}
}
