//go:build cephlive

package client

// Live drill against a real Ceph RGW. Excluded from normal builds/tests by the `cephlive` tag.
// NO credentials live in this file — everything comes from the environment:
//
//	CEPH_S3_ENDPOINT   e.g. https://s3.example.com
//	CEPH_ADMIN_URL     e.g. https://s3.example.com/admin
//	CEPH_ADMIN_AK      RGW admin-ops access key
//	CEPH_ADMIN_SK      RGW admin-ops secret key
//	CEPH_REGION        SigV4 region (us-east-1 works on RGW)
//	CEPH_S3_WEBSITE_ENDPOINT  optional; only the website drill needs it
//	CEPH_DRILL_UID     the RGW user to own everything (MUST start with dev/test)
//	CEPH_DRILL_DESTROY set to "yes" to enable TestLiveCephZZCleanup (deletes + purges)
//
// Every bucket this drill creates is OWNED BY CEPH_DRILL_UID, and cleanup purges only that user, so it
// can neither read nor delete another user's buckets.
//
//	go test ./internal/cloud/client/ -tags cephlive -run TestLiveCeph -v

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func liveEnv(t *testing.T) (adminCfg CephConfig, bucket string) {
	t.Helper()
	need := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Skipf("%s not set — skipping live ceph drill", k)
		}
		return v
	}
	uid := need("CEPH_DRILL_UID")
	if !strings.HasPrefix(uid, "dev") && !strings.HasPrefix(uid, "test") {
		t.Fatalf("refusing to drill: CEPH_DRILL_UID %q must start with dev or test", uid)
	}
	// S3 bucket names must be DNS-safe (no underscores); the RGW uid has no such restriction.
	bucket = strings.ReplaceAll(strings.ToLower(uid), "_", "-") + "-b1"
	return CephConfig{
		S3Endpoint: need("CEPH_S3_ENDPOINT"),
		// optional: only the website drill needs it
		S3WebsiteEndpoint: os.Getenv("CEPH_S3_WEBSITE_ENDPOINT"),
		AdminURL:          need("CEPH_ADMIN_URL"),
		Region:            need("CEPH_REGION"),
		AdminAccessKey:    need("CEPH_ADMIN_AK"),
		AdminSecretKey:    need("CEPH_ADMIN_SK"),
		RGWUID:            uid,
		// 10 GiB quota — proves the quota leg without reserving anything meaningful.
		DefaultQuotaBytes: 10 << 30,
	}, bucket
}

// projectClient runs EnsureCephUser (idempotent) and returns an admin client + a project-keyed client.
func liveClients(t *testing.T, ctx context.Context) (admin, proj *Client, bucket string) {
	t.Helper()
	cfg, bucket := liveEnv(t)
	admin, err := NewCephS3(ctx, cfg)
	if err != nil {
		t.Fatalf("NewCephS3(admin): %v", err)
	}
	ak, sk, err := admin.EnsureCephUser(ctx, "proxcloud-"+cfg.RGWUID)
	if err != nil {
		t.Fatalf("EnsureCephUser: %v", err)
	}
	if ak == "" || sk == "" {
		t.Fatal("EnsureCephUser returned empty keys")
	}
	t.Logf("rgw user %s ready (access key %s…)", cfg.RGWUID, ak[:4])
	pcfg := cfg
	pcfg.ProjectAccessKey, pcfg.ProjectSecretKey = ak, sk
	proj, err = NewCephS3(ctx, pcfg)
	if err != nil {
		t.Fatalf("NewCephS3(project): %v", err)
	}
	return admin, proj, bucket
}

