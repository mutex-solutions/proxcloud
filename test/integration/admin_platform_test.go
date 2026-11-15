//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
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
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errors":{"fake":"nothing here"}}`))
	}))
	t.Cleanup(fakePVE.Close)
	enc := textcrypt.New("test-key")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform", "importStorage": "nas-templates",
		"network": pgdoc.M{"cidrPool": "10.100.0.0/16", "blockedCidrs": []string{"10.200.200.0/25"}}},
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
	mu.Lock()
	sent := strings.Join(auths, "\n")
	mu.Unlock()
	if !strings.Contains(sent, "PVEAPIToken=proxcloud@pve!platform="+portalToken) || !strings.Contains(sent, "PVEAPIToken=capmox@pve!capi="+plantedCAPI) || !strings.Contains(sent, "PVEAPIToken=csi@pve!csi="+plantedCSI) {
		t.Fatal("the preflight did not use the stored platform tokens")
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
	if err := store.SaveRun(ctx, &platformlife.Run{ID: "pr-1", InstallationID: in.ID, Kind: platformlife.RunApply, State: platformlife.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if a := call("admin-sub", http.MethodPut, one, platformBody("10.200.200.1", 8, 3), http.StatusConflict); code(a) != "run_active" {
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
	var events []pgdoc.M
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) { // the middleware writes asynchronously
		events = nil
		if err := db.C("auditEvent").Find(ctx, pgdoc.M{"resourceType": "PLATFORM_INSTALLATION"}, &events); err != nil {
			t.Fatal(err)
		}
		if len(events) >= 4 || time.Now().After(deadline) {
			break
		}
	}
	if len(events) != 4 { // create, credentials, two updates; the preflight and every refusal are not audited
		t.Fatalf("%d audit events, want 4: %v", len(events), events)
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
	if err != nil || run == nil || run.State != platformlife.RunQueued || len(run.Stages) != 11 || run.RequestedBy != "admin-sub" {
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
	run, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunApply, "admin-sub", "")
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
	if _, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunApply, "admin-sub", "remove lab"); !errors.Is(err, platformlife.ErrBadConfirm) {
		t.Fatalf("confirm on an apply: %v", err)
	}
	if _, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunRemove, "admin-sub", "remove other"); !errors.Is(err, platformlife.ErrBadConfirm) {
		t.Fatalf("a wrong confirmation: %v", err)
	}
	if r, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunRemove, "admin-sub", "remove lab"); err != nil || !r.Confirmed {
		t.Fatalf("confirmed remove: %+v %v", r, err)
	}
}
