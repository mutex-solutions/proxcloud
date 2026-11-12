package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

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

	h := &Handler{adminIssuer: "operator-realm"}
	h.SetDurableJobs(jobs, nil, func(j *durablejobs.Job) error { _, err := templateupdates.DecodeVMPayload(j.Payload); return err })
	h.SetTemplateUpdates(&templateupdates.Service{Receipts: rec, Jobs: jobs, DB: db})
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
	// Permission: a billing-only operator cannot replay a template update.
	if !h.adminRequiresServiceManage(ctx, failed) {
		t.Fatal("template update replay does not require admin:service:manage")
	}
}