// TestLiveCephAProvisionAndCRUD is the non-destructive leg: provision the tenant-user, then exercise the
// whole bucket + object surface. Leaves the bucket in place for inspection.
func TestLiveCephAProvisionAndCRUD(t *testing.T) {
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)

	// --- bucket create (idempotent-ish: RGW returns BucketAlreadyOwnedByYou on re-run) ---
	if _, err := proj.CreateBucket(ctx, CreateBucketOpts{Name: bucket}); err != nil && !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Logf("bucket %s ready", bucket)

	// --- object upload ---
	payload := []byte("hello from proxcloud ceph drill\n")
	if err := proj.UploadBucketObject(ctx, bucket, "hello.txt", "text/plain", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("UploadBucketObject: %v", err)
	}
	if err := proj.CreateFolder(ctx, bucket, "folder"); err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	nested := []byte("nested\n")
	if err := proj.UploadBucketObject(ctx, bucket, "folder/nested.txt", "text/plain", int64(len(nested)), bytes.NewReader(nested)); err != nil {
		t.Fatalf("UploadBucketObject(nested): %v", err)
	}

	// --- list root: expect hello.txt (file) + folder/ (dir) ---
	objs, err := proj.ListBucketObjects(ctx, bucket, "")
	if err != nil {
		t.Fatalf("ListBucketObjects(root): %v", err)
	}
	var sawFile, sawDir bool
	for _, o := range objs {
		if o["directory"].(bool) && o["displayName"] == "folder" {
			sawDir = true
		}
		if !o["directory"].(bool) && o["name"] == "hello.txt" {
			sawFile = true
		}
	}
	if !sawFile || !sawDir {
		t.Fatalf("root listing missing file/dir: %+v", objs)
	}

	// --- list inside folder: expect nested.txt with trimmed displayName ---
	inner, err := proj.ListBucketObjects(ctx, bucket, "folder")
	if err != nil {
		t.Fatalf("ListBucketObjects(folder): %v", err)
	}
	var sawNested bool
	for _, o := range inner {
		if o["name"] == "folder/nested.txt" && o["displayName"] == "nested.txt" {
			sawNested = true
		}
	}
	if !sawNested {
		t.Fatalf("folder listing missing nested.txt: %+v", inner)
	}

	// --- download round-trip ---
	got, ct, err := proj.DownloadObject(ctx, bucket, "hello.txt")
	if err != nil {
		t.Fatalf("DownloadObject: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("download mismatch: %q", got)
	}
	t.Logf("download ok (%d bytes, %s)", len(got), ct)

	// --- admin-ops stats (the billing meter) ---
	stats, err := admin.GetBucket(ctx, bucket)
	if err != nil {
		t.Fatalf("GetBucket(admin stats): %v", err)
	}
	t.Logf("GetBucket stats: objectCount=%v sizeInBytes=%v sizeInGb=%v",
		stats["objectCount"], stats["sizeInBytes"], stats["sizeInGb"])
	if stats["objectCount"].(int64) < 2 {
		t.Fatalf("expected >=2 objects in stats, got %v", stats["objectCount"])
	}

	list, err := admin.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets(admin): %v", err)
	}
	var found bool
	for _, b := range list {
		if b["bucketName"] == bucket {
			found = true
			t.Logf("ListBuckets: %s objectCount=%v sizeInBytes=%v", bucket, b["objectCount"], b["sizeInBytes"])
		}
	}
	if !found {
		t.Fatalf("ListBuckets did not return %s: %+v", bucket, list)
	}

	// --- ACL: private → public → private ---
	if pub, err := proj.IsBucketPublic(ctx, bucket); err != nil {
		t.Fatalf("IsBucketPublic: %v", err)
	} else if pub {
		t.Fatal("fresh bucket reported public")
	}
	if err := proj.SetBucketRead(ctx, bucket, ".r:*,.rlistings"); err != nil {
		t.Fatalf("SetBucketRead(public): %v", err)
	}
	if pub, err := proj.IsBucketPublic(ctx, bucket); err != nil || !pub {
		t.Fatalf("bucket not public after SetBucketRead: pub=%v err=%v", pub, err)
	}
	if err := proj.SetBucketRead(ctx, bucket, ""); err != nil {
		t.Fatalf("SetBucketRead(private): %v", err)
	}
	if pub, _ := proj.IsBucketPublic(ctx, bucket); pub {
		t.Fatal("bucket still public after private")
	}
	t.Log("ACL public/private round-trip ok")

	// --- bucket API URLs ---
	s3urls, err := proj.BucketAPIs(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketAPIs: %v", err)
	}
	t.Logf("BucketAPIs s3=%v", s3urls)
}

