//go:build integration && cephlive

// App-level E2E for the ceph-s3 object-store backend, exercising the REAL wiring against a live RGW:
//
//	bootstrap (EnableAndBootstrap → BootstrapCephOnto)  → RGW tenant-user + quota + encrypted credential
//	write dispatch (providers.WriteService TypeBucket)  → real bucket, cached CloudResource
//	sync (syncjob.SyncOne → syncCephService)            → BUCKET cache carries usage → billing rates it
//	teardown (TeardownProject)                          → RGW user purged, credential deleted   [gated]
//
// Postgres comes from the shared testcontainer (main_test.go). RGW comes from the environment — the same
// vars the client live drill uses. Everything is scoped to tenant "dev_e2e_<projectId>" (tenantPrefix
// dev_e2e_), a namespace that cannot see any other tenant's buckets.
//
//	go test -tags="integration cephlive" ./test/integration/ -run TestCephE2E -v
//
// The destructive teardown leg only runs with CEPH_E2E_DESTROY=yes.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	cephSvcID  = "svc-ceph"
	cephProjID = "proj-ceph-e2e"
	cephUIDPx  = "dev_e2e_" // config.uidPrefix — keeps drill users obviously non-production
	cephEncKey = "integration-test-encryption-key"
)

// wantUID is the RGW user BootstrapCephOnto must derive: uidPrefix + projectId.
var wantUID = cephUIDPx + cephProjID

// cephBucketName mints a unique bucket per run: the RGW bucket namespace is GLOBAL (no tenants), so a
// fixed name would collide with the previous run's leftover bucket.
func cephBucketName(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "dev-e2e-" + hex.EncodeToString(b[:]) + "-b1"
}

type cephEnv struct{ s3, admin, ak, sk, region string }

func cephEnvOrSkip(t *testing.T) cephEnv {
	t.Helper()
	e := cephEnv{
		s3: os.Getenv("CEPH_S3_ENDPOINT"), admin: os.Getenv("CEPH_ADMIN_URL"),
		ak: os.Getenv("CEPH_ADMIN_AK"), sk: os.Getenv("CEPH_ADMIN_SK"), region: os.Getenv("CEPH_REGION"),
	}
	if e.s3 == "" || e.admin == "" || e.ak == "" || e.sk == "" {
		t.Skip("CEPH_* env not set — skipping live ceph app E2E")
	}
	if e.region == "" {
		e.region = "us-east-1"
	}
	return e
}

// cephRig wires the real repos/services the way cmd/api does.
type cephRig struct {
	db        *pgdoc.DB
	h         *project.Handler
	esSvc     *externalservice.Service
	cloudRepo *cloud.Repo
	projRepo  *project.Repo
	creds     *cephcred.Repo
	env       cephEnv
}

func newCephRig(t *testing.T) *cephRig {
	t.Helper()
	env := cephEnvOrSkip(t)
	db := freshPG(t)
	enc := textcrypt.New(cephEncKey)

	// Seed the ceph-s3 CLOUD provider exactly as the admin create path stores it: free-form config,
	// secret ENCRYPTED at rest (Service.decrypt round-trips it back on read).
	mustInsert(t, db, "externalService", pgdoc.M{
		"_id": cephSvcID, "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"name": "ceph-s3-e2e",
		"config": pgdoc.M{
			"provider": "ceph-s3", "s3Endpoint": env.s3, "adminApiUrl": env.admin,
			"region": env.region, "uidPrefix": cephUIDPx, "defaultQuotaGiB": 10,
			"services": pgdoc.M{"object-store": pgdoc.M{env.region: true}},
		},
		"secret": pgdoc.M{
			"adminAccessKey": enc.Encrypt(env.ak),
			"adminSecretKey": enc.Encrypt(env.sk),
		},
	})
	// A fresh, not-yet-provisioned project.
	mustInsert(t, db, "project", pgdoc.M{
		"_id": cephProjID, "name": "ceph e2e", "status": project.StatusDisabled,
		"organizationId": "org-1", "memberships": []any{}, "services": []any{},
	})

	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	cloudRepo := cloud.NewRepo(db)
	projRepo := project.NewRepo(db)
	projSvc := project.NewService(projRepo, org.NewRepo(db), billing.NewRepo(db), user.NewRepo(db), nil)
	creds := cephcred.New(db, enc)

	h := project.NewHandler(projSvc, project.NewPolicy(nil), nil, nil, nil, nil, cloudRepo, esSvc,
		nil, nil, func() *client.Client { return nil }, env.region)
	h.SetCephCreds(creds, cephcred.NewKeyRepo(db, enc))

	return &cephRig{db: db, h: h, esSvc: esSvc, cloudRepo: cloudRepo, projRepo: projRepo, creds: creds, env: env}
}

