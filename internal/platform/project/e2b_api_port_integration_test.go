//go:build integration

package project

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

type portImportAPI struct {
	sandbox.K8sAPI
	mu              sync.Mutex
	pods            map[string]map[string]any
	starts, deletes int
	result          *sandboximport.Result
	ambiguous       bool
}

func (a *portImportAPI) GetSecretData(_ context.Context, _, name string) (map[string][]byte, error) {
	if name == "sandbox-import-worker" {
		return map[string][]byte{"image": []byte("registry.example.org/library/proxcloud-sandbox-import@sha256:" + strings.Repeat("a", 64))}, nil
	}
	return map[string][]byte{".dockerconfigjson": []byte(`{"auths":{"registry.example.org":{"auth":"cHVsbDpwYXNz"}}}`)}, nil
}
func (a *portImportAPI) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	if plural != "pods" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts++
	md := obj["metadata"].(map[string]any)
	md["uid"] = "import-worker-uid"
	obj["status"] = map[string]any{"phase": "Running"}
	a.pods[md["name"].(string)] = obj
	if a.ambiguous {
		return fmt.Errorf("ambiguous apply")
	}
	return nil
}
func (a *portImportAPI) Apply(context.Context, string, string, map[string]any) error { return nil }
func (a *portImportAPI) GetCore(_ context.Context, _, _, name string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pod := a.pods[name]
	if pod == nil {
		return nil, nil
	}
	raw, _ := json.Marshal(pod)
	var cp map[string]any
	_ = json.Unmarshal(raw, &cp)
	if a.result != nil {
		cp["status"] = map[string]any{"phase": "Succeeded"}
	}
	return cp, nil
}
func (a *portImportAPI) ListPods(context.Context, string, string) ([]map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, p := range a.pods {
		out = append(out, p)
	}
	return out, nil
}
func (a *portImportAPI) PodLogs(context.Context, string, string, string, int) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(a.result)
	return string(b), nil
}
func (a *portImportAPI) DeleteCoreUID(ctx context.Context, plural, ns, name, uid string) error {
	a.mu.Lock()
	pod := a.pods[name]
	valid := pod == nil || pod["metadata"].(map[string]any)["uid"] == uid
	a.mu.Unlock()
	if !valid {
		return fmt.Errorf("UID precondition failed")
	}
	return a.DeleteCore(ctx, plural, ns, name)
}
func (a *portImportAPI) DeleteCore(_ context.Context, plural, _, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if plural == "pods" {
		delete(a.pods, name)
		a.deletes++
	}
	return nil
}
func (a *portImportAPI) ListNamespaces(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}
func (a *portImportAPI) ApplyNamespace(context.Context, map[string]any) error             { return nil }
func (a *portImportAPI) EnsureNamespace(context.Context, string, map[string]string) error { return nil }