// TestLiveCephBWebsite drills static website hosting end-to-end, including an ANONYMOUS https fetch of the
// published page — the only thing that actually proves the public-read bucket policy took effect (a
// public-read ACL alone would still 403 the object). Runs after the CRUD leg (name sorts A < B < ZZ).
func TestLiveCephBWebsite(t *testing.T) {
	if os.Getenv("CEPH_S3_WEBSITE_ENDPOINT") == "" {
		t.Skip("CEPH_S3_WEBSITE_ENDPOINT not set — skipping website drill")
	}
	ctx := context.Background()
	_, proj, bucket := liveClients(t, ctx)

	if _, err := proj.CreateBucket(ctx, CreateBucketOpts{Name: bucket}); err != nil && !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("CreateBucket: %v", err)
	}
	page := []byte("<h1>proxcloud ceph website drill</h1>\n")
	if err := proj.UploadBucketObject(ctx, bucket, "index.html", "text/html", int64(len(page)), bytes.NewReader(page)); err != nil {
		t.Fatalf("upload index.html: %v", err)
	}

	// Before enabling: anonymous fetch of the object over the S3 endpoint must be denied.
	if code, _ := anonGet(t, strings.TrimRight(os.Getenv("CEPH_S3_ENDPOINT"), "/")+"/"+bucket+"/index.html"); code == 200 {
		t.Fatalf("object is anonymously readable BEFORE enabling the website (code %d)", code)
	}

	site, err := proj.EnableBucketWebsite(ctx, bucket, "index.html", "error.html")
	if err != nil {
		t.Fatalf("EnableBucketWebsite: %v", err)
	}
	// Enabling made every object anonymously readable. ALWAYS revert, even if an assertion below fails —
	// a failed drill must never leave a public bucket behind. t.Cleanup runs on t.Fatalf too.
	t.Cleanup(func() {
		if err := proj.DisableBucketWebsite(context.Background(), bucket); err != nil {
			t.Errorf("cleanup: DisableBucketWebsite: %v", err)
		}
	})
	if !site.Enabled || !site.PublicObjects {
		t.Fatalf("website state = %+v; want enabled + publicObjects", site)
	}
	if site.URL == "" {
		t.Fatal("no website URL resolved")
	}
	t.Logf("website enabled: %s (index=%s error=%s)", site.URL, site.IndexDocument, site.ErrorDocument)

	// The real proof: fetch the site anonymously, no credentials at all.
	code, body := anonGet(t, site.URL)
	if code != 200 {
		t.Fatalf("anonymous GET %s = %d (want 200); body=%.200s", site.URL, code, body)
	}
	if !strings.Contains(body, "proxcloud ceph website drill") {
		t.Fatalf("website served unexpected body: %.200s", body)
	}
	t.Logf("anonymous GET %s → 200, served index.html", site.URL)

	// Round-trip: disabling must both stop the website and close anonymous access again.
	if err := proj.DisableBucketWebsite(ctx, bucket); err != nil {
		t.Fatalf("DisableBucketWebsite: %v", err)
	}
	after, err := proj.GetBucketWebsite(ctx, bucket)
	if err != nil {
		t.Fatalf("GetBucketWebsite after disable: %v", err)
	}
	if after.Enabled || after.PublicObjects {
		t.Fatalf("website still enabled after disable: %+v", after)
	}
	if code, _ := anonGet(t, strings.TrimRight(os.Getenv("CEPH_S3_ENDPOINT"), "/")+"/"+bucket+"/index.html"); code == 200 {
		t.Errorf("object still anonymously readable after DisableBucketWebsite (policy not removed)")
	}
	t.Log("website disabled + public-read policy removed")
}

// anonGet performs a credential-free GET (fresh client, no auth, no redirect following surprises).
func anonGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Logf("anonymous GET %s failed at transport: %v", url, err)
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestLiveCephZZCleanup is the DESTRUCTIVE leg — deletes the drill objects, bucket, and purges the
// tenant-user. Gated: it no-ops unless CEPH_DRILL_DESTROY=yes. Name sorts last so -run TestLiveCeph
// executes it after the CRUD leg.
func TestLiveCephZZCleanup(t *testing.T) {
	if os.Getenv("CEPH_DRILL_DESTROY") != "yes" {
		t.Skip("CEPH_DRILL_DESTROY != yes — skipping destructive cleanup")
	}
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)

	if err := proj.DeleteBucketObject(ctx, bucket, "hello.txt"); err != nil {
		t.Errorf("DeleteBucketObject(hello.txt): %v", err)
	}
	if err := proj.DeleteBucketObject(ctx, bucket, "folder"); err != nil { // prefix delete: folder + contents
		t.Errorf("DeleteBucketObject(folder): %v", err)
	}
	left, err := proj.ListBucketObjects(ctx, bucket, "")
	if err != nil {
		t.Errorf("ListBucketObjects after delete: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("bucket not empty after object deletes: %+v", left)
	}
	if err := proj.DeleteBucket(ctx, bucket); err != nil {
		t.Errorf("DeleteBucket: %v", err)
	}
	t.Logf("bucket %s deleted", bucket)

	if err := admin.PurgeCephUser(ctx); err != nil {
		t.Fatalf("PurgeCephUser: %v", err)
	}
	t.Log("rgw user purged")
}
