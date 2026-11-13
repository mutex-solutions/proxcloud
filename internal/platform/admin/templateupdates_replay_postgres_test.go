package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Opt-in (TEMPLATEUPDATES_TEST_POSTGRES, a disposable loopback DSN): replaying a dead template
// update needs admin:service:manage, and a failed one is refused with 409 (re-enqueue instead),
// while a job that died mid-flight is replayed.
func TestPGTemplateUpdateReplay(t *testing.T) {
	dsn := os.Getenv("TEMPLATEUPDATES_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set TEMPLATEUPDATES_TEST_POSTGRES to a disposable loopback PostgreSQL DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatal("loopback DSN required")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "tur_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := pgdoc.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close(ctx)
		_, _ = adminPool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		adminPool.Close()
	})
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	rec := templateupdates.PGReceipts{DB: db}
	if err := rec.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	payload := templateupdates.VMPayload{Version: 1, Package: "proxcloud-vm-debian-13", Tag: "debian-13-os-20261111000000-aaaaaaaaaaaa",
		Digest: "sha256:" + strings.Repeat("9", 64), DiskDigest: "sha256:" + strings.Repeat("8", 64), DiskBytes: 4096, VirtualSizeBytes: 20 << 30,
		OS: "debian-13", Variant: "os", BuildVersion: "20261111000000", ServiceID: "svc", Node: "pve1", ImportStorage: "imp", DiskStorage: "disk"}
	raw, _ := json.Marshal(payload)
	mk := func(key string, step templateupdates.Step) string {
		id, err := jobs.Enqueue(ctx, durablejobs.TemplateUpdateVM, key, raw, 3)
		if err != nil {
			t.Fatal(err)
		}
		if err := rec.Save(ctx, &templateupdates.Receipt{ID: id, Kind: "vm", Payload: payload, Step: step}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),last_error='x' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	failed, midflight := mk("k-failed", templateupdates.StepFailed), mk("k-mid", templateupdates.StepUploading)

	// Sandbox mirror jobs replay by their own receipt.
	sbx := templateupdates.PGSandbox{DB: db}
	if err := sbx.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	sp := templateupdates.SandboxPayload{Version: 1, Action: templateupdates.SandboxMirror, ServiceID: "sbx", Name: "base", Registry: "192.0.2.226",
		Tag: "20261112-9d00563", Digest: "sha256:" + strings.Repeat("7", 64)}
	sraw, _ := json.Marshal(sp)
	mkSbx := func(key string, step templateupdates.Step) string {
		id, err := jobs.Enqueue(ctx, durablejobs.TemplateUpdateSandbox, key, sraw, 3)
		if err != nil {
			t.Fatal(err)
		}
		if err := sbx.Save(ctx, &templateupdates.SandboxReceipt{ID: id, Kind: "sandbox", Payload: sp, Step: step}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',finished_at=clock_timestamp(),last_error='x' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	sbxFailed, sbxMid := mkSbx("s-failed", templateupdates.SbxFailed), mkSbx("s-mid", templateupdates.SbxMirroring)

	h := &Handler{adminIssuer: "operator-realm"}
	h.SetDurableJobs(jobs, nil, func(j *durablejobs.Job) error {
		if j.Kind == durablejobs.TemplateUpdateSandbox {
			_, err := templateupdates.DecodeSandboxPayload(j.Payload)
			return err
		}
		_, err := templateupdates.DecodeVMPayload(j.Payload)
		return err
	})
	h.SetTemplateUpdates(&templateupdates.Service{Receipts: rec, Jobs: jobs, DB: db, Sandbox: sbx})
	r := chi.NewRouter()
	r.Post("/admin/durable-jobs/{id}/replay", h.durableReplay)
	body := `{"reason":"retry after a fix"}`
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	if w := tuDo(r, "POST", "/admin/durable-jobs/"+failed+"/replay", body, op); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "new import") {
		t.Fatalf("failed replay: %d %s", w.Code, w.Body.String())
	}
	if w := tuDo(r, "POST", "/admin/durable-jobs/"+midflight+"/replay", body, op); w.Code != http.StatusOK {
		t.Fatalf("mid-flight replay: %d %s", w.Code, w.Body.String())
	}
	if j, _ := jobs.Get(ctx, midflight); j.State != "pending" {
		t.Fatalf("not re-queued: %+v", j)
	}
	if w := tuDo(r, "POST", "/admin/durable-jobs/"+sbxFailed+"/replay", body, op); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "start a new one") {
		t.Fatalf("failed sandbox replay: %d %s", w.Code, w.Body.String())
	}
	if w := tuDo(r, "POST", "/admin/durable-jobs/"+sbxMid+"/replay", body, op); w.Code != http.StatusOK {
		t.Fatalf("mid-flight sandbox replay: %d %s", w.Code, w.Body.String())
	}
	if ext, err := h.adminRequiresServiceManage(ctx, sbxFailed); err != nil || !ext {
		t.Fatalf("sandbox replay does not require admin:service:manage: %v %v", ext, err)
	}
	// Permission: a billing-only operator cannot replay a template update.
	if ext, err := h.adminRequiresServiceManage(ctx, failed); err != nil || !ext {
		t.Fatalf("template update replay does not require admin:service:manage: %v %v", ext, err)
	}
	// A missing or malformed id is not an error (billing key applies; Replay then refuses).
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if ext, err := h.adminRequiresServiceManage(ctx, id); err != nil || ext {
			t.Fatalf("missing/malformed id %q: %v %v", id, ext, err)
		}
	}
	// A lookup failure fails closed instead of falling back to the billing key.
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := h.adminRequiresServiceManage(cancelled, failed); err == nil {
		t.Fatal("lookup error must be returned so replay fails closed")
	}
	// ClaimDead never re-acquires a job whose cleanup already finished.
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET last_error='cleaned_up' WHERE id=$1`, failed); err != nil {
		t.Fatal(err)
	}
	if j, err := jobs.ClaimDead(ctx, failed, time.Minute); err != nil || j != nil {
		t.Fatalf("cleaned_up job re-claimed: %+v %v", j, err)
	}
}