func TestImportPortConcurrentAliasesTerminalBindingAndUnknownHold(t *testing.T) {
	ctx := context.Background()
	pgc, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("api_port"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pgc) })
	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(ctx) })
	if err := registrypins.EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	h := &Handler{sbTemplates: db.C("sandbox_templates"), sbBuildPinsDB: db, sandboxGates: func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
		return []sandbox.Gate{{Name: "fixture", OK: true}}
	}}
	// Do not pre-create sandbox_templates: a direct import is the first template
	// action on this fresh install and must establish its table outside SQL TX.
	project := strings.Repeat("b", 24)
	if err := db.C("project").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.C("project").Upsert(ctx, project, pgdoc.M{"status": "ENABLED"}); err != nil {
		t.Fatal(err)
	}
	source := "registry.example.org/t/" + project + "/" + strings.Repeat("c", 20) + "@sha256:" + strings.Repeat("d", 64)
	api := &portImportAPI{pods: map[string]map[string]any{}}
	caller := &e2bCaller{proj: &Project{ID: project, Status: StatusEnabled}, es: &externalservice.ExternalService{ID: "svc"}, ss: sandbox.NewWithAPI(api, sandbox.Config{TemplateRegistry: "registry.example.org/t"}, "svc")}
	request := func(name, source string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/templates/import", strings.NewReader(fmt.Sprintf(`{"name":%q,"source":%q}`, name, source)))
		r = r.WithContext(context.WithValue(ctx, e2bCtxKey{}, caller))
		w := httptest.NewRecorder()
		h.e2bTemplateImport(w, r)
		return w
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := request(fmt.Sprintf("alias%d", i), source)
			if w.Code != 202 {
				t.Errorf("import: %d %s", w.Code, w.Body)
			}
		}(i)
	}
	wg.Wait()
	if api.starts != 1 {
		t.Fatalf("same import dispatched %d workers", api.starts)
	}
	var docs []sandboxTemplateDoc
	if err := h.sbTemplates.Find(ctx, pgdoc.M{"projectId": project}, &docs); err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || len(docs[0].names()) != 6 {
		t.Fatalf("alias idempotence: %+v", docs)
	}
	doc := &docs[0]
	build := &doc.Builds[0]
	if buildPinSDKStatus(build) == sandbox.BuildReady {
		t.Fatal("worker dispatch is not usable success")
	}
	digest := "sha256:" + strings.Repeat("d", 64)
	api.result = &sandboximport.Result{Source: source, Image: source, SourceDigest: digest, DestinationDigest: digest, ConfigDigest: digest, Platform: "linux/amd64", User: "app", Workdir: "/srv"}
	// Crash after terminal owner persistence: binding fails once, leaving raw
	// READY but effective BUILDING. A fresh handler must finish without the pod.
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION fail_import_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='bound' THEN RAISE EXCEPTION 'binding failpoint'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_import_binding BEFORE UPDATE ON registry_pin FOR EACH ROW EXECUTE FUNCTION fail_import_binding()`); err != nil {
		t.Fatal(err)
	}
	h.syncImport(ctx, caller, doc, build)
	if build.Import.Registry.State != buildTerminalObserved || build.Status != sandbox.BuildReady || !buildNeedsSync(build) || buildPinSDKStatus(build) != sandbox.BuildBuilding {
		t.Fatalf("terminal handoff did not retain recovery intent: %+v", build)
	}
	api.mu.Lock()
	heldPod := api.pods[sandbox.BuildResourceID(build.BuildID)]
	delete(api.pods, sandbox.BuildResourceID(build.BuildID))
	api.mu.Unlock()
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER fail_import_binding ON registry_pin`); err != nil {
		t.Fatal(err)
	}
	// Crash after BOUND but before final owner-finished publication.
	if _, err := db.Pool.Exec(ctx, `CREATE FUNCTION fail_import_finish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.doc->'builds'->0->'import'->'registryOperation'->>'state'='finished' THEN RAISE EXCEPTION 'owner finish failpoint'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_import_finish BEFORE UPDATE ON sandbox_templates FOR EACH ROW EXECUTE FUNCTION fail_import_finish()`); err != nil {
		t.Fatal(err)
	}
	restarted := &Handler{sbTemplates: db.C("sandbox_templates"), sbBuildPinsDB: db}
	fresh, err := restarted.loadTemplate(ctx, project, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc = fresh
	build = &doc.Builds[0]
	restarted.syncImport(ctx, caller, doc, build)
	if build.Import.Registry.State != buildTerminalObserved || !buildNeedsSync(build) {
		t.Fatal("bound owner lost recovery intent")
	}
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER fail_import_finish ON sandbox_templates`); err != nil {
		t.Fatal(err)
	}
	fresh, err = restarted.loadTemplate(ctx, project, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc = fresh
	build = &doc.Builds[0]
	restarted.syncImport(ctx, caller, doc, build)
	api.mu.Lock()
	api.pods[sandbox.BuildResourceID(build.BuildID)] = heldPod
	api.mu.Unlock()
	if buildPinSDKStatus(build) != sandbox.BuildReady || build.Import.Registry.State != buildPinFinished {
		t.Fatalf("terminal result did not bind: %+v", build)
	}
	store, _ := registrypins.New(db, "registry.example.org")
	pins, err := store.PinsForOperation(ctx, build.Import.Registry.Operation)
	if err != nil || len(pins) != 1 || pins[0].State != registrypins.Bound || pins[0].Ref != source {
		t.Fatalf("binding=%+v %v", pins, err)
	}
	stale := *doc
	stale.Aliases = nil
	if err := h.saveTemplate(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	got, _ := h.loadTemplate(ctx, project, "alias5")
	if got == nil {
		t.Fatal("stale build save removed aliases")
	}
	// Alias deletion cannot delete the shared artifact; it only removes the name.
	r := httptest.NewRequest("DELETE", "/templates/alias5", nil)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("templateID", doc.Aliases[0])
	r = r.WithContext(context.WithValue(context.WithValue(ctx, e2bCtxKey{}, caller), chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	h.e2bTemplateDeleteLocked(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if got, _ := h.loadTemplate(ctx, project, doc.ID); got == nil {
		t.Fatal("alias cleanup removed artifact")
	}
	// Barrier race on a previously absent name: alias attachment and snapshot
	// reservation must have one winner, with no external filesystem/worker effect.
	locked, release, lockDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		lockDone <- db.WithTx(ctx, func(tc context.Context) error {
			if err := db.LockXact(tc, projectTemplateLock(project)); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	snapshotDone := make(chan error, 1)
	aliasDone := make(chan int, 1)
	go func() {
		_, _, _, err := h.reserveSnapshot(ctx, caller, "sbx-source", "name-race", false, nil, sandbox.Template{}, source, map[string]bool{})
		snapshotDone <- err
	}()
	go func() {
		r := httptest.NewRequest("POST", "/templates/aliases", strings.NewReader(`{"aliases":["name-race"]}`))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("templateID", doc.ID)
		r = r.WithContext(context.WithValue(context.WithValue(ctx, e2bCtxKey{}, caller), chi.RouteCtxKey, rc))
		w := httptest.NewRecorder()
		h.e2bTemplateAliases(w, r)
		aliasDone <- w.Code
	}()
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	snapshotErr, aliasCode := <-snapshotDone, <-aliasDone
	if (snapshotErr == nil) == (aliasCode == 200) {
		t.Fatalf("absent-name race needs one winner: snapshot=%v alias=%d", snapshotErr, aliasCode)
	}
	if snapshotErr == nil {
		if reserved, _ := h.loadTemplate(ctx, project, "name-race"); reserved != nil {
			_, _ = h.sbTemplates.DeleteByID(ctx, reserved.ID)
		}
	}
	if api.starts != 1 {
		t.Fatal("name reservation performed external I/O")
	}
	api.mu.Lock()
	api.result = nil
	api.ambiguous = true
	api.mu.Unlock()
	other := strings.Replace(source, strings.Repeat("d", 64), strings.Repeat("e", 64), 1)
	w = request("uncertain", other)
	if w.Code != 503 {
		t.Fatalf("ambiguous import=%d %s", w.Code, w.Body)
	}
	unknown, _ := h.loadTemplate(ctx, project, "uncertain")
	if unknown == nil || unknown.Builds[0].Import.Registry.State != buildPinUnknown {
		t.Fatal("ambiguous operation was not held")
	}
	starts := api.starts
	// All producer reservations use the same absent-name lock. The loser must
	// refuse before its caller could start filesystem capture/upload.
	if _, _, _, err := h.reserveSnapshot(ctx, caller, "sbx-source", doc.Name, false, nil, sandbox.Template{}, source, map[string]bool{}); err == nil {
		t.Fatal("snapshot overwrote imported canonical name")
	}
	if _, _, _, err := h.reserveSnapshot(ctx, caller, "sbx-source", doc.Aliases[len(doc.Aliases)-1], false, nil, sandbox.Template{}, source, map[string]bool{}); err == nil {
		t.Fatal("snapshot overwrote imported alias")
	}
	w = request("uncertain-alias", other)
	if w.Code != 202 || api.starts != starts {
		t.Fatal("uncertain retry redispatched")
	}
	old := time.Now().Add(-2 * sandbox.BuildTimeout)
	unknown.Builds[0].CreatedAt = &old
	h.syncImport(ctx, caller, unknown, &unknown.Builds[0])
	if unknown.Builds[0].Import.Registry.State != buildPinUnknown {
		t.Fatal("deadline cleared UNKNOWN")
	}
}