// projectClient rebuilds the project-keyed ceph client from the stored credential — the same composition
// the handler's cephClientForProject performs.
func (r *cephRig) projectClient(t *testing.T, ctx context.Context) *client.Client {
	t.Helper()
	cred, err := r.creds.Get(ctx, cephProjID, cephSvcID)
	if err != nil || cred == nil {
		t.Fatalf("credential missing after bootstrap: %v", err)
	}
	es, err := r.esSvc.Get(ctx, cephSvcID)
	if err != nil {
		t.Fatalf("get es: %v", err)
	}
	cc, err := client.NewCephS3(ctx, es.CephConfig(r.env.region, cred.AccessKey, cred.SecretKey, cred.RGWUID))
	if err != nil {
		t.Fatalf("build project ceph client: %v", err)
	}
	return cc
}

func TestCephE2EBootstrapWriteSync(t *testing.T) {
	ctx := context.Background()
	rig := newCephRig(t)

	// --- 1. bootstrap: provisions the RGW tenant-user, stores the credential, attaches the binding ---
	p, err := rig.projRepo.FindByID(ctx, cephProjID)
	if err != nil || p == nil {
		t.Fatalf("load project: %v", err)
	}
	if err := rig.h.EnableAndBootstrap(ctx, p); err != nil {
		t.Fatalf("EnableAndBootstrap: %v", err)
	}
	saved, err := rig.projRepo.FindByID(ctx, cephProjID)
	if err != nil || saved == nil {
		t.Fatalf("reload project: %v", err)
	}
	if saved.Status != project.StatusEnabled {
		t.Errorf("status = %q; want ENABLED", saved.Status)
	}
	if !saved.HasService(cephSvcID) {
		t.Fatalf("project not attached to ceph service: %+v", saved.Services)
	}
	// A ceph-s3 project has NO Keystone tenant (decision #2).
	if ext := saved.ExternalProjectID(cephSvcID); ext != "" {
		t.Errorf("ceph project should have no externalProjectId, got %q", ext)
	}
	binding, _ := saved.Services[0].(map[string]any)
	if binding["provider"] != "ceph-s3" || binding["rgwUid"] != wantUID {
		t.Errorf("binding = %+v; want provider ceph-s3 + rgwUid %s", binding, wantUID)
	}
	t.Logf("bootstrapped: rgwUid=%s", wantUID)

	// --- 2. credential stored, secret ENCRYPTED at rest ---
	cred, err := rig.creds.Get(ctx, cephProjID, cephSvcID)
	if err != nil || cred == nil {
		t.Fatalf("cred: %v", err)
	}
	if cred.AccessKey == "" || cred.SecretKey == "" || cred.RGWUID != wantUID {
		t.Fatalf("bad credential: %+v", cred)
	}
	var raw pgdoc.M
	if found, err := rig.db.C("cephRgwCredential").Get(ctx, cephProjID+"_"+cephSvcID, &raw); err != nil || !found {
		t.Fatalf("raw cred doc: found=%v err=%v", found, err)
	}
	if rawSecret, _ := raw["secretKey"].(string); rawSecret == "" || rawSecret == cred.SecretKey {
		t.Errorf("secretKey is not encrypted at rest (raw == plaintext)")
	}
	// The project doc must NOT carry the secret — it is serialized to the client.
	if strings.Contains(strings.ToLower(dumpJSON(t, saved.Services)), strings.ToLower(cred.SecretKey)) {
		t.Error("secret key leaked into the project document")
	}

	// --- 3. write dispatch: create a bucket through providers.WriteService (the real create path) ---
	cephBucket := cephBucketName(t)
	cc := rig.projectClient(t, ctx)
	// Global bucket namespace → always clean up, or the next run leaks a bucket onto the cluster.
	t.Cleanup(func() {
		bg := context.Background()
		_ = cc.DeleteBucketObject(bg, cephBucket, "")
		if err := cc.DeleteBucket(bg, cephBucket); err != nil {
			t.Logf("cleanup: DeleteBucket %s: %v", cephBucket, err)
		}
	})
	ws := providers.NewWriteService(cc, rig.cloudRepo)
	cr, err := ws.Create(ctx, cephSvcID, rig.env.region, cephProjID, "user-1", providers.CreateRequest{
		Type: cloud.TypeBucket, Data: map[string]any{"bucketName": cephBucket},
	})
	if err != nil {
		t.Fatalf("WriteService.Create bucket: %v", err)
	}
	if cr.ExternalID != cephBucket {
		t.Fatalf("externalId = %q; want %q", cr.ExternalID, cephBucket)
	}
	if cr.Data["storageBackend"] != client.BackendCephS3 {
		t.Errorf("storageBackend = %v; want %s", cr.Data["storageBackend"], client.BackendCephS3)
	}
	t.Logf("bucket created via write dispatch: %s", cephBucket)

	// Put an object so the sync has real usage to meter.
	payload := "app-level e2e payload\n"
	if err := cc.UploadBucketObject(ctx, cephBucket, "e2e.txt", "text/plain", int64(len(payload)), strings.NewReader(payload)); err != nil {
		t.Fatalf("UploadBucketObject: %v", err)
	}

	// --- 4. sync: syncCephService reconciles the BUCKET cache with admin-ops usage (the billing meter) ---
	job := syncjob.New(rig.projRepo, rig.esSvc, rig.cloudRepo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := job.SyncOne(ctx, cephProjID, cephSvcID); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	rows, err := rig.cloudRepo.FindByProjectAndType(ctx, cephProjID, cloud.TypeBucket)
	if err != nil {
		t.Fatalf("read bucket cache: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 cached bucket (create + sync must not duplicate), got %d", len(rows))
	}
	got := rows[0]
	if got.ExternalID != cephBucket || got.ServiceID != cephSvcID {
		t.Errorf("cached row = %+v", got)
	}
	size := toInt64(got.Data["sizeInBytes"])
	objects := toInt64(got.Data["objectCount"])
	if size <= 0 || objects < 1 {
		t.Errorf("sync did not meter usage: sizeInBytes=%v objectCount=%v", got.Data["sizeInBytes"], got.Data["objectCount"])
	}
	if got.Data["storageBackend"] != client.BackendCephS3 {
		t.Errorf("synced storageBackend = %v", got.Data["storageBackend"])
	}
	t.Logf("sync metered usage: objectCount=%d sizeInBytes=%d (billing rates sizeInGb)", objects, size)
}

// TestCephE2EZZTeardown is DESTRUCTIVE: it purges the RGW tenant-user this suite created (and only that
// one). Gated on CEPH_E2E_DESTROY=yes. Depends on the bootstrap test having run in the same DB, so it
// re-bootstraps into its own fresh database first.
func TestCephE2EZZTeardown(t *testing.T) {
	if os.Getenv("CEPH_E2E_DESTROY") != "yes" {
		t.Skip("CEPH_E2E_DESTROY != yes — skipping destructive teardown leg")
	}
	ctx := context.Background()
	rig := newCephRig(t)

	p, _ := rig.projRepo.FindByID(ctx, cephProjID)
	if err := rig.h.EnableAndBootstrap(ctx, p); err != nil {
		t.Fatalf("EnableAndBootstrap: %v", err)
	}
	cephBucket := cephBucketName(t)
	cc := rig.projectClient(t, ctx)
	if _, err := cc.CreateBucket(ctx, client.CreateBucketOpts{Name: cephBucket}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// TeardownProject: the per-resource sweep may fail to delete a NON-EMPTY bucket (S3 refuses, as Swift
	// does) — the ceph branch then purges the tenant-user WITH its data, which is the real cleanup. So a
	// non-nil error here is expected and not fatal; what matters is that the user + credential are gone.
	if err := rig.h.TeardownProject(ctx, cephProjID); err != nil {
		t.Logf("TeardownProject reported (expected for a non-empty bucket): %v", err)
	}
	if cred, err := rig.creds.Get(ctx, cephProjID, cephSvcID); err != nil || cred != nil {
		t.Errorf("credential not deleted: cred=%v err=%v", cred, err)
	}
	// The tenant-user must be gone: an admin-keyed client can no longer list its buckets.
	es, _ := rig.esSvc.Get(ctx, cephSvcID)
	admin, err := client.NewCephS3(ctx, es.CephConfig(rig.env.region, "", "", wantUID))
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	buckets, err := admin.ListBuckets(ctx)
	if err == nil && len(buckets) > 0 {
		t.Errorf("user %s still owns %d buckets after purge: %+v", wantUID, len(buckets), buckets)
	}
	t.Logf("purged: user %s owns no buckets (err=%v)", wantUID, err)
}

// dumpJSON renders a value for substring leak checks.
func dumpJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// toInt64 coerces a jsonb-roundtripped number (float64 / int64 / json.Number) to int64.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
