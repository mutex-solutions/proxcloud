package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const runtimePGHost = "registry.test"
const runtimePGTemplate = "0123456789abcdef0123"

// Opt-in tests never pull images/start services. An explicitly supplied disposable
// loopback PostgreSQL supplies a database per test and TWO independent pools.
func runtimePGDatabase(t *testing.T) (*pgdoc.DB, *pgdoc.DB) {
	t.Helper()
	dsn := os.Getenv("RUNTIME_CREATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set RUNTIME_CREATE_TEST_POSTGRES to disposable loopback PostgreSQL")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() != "127.0.0.1" || u.Port() == "" {
		t.Fatal("RUNTIME_CREATE_TEST_POSTGRES must be explicit loopback URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "runtime_create_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u.Path = "/" + name
	a, err := pgdoc.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := pgdoc.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.Close(ctx)
		_ = b.Close(ctx)
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return a, b
}

type runtimeCreateAPI struct {
	sandbox.K8sAPI
	mu              sync.Mutex
	cr, pod         map[string]any
	db, independent *pgdoc.DB
	applies, calls  int
	failApply       bool
	before          func(context.Context, map[string]any) error
	podReads        chan<- struct{}
	allowPodRead    <-chan struct{}
}

func runtimeCopy(in map[string]any) map[string]any {
	b, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func runtimeDig(in map[string]any, path ...string) any {
	var v any = in
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}
func runtimeStr(in map[string]any, path ...string) string {
	s, _ := runtimeDig(in, path...).(string)
	return s
}
func (a *runtimeCreateAPI) EnsureNamespace(ctx context.Context, _ string, _ map[string]string) error {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.before != nil {
		return a.before(ctx, nil)
	}
	return nil
}
func (a *runtimeCreateAPI) ApplyResourceQuota(context.Context, map[string]any) error { return nil }
func (a *runtimeCreateAPI) ListCRs(context.Context, string, string, string, string, string) ([]map[string]any, error) {
	return nil, nil
}
func (a *runtimeCreateAPI) ApplyCR(ctx context.Context, _, _, plural string, obj map[string]any) error {
	if plural != sandbox.CRPlural {
		return nil
	}
	if _, err := a.db.Tx(ctx); err != nil {
		return fmt.Errorf("existing rotation transaction missing: %w", err)
	}
	if err := pgdoc.RejectInheritedTransaction(ctx); !errors.Is(err, pgdoc.ErrInheritedTransaction) {
		return errors.New("effect ran outside original start transaction")
	}
	ann := runtimeDig(obj, "metadata", "annotations").(map[string]any)
	op, _ := ann["proxcloud.io/runtime-operation"].(string)
	if op != "" {
		// Independent pool MUST see owner, one pin and authorized marker committed
		// BEFORE any callback/K8s call in the rotation transaction.
		var n int
		if err := a.independent.Pool.QueryRow(context.Background(), `SELECT count(*) FROM registry_pin p JOIN registry_runtime_dispatch d ON d.operation=p.operation JOIN sandbox_runtime_start o ON o.id=p.operation::text WHERE p.operation=$1 AND p.state='pending' AND d.state='authorized' AND o.doc->>'state'='dispatching'`, op).Scan(&n); err != nil || n != 1 {
			return fmt.Errorf("standalone pin not committed before dispatch: n=%d err=%v", n, err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applies++
	cr := runtimeCopy(obj)
	md := cr["metadata"].(map[string]any)
	md["uid"], md["resourceVersion"] = "actual-cr-uid", "1"
	cr["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)}}}
	a.cr = cr
	pt := runtimeDig(cr, "spec", "podTemplate").(map[string]any)
	pmd := runtimeCopy(pt["metadata"].(map[string]any))
	pmd["name"], pmd["namespace"], pmd["uid"] = md["name"], md["namespace"], "actual-pod-uid"
	pmd["ownerReferences"] = []any{map[string]any{"apiVersion": sandbox.CRGroup + "/" + sandbox.CRVersion, "kind": "Sandbox", "name": md["name"], "uid": md["uid"], "controller": true}}
	image := runtimeDig(pt, "spec", "containers").([]any)[0].(map[string]any)["image"]
	a.pod = map[string]any{"metadata": pmd, "spec": pt["spec"], "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "main", "imageID": "docker-pullable://" + fmt.Sprint(image)}}}}
	if a.failApply {
		return errors.New("lost apply response with secret fixture-payload")
	}
	return nil
}
func (a *runtimeCreateAPI) GetCR(context.Context, string, string, string, string, string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return runtimeCopy(a.cr), nil
}
func (a *runtimeCreateAPI) GetCore(ctx context.Context, _, _, _ string) (map[string]any, error) {
	a.mu.Lock()
	pod, reads, allow := runtimeCopy(a.pod), a.podReads, a.allowPodRead
	a.mu.Unlock()
	if reads != nil {
		select {
		case reads <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-allow:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return pod, nil
}
func (a *runtimeCreateAPI) DeleteCR(context.Context, string, string, string, string, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cr = nil
	a.pod = nil
	return nil
}
func (a *runtimeCreateAPI) MergePatchCR(_ context.Context, _, _, _, _, _ string, patch map[string]any) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var merge func(map[string]any, map[string]any)
	merge = func(dst, src map[string]any) {
		for k, v := range src {
			if sm, ok := v.(map[string]any); ok {
				dm, ok := dst[k].(map[string]any)
				if !ok {
					dm = map[string]any{}
					dst[k] = dm
				}
				merge(dm, sm)
			} else if v == nil {
				delete(dst, k)
			} else {
				dst[k] = v
			}
		}
	}
	merge(a.cr, patch)
	return runtimeCopy(a.cr), nil
}

type runtimePGFixture struct {
	h     *Handler
	a, b  *pgdoc.DB
	api   *runtimeCreateAPI
	c     *e2bCaller
	store *registrypins.Store
	image string
}

func newRuntimePGFixture(t *testing.T) *runtimePGFixture {
	t.Helper()
	ctx := context.Background()
	a, b := runtimePGDatabase(t)
	if err := registrypins.EnsureSchema(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSandboxRuntimePins(ctx, a); err != nil {
		t.Fatal(err)
	}
	enc := textcrypt.New("runtime-fixture-key")
	esSvc := externalservice.NewService(externalservice.NewRepo(a), enc)
	proj := &Project{ID: "project1", Status: StatusEnabled}
	if err := a.C("project").Upsert(ctx, proj.ID, proj); err != nil {
		t.Fatal(err)
	}
	image := runtimePGHost + "/t/project1/" + runtimePGTemplate + "@sha256:" + strings.Repeat("a", 64)
	es := &externalservice.ExternalService{ID: "sandbox", Type: "CLOUD", Status: "PUBLIC", Config: map[string]any{"provider": "sandbox", "region": "local", "sandbox": map[string]any{
		"templateRegistry": runtimePGHost + "/t", "envd": true, "domain": "sandbox.test", "templates": map[string]any{"base": map[string]any{"image": runtimePGHost + "/library/base:latest", "cpu": 1, "memoryGiB": 1}, "managed-builtin": map[string]any{"image": image, "cpu": 1, "memoryGiB": 1}},
	}}, Secret: enc.EncryptObject(map[string]any{"kubeconfig": "fixture-only", "proxyWakeToken": "old-wake"})}
	if err := a.C("externalService").Upsert(ctx, es.ID, es); err != nil {
		t.Fatal(err)
	}
	es, err := esSvc.Get(ctx, es.ID)
	if err != nil {
		t.Fatal(err)
	}
	api := &runtimeCreateAPI{db: a, independent: b}
	h := &Handler{audit: audit.NewService(audit.NewRepo(a), slog.Default()), sbTemplates: a.C("sandbox_templates"), sbRuntimeDB: a, sbSafety: a.C("sandbox_safety"), esSvc: esSvc, cloud: cloud.NewRepo(a), sandboxGates: func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
		return []sandbox.Gate{{Name: "fake", OK: true}}
	}}
	if err := a.C("auditEvent").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.sbSafety.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	h.SetSandbox(func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.NewWithAPI(api, es.SandboxConfig(), es.ID), nil
	})
	tmpl := &sandboxTemplateDoc{ID: runtimePGTemplate, ProjectID: proj.ID, Name: "canonical", Aliases: []string{"requested"}, CPU: 1, MemoryGiB: 1, Tags: map[string]string{"stable": "build-one", "default": "build-one"}, Builds: []templateBuild{{BuildID: "build-one", Status: sandbox.BuildReady, Image: image, StartCmd: "serve", ReadyCmd: "ready", User: "tenant", Workdir: "/home/tenant", StartAsRoot: true}}}
	if _, err = h.sbTemplates.InsertOne(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	ss := sandbox.NewWithAPI(api, es.SandboxConfig(), es.ID)
	store, err := registrypins.New(a, runtimePGHost)
	if err != nil {
		t.Fatal(err)
	}
	return &runtimePGFixture{h: h, a: a, b: b, api: api, c: &e2bCaller{user: &user.User{Sub: "fixture"}, proj: proj, es: es, ss: ss}, store: store, image: image}
}
func (f *runtimePGFixture) spec(t *testing.T, ref string) sandbox.Spec {
	t.Helper()
	custom, name, source, err := f.h.resolveRuntimeCreateTemplate(context.Background(), f.c.proj.ID, ref)
	if err != nil {
		t.Fatal(err)
	}
	s := sandbox.Spec{ProjectID: f.c.proj.ID, Template: ref, Custom: custom, RuntimeSource: source, Env: map[string]string{"USER_INPUT": "private-env-value"}, TimeoutSeconds: 300}
	if custom != nil {
		s.Template = name
	}
	q := f.c.es.SandboxProjectQuota(f.c.proj.Quota)
	s.Quota = &q
	if err = s.Validate(f.c.ss.Config()); err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *runtimePGFixture) owner(t *testing.T, id string) *runtimeStartOwner {
	t.Helper()
	o, err := f.h.findRuntimeOwner(context.Background(), f.c.proj.ID, f.c.es.ID, id)
	if err != nil || o == nil {
		t.Fatalf("owner %v %v", o, err)
	}
	return o
}
func (f *runtimePGFixture) pins(t *testing.T, o *runtimeStartOwner) []registrypins.Pin {
	t.Helper()
	p, err := f.store.PinsForOperation(context.Background(), o.Operation)
	if err != nil || len(p) != 1 {
		t.Fatalf("pins %+v %v", p, err)
	}
	return p
}

func TestRuntimeCreatePostgresNativeAndE2BBindAndRetainAfterDelete(t *testing.T) {
	for _, mode := range []string{"native", "e2b"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimePGFixture(t)
			ctx := context.Background()
			// Assert standalone commit even on the FIRST namespace call, not only Apply.
			f.api.before = func(tc context.Context, _ map[string]any) error {
				var n int
				if _, err := f.a.Tx(tc); err != nil {
					return err
				}
				if err := f.b.Pool.QueryRow(ctx, `SELECT count(*) FROM registry_pin p JOIN sandbox_runtime_start o ON o.id=p.operation::text WHERE p.state='pending' AND o.doc->>'state'='dispatching'`).Scan(&n); err != nil || n != 1 {
					return fmt.Errorf("namespace before pin commit: %d %v", n, err)
				}
				return nil
			}
			w := httptest.NewRecorder()
			if mode == "native" {
				r := httptest.NewRequest("POST", "/cloud", nil)
				f.h.sandboxCreate(w, r, f.c.user, f.c.proj, f.c.es, providers.CreateRequest{Type: cloud.TypeSandbox, Data: map[string]any{"template": "requested:stable", "env": map[string]string{"USER_INPUT": "private-env-value"}}})
			} else {
				r := httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{"templateID":"requested:stable","envVars":{"USER_INPUT":"private-env-value"}}`))
				r = r.WithContext(context.WithValue(ctx, e2bCtxKey{}, f.c))
				f.h.e2bCreate(w, r)
			}
			if w.Code != 200 && w.Code != 201 {
				t.Fatalf("%s %d %s", mode, w.Code, w.Body)
			}
			var owners []runtimeStartOwner
			if err := f.h.runtimeOwners().Find(ctx, pgdoc.M{}, &owners); err != nil || len(owners) != 1 {
				t.Fatal(owners, err)
			}
			o := &owners[0]
			ps := f.pins(t, o)
			if o.Source.RequestedAlias != "requested:stable" || o.Source.BuildID != "build-one" || o.Source.Generation != "" || o.State != "observed" || ps[0].State != registrypins.Bound || ps[0].BindingUID != "actual-pod-uid" || ps[0].BindingGeneration != o.Operation.Generation || o.CRUID != "actual-cr-uid" || o.Spec.Image != f.image {
				t.Fatalf("owner %+v pins %+v", o, ps)
			}
			var raw string
			if err := f.a.Pool.QueryRow(ctx, `SELECT doc::text FROM sandbox_runtime_start WHERE id=$1`, o.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-env-value", "old-wake", "kubeconfig", "traffic-token", "PROXCLOUD_ENVD_ACCESS_TOKEN"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("owner stored raw private payload %s", secret)
				}
			}
			if o.Pins[0].ID != ps[0].ID || o.Pins[0].Operation != o.Operation || o.Pins[0].pin() != ps[0] {
				t.Fatal("receipt DTO lost identity on PG roundtrip")
			}
			if err := f.c.ss.DeleteSandbox(ctx, f.c.proj.ID, o.SandboxID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.a.C("cloudResource").DeleteMany(ctx, pgdoc.M{}); err != nil {
				t.Fatal(err)
			}
			if f.owner(t, o.SandboxID).State != "observed" || f.pins(t, o)[0].State != registrypins.Bound {
				t.Fatal("physical delete/billing archive released owner")
			}
			prune := registrypins.Operation{ID: uuid.NewString(), ProjectID: f.c.proj.ID, Owner: "test-prune", Generation: uuid.NewString()}
			if _, err := f.store.BeginPrune(ctx, prune, f.image); !errors.Is(err, registrypins.ErrOperatorHold) {
				t.Fatal("deleted consumer lost conservative hold", err)
			}
		})
	}
}

func TestRuntimeCreatePostgresRotationRefusalCancelsUnusedAndApplyUnknownCannotReplay(t *testing.T) {
	for _, mode := range []string{"rotation", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimePGFixture(t)
			ctx := context.Background()
			s := f.spec(t, "requested:stable")
			if mode == "rotation" {
				if err := f.a.C("sandbox_safety").Upsert(ctx, "rotation:sandbox", pgdoc.M{"active": true}); err != nil {
					t.Fatal(err)
				}
			} else {
				f.api.failApply = true
			}
			_, err := f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s)
			if !errors.Is(err, errRuntimePinState) || strings.Contains(err.Error(), "fixture-payload") {
				t.Fatalf("unsafe error %v", err)
			}
			o := f.owner(t, s.ID)
			p := f.pins(t, o)[0]
			want, state, n := buildPinFailed, registrypins.Released, 0
			if mode == "ambiguous" {
				want, state, n = buildPinUnknown, registrypins.Unknown, 1
			}
			if o.State != want || p.State != state || f.api.applies != n {
				t.Fatalf("owner %+v pin %+v applies %d", o, p, f.api.applies)
			}
			// Restart/no reissue for the exact owner; wrong local envelope or copies
			// cannot get any K8s call, nor can a newly composed Store mint a proof.
			if _, err = f.store.AuthorizeRuntimeDispatch(ctx, o.Operation, o.Spec); !errors.Is(err, registrypins.ErrConflict) {
				t.Fatal("reminted", err)
			}
			calls := f.api.calls
			s.ID = o.SandboxID
			s.RuntimePins = &registrypins.RuntimeProof{}
			if _, err = f.c.ss.CreateSandbox(ctx, &s); !errors.Is(err, registrypins.ErrConflict) || f.api.calls != calls || f.api.applies != n {
				t.Fatal("retry made another effect", err)
			}
		})
	}
}

func TestRuntimeCreatePostgresCandidateDriftAndCrossDBInheritedTransaction(t *testing.T) {
	for _, mode := range []string{"tag", "settings", "deleting", "secret", "builtin", "owned_tx", "cross_db_tx"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimePGFixture(t)
			ctx := context.Background()
			ref := "requested:stable"
			if mode == "builtin" {
				ref = "managed-builtin"
			}
			s := f.spec(t, ref)
			if mode == "owned_tx" || mode == "cross_db_tx" {
				db := f.a
				if mode == "cross_db_tx" {
					db = f.b
				}
				err := db.WithTx(ctx, func(tc context.Context) error {
					_, _, e := f.h.prepareRuntimeCreate(tc, f.c.proj, f.c.es, f.c.ss, &s)
					return e
				})
				if !errors.Is(err, pgdoc.ErrInheritedTransaction) {
					t.Fatal(err)
				}
				if f.api.calls != 0 {
					t.Fatal("inherited tx touched cluster")
				}
				return
			}
			// Freeze the early candidate, then mutate current PRIMARY before BeginPins.
			// BeginPins still commits Pending first; final locked revalidation refuses.
			if mode == "secret" {
				es, err := f.a.C("externalService").SetByID(ctx, "sandbox", pgdoc.M{"secret.proxyWakeToken": ""}, nil)
				if err != nil || !es {
					t.Fatal(err)
				}
			} else if mode == "builtin" {
				_, err := f.a.C("externalService").SetByID(ctx, "sandbox", pgdoc.M{"config.sandbox.templates.managed-builtin.startCmd": "changed"}, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var d sandboxTemplateDoc
				if _, err := f.h.sbTemplates.Get(ctx, runtimePGTemplate, &d); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "tag":
					d.Tags["stable"] = "different"
				case "settings":
					d.Builds[0].Workdir = "/different"
				case "deleting":
					d.Deleting = true
				}
				if _, err := f.h.sbTemplates.Replace(ctx, d.ID, &d); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s)
			if !errors.Is(err, errRuntimePinState) {
				t.Fatal(err)
			}
			o := f.owner(t, s.ID)
			if o.State != buildPinFailed || f.pins(t, o)[0].State != registrypins.Released || f.api.calls != 0 || f.api.applies != 0 {
				t.Fatalf("drift dispatched owner=%+v calls=%d", o, f.api.calls)
			}
		})
	}
}

func TestRuntimeCreatePostgresConcurrentObserversJoinAndStaleUIDCannotPoison(t *testing.T) {
	f := newRuntimePGFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := f.spec(t, "requested:stable")
	if _, err := f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s); err != nil {
		t.Fatal(err)
	}
	stale := *f.owner(t, s.ID)
	reads, allow := make(chan struct{}, 2), make(chan struct{})
	f.api.mu.Lock()
	f.api.podReads, f.api.allowPodRead = reads, allow
	f.api.mu.Unlock()
	result := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { result <- f.h.observeRuntimeCreate(ctx, f.c, &s) }()
	}
	// Both observers have read the SAME pre-CAS durable owner and independent
	// actual Pod envelope. Release together; no timing sleep or goroutine guesses.
	for i := 0; i < 2; i++ {
		select {
		case <-reads:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(allow)
	for i := 0; i < 2; i++ {
		if err := <-result; err != nil {
			t.Fatalf("same envelope failed to join: %v", err)
		}
	}
	f.api.mu.Lock()
	f.api.podReads, f.api.allowPodRead = nil, nil
	f.api.mu.Unlock()
	o := f.owner(t, s.ID)
	p := f.pins(t, o)[0]
	if o.State != "observed" || p.State != registrypins.Bound || p.Epoch != 2 || o.PodUID != "actual-pod-uid" || f.api.applies != 1 {
		t.Fatalf("concurrent result %+v %+v", o, p)
	}
	// A replacement Pod with the same CR/annotations is not the first immutable
	// Pod UID. Its stale source CAS must lose without Record/MarkUnknown effects.
	f.api.mu.Lock()
	f.api.pod["metadata"].(map[string]any)["uid"] = "replacement-pod-uid"
	f.api.mu.Unlock()
	if err := f.h.observeRuntimeCreate(ctx, f.c, &s); !errors.Is(err, errRuntimePinState) {
		t.Fatal("replacement pod accepted", err)
	}
	f.h.runtimeUnknownDetached(ctx, f.store, &stale)
	f.h.runtimeCreateFailure(ctx, f.c, s.ID)
	current := f.owner(t, s.ID)
	if current.State != "observed" || current.PodUID != o.PodUID || current.Epoch != o.Epoch || f.pins(t, o)[0] != p {
		t.Fatal("stale UID/error changed winner")
	}
	// Deterministically exercise exact old Pending -> next-epoch Bound adoption.
	old := p
	old.State, old.Epoch, old.BindingUID, old.BindingGeneration = registrypins.Pending, 1, "", ""
	ev := registrypins.RuntimePodEvidence{UID: o.PodUID, CRUID: o.CRUID, Operation: o.ID, Nonce: o.Operation.Generation, Spec: o.Spec, Phase: "Running"}
	joined, err := observeRuntimeReceipt(ctx, f.store, old, ev)
	if err != nil || joined != p {
		t.Fatal("old receipt did not read-only join exact winner", joined, err)
	}
	bad := ev
	bad.UID = "replacement-pod-uid"
	if _, err = observeRuntimeReceipt(ctx, f.store, old, bad); err == nil {
		t.Fatal("wrong UID adopted old receipt")
	}
}

func TestRuntimeCreatePostgresProofCopiesOneEffectAndRestartReceipt(t *testing.T) {
	f := newRuntimePGFixture(t)
	ctx := context.Background()
	s := f.spec(t, "requested:stable")
	o, store, err := f.h.prepareRuntimeCreate(ctx, f.c.proj, f.c.es, f.c.ss, &s)
	if err != nil {
		t.Fatal(err)
	}
	copyProof := *s.RuntimePins
	if err = f.h.withSandboxStart(ctx, f.c.es.ID, func(tc context.Context) error { _, e := f.c.ss.CreateSandbox(tc, &s); return e }); err != nil {
		t.Fatal(err)
	}
	before := f.api.calls
	duplicate := sandbox.CloneCreateSpec(s)
	duplicate.RuntimePins = &copyProof
	if _, err = f.c.ss.CreateSandbox(ctx, &duplicate); !errors.Is(err, registrypins.ErrConflict) || f.api.calls != before || f.api.applies != 1 {
		t.Fatal("proof copy sent twice", err)
	}
	// The persisted receipt includes BOTH IDs, so a restart has no need to
	// reacquire or authorize another dispatch just to recover its identity.
	restart := &Handler{sbRuntimeDB: f.b}
	fresh, err := restart.findRuntimeOwner(ctx, o.ProjectID, o.ServiceID, o.SandboxID)
	if err != nil || fresh == nil || len(fresh.Pins) != 1 || fresh.Pins[0].Operation != o.Operation || fresh.Pins[0].ID != o.Pins[0].ID {
		t.Fatal("restart receipt identity loss", fresh, err)
	}
	if _, err = store.AuthorizeRuntimeDispatch(ctx, fresh.Operation, fresh.Spec); !errors.Is(err, registrypins.ErrConflict) {
		t.Fatal("restart reminted", err)
	}
}

func TestRuntimeCreatePostgresExplicitCuratedSkipsAndPruneBothOrders(t *testing.T) {
	for _, mode := range []string{"library", "external", "prune-first", "pin-first"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimePGFixture(t)
			ctx := context.Background()
			if mode == "library" || mode == "external" {
				if mode == "external" {
					if _, err := f.a.C("externalService").SetByID(ctx, "sandbox", pgdoc.M{"config.sandbox.templates.base.image": "external.test/base:latest"}, nil); err != nil {
						t.Fatal(err)
					}
					es, err := f.h.esSvc.Get(ctx, "sandbox")
					if err != nil {
						t.Fatal(err)
					}
					f.c.es = es
					f.c.ss = sandbox.NewWithAPI(f.api, es.SandboxConfig(), es.ID)
				}
				s := f.spec(t, "base")
				row, err := f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.h.waitRuntimeCreateReady(ctx, f.c, row, &s); err != nil {
					t.Fatal(err)
				}
				var n int
				if err = f.a.Pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_runtime_start`).Scan(&n); err != nil || n != 0 {
					t.Fatal("curated skip acquired owner", n, err)
				}
				return
			}
			op := registrypins.Operation{ID: uuid.NewString(), ProjectID: "project1", Owner: "fake-prune", Generation: uuid.NewString()}
			second, err := registrypins.New(f.b, runtimePGHost)
			if err != nil {
				t.Fatal(err)
			}
			s := f.spec(t, "requested:stable")
			if mode == "prune-first" {
				if _, err = second.BeginPrune(ctx, op, f.image); err != nil {
					t.Fatal(err)
				}
				if _, err = f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s); !errors.Is(err, errRuntimePinState) || f.api.applies != 0 || f.api.calls != 0 {
					t.Fatal("active prune permitted dispatch", err)
				}
			} else {
				if _, err = f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s); err != nil {
					t.Fatal(err)
				}
				if _, err = second.BeginPrune(ctx, op, f.image); !errors.Is(err, registrypins.ErrOperatorHold) {
					t.Fatal("pending pin permitted prune", err)
				}
			}
		})
	}
}

func TestRuntimeCreatePostgresFreshEvidenceAdversarialAndUnknownObservationFence(t *testing.T) {
	f := newRuntimePGFixture(t)
	ctx := context.Background()
	s := f.spec(t, "requested:stable")
	row, err := f.h.createSandboxResource(ctx, f.c.proj, f.c.es, f.c.ss, &s)
	if err != nil {
		t.Fatal(err)
	}
	o := f.owner(t, s.ID)
	stale := *o
	original := runtimeCopy(f.api.pod)
	for _, mode := range []string{"uid", "nonce", "image", "imageID", "owner", "phase"} {
		t.Run(mode, func(t *testing.T) {
			f.api.mu.Lock()
			f.api.pod = runtimeCopy(original)
			switch mode {
			case "uid":
				f.api.pod["metadata"].(map[string]any)["uid"] = ""
			case "nonce":
				runtimeDig(f.api.pod, "metadata", "annotations").(map[string]any)["proxcloud.io/runtime-nonce"] = "wrong"
			case "image":
				runtimeDig(f.api.pod, "spec", "containers").([]any)[0].(map[string]any)["image"] = "wrong"
			case "imageID":
				runtimeDig(f.api.pod, "status", "containerStatuses").([]any)[0].(map[string]any)["imageID"] = ""
			case "owner":
				runtimeDig(f.api.pod, "metadata", "ownerReferences").([]any)[0].(map[string]any)["uid"] = "wrong"
			case "phase":
				f.api.pod["status"].(map[string]any)["phase"] = "Succeeded"
			}
			f.api.mu.Unlock()
			if err := f.h.observeRuntimeCreate(ctx, f.c, &s); !errors.Is(err, errRuntimePinState) {
				t.Fatal("bad evidence accepted", err)
			}
			if f.pins(t, o)[0].State != registrypins.Pending {
				t.Fatal("bad evidence mutated pin")
			}
		})
	}
	f.api.mu.Lock()
	f.api.pod = runtimeCopy(original)
	f.api.mu.Unlock()
	if err = f.h.waitRuntimeCreateReady(ctx, f.c, row, &s); err != nil {
		t.Fatal(err)
	}
	f.h.runtimeUnknownDetached(ctx, f.store, &stale)
	if f.owner(t, s.ID).State != "observed" || f.pins(t, o)[0].State != registrypins.Bound {
		t.Fatal("stale dispatch error poisoned observed binding")
	}
	if err = f.h.observeRuntimeCreate(ctx, f.c, &s); err != nil {
		t.Fatal("same evidence not idempotent", err)
	}
	// A protected create whose durable owner vanished is NOT a legacy skip.
	if _, err = f.h.runtimeOwners().DeleteOne(ctx, pgdoc.M{"_id": o.ID}); err != nil {
		t.Fatal(err)
	}
	if err = f.h.observeRuntimeCreate(ctx, f.c, &s); !errors.Is(err, errRuntimePinState) {
		t.Fatal("missing protected owner silently skipped", err)
	}
}
