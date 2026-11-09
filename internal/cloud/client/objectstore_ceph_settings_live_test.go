//go:build cephlive

package client

// Live drill for the bucket-settings surface + per-key grants, against a real RGW.
//
//	go test ./internal/cloud/client/ -tags cephlive -run TestLiveCephD -v
//
// Everything created here is removed by t.Cleanup, including the temp bucket and the temp S3 key.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestLiveCephDBucketSettings(t *testing.T) {
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)
	lockBucket := bucket + "-lock"

	// --- a bucket created WITH object lock (create-time only) ---
	if _, err := proj.CreateBucket(ctx, CreateBucketOpts{Name: lockBucket, ObjectLockEnabled: true}); err != nil {
		t.Fatalf("CreateBucket(objectLock): %v", err)
	}
	t.Cleanup(func() {
		if err := admin.ForceDeleteBucket(context.Background(), lockBucket); err != nil {
			t.Logf("cleanup: ForceDeleteBucket %s: %v", lockBucket, err)
		}
	})

	// versioning is implied by object lock
	s, err := proj.GetBucketSettings(ctx, lockBucket)
	if err != nil {
		t.Fatalf("GetBucketSettings: %v", err)
	}
	if s.Versioning != VersioningEnabled {
		t.Errorf("object-lock bucket versioning = %q; want Enabled", s.Versioning)
	}
	if s.ObjectLock == nil || !s.ObjectLock.Enabled {
		t.Errorf("objectLock = %+v; want enabled", s.ObjectLock)
	}
	t.Logf("create-time object lock: versioning=%s objectLock=%+v indexType=%q placement=%q",
		s.Versioning, s.ObjectLock, s.IndexType, s.PlacementRule)

	// GOVERNANCE default retention accepted; COMPLIANCE refused before we ever hit the wire.
	if err := proj.SetObjectLockDefaults(ctx, lockBucket, ObjectLockGovernance, 1); err != nil {
		t.Errorf("SetObjectLockDefaults(GOVERNANCE): %v", err)
	}
	if err := proj.SetObjectLockDefaults(ctx, lockBucket, ObjectLockCompliance, 1); err == nil {
		t.Error("COMPLIANCE should be refused")
	}

	// --- quota / lifecycle / cors / tags on the ordinary drill bucket ---
	if err := proj.SetBucketQuota(ctx, bucket, 5<<30, 1000, true); err != nil {
		t.Errorf("SetBucketQuota: %v", err)
	}
	if err := proj.SetBucketLifecycle(ctx, bucket, []LifecycleRule{
		{ID: "expire-tmp", Prefix: "tmp/", Enabled: true, ExpirationDays: 30, AbortIncompleteMultipartDays: 7},
	}); err != nil {
		t.Errorf("SetBucketLifecycle: %v", err)
	}
	if err := proj.SetBucketCORS(ctx, bucket, []CORSRule{
		{AllowedMethods: []string{"GET", "HEAD"}, AllowedOrigins: []string{"https://example.com"}, MaxAgeSeconds: 3600},
	}); err != nil {
		t.Errorf("SetBucketCORS: %v", err)
	}
	if err := proj.SetBucketTags(ctx, bucket, map[string]string{"env": "drill"}); err != nil {
		t.Errorf("SetBucketTags: %v", err)
	}
	if err := proj.SetBucketVersioning(ctx, bucket, true); err != nil {
		t.Errorf("SetBucketVersioning: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = proj.SetBucketLifecycle(bg, bucket, nil)
		_ = proj.SetBucketCORS(bg, bucket, nil)
		_ = proj.SetBucketTags(bg, bucket, nil)
		_ = proj.SetBucketQuota(bg, bucket, -1, -1, false)
		_ = proj.SetBucketVersioning(bg, bucket, false) // Suspended (S3 cannot go back to Disabled)
	})

	got, err := proj.GetBucketSettings(ctx, bucket)
	if err != nil {
		t.Fatalf("GetBucketSettings: %v", err)
	}
	if !got.Quota.Enabled || got.Quota.MaxSize != 5<<30 || got.Quota.MaxObjects != 1000 {
		t.Errorf("quota readback = %+v", got.Quota)
	}
	if len(got.Lifecycle) != 1 || got.Lifecycle[0].ID != "expire-tmp" || got.Lifecycle[0].ExpirationDays != 30 {
		t.Errorf("lifecycle readback = %+v", got.Lifecycle)
	}
	if len(got.CORS) != 1 || len(got.CORS[0].AllowedOrigins) != 1 {
		t.Errorf("cors readback = %+v", got.CORS)
	}
	if got.Tags["env"] != "drill" {
		t.Errorf("tags readback = %+v", got.Tags)
	}
	if got.Versioning != VersioningEnabled {
		t.Errorf("versioning readback = %q", got.Versioning)
	}
	t.Logf("settings readback OK: quota=%+v lifecycle=%d cors=%d tags=%v versioning=%s",
		got.Quota, len(got.Lifecycle), len(got.CORS), got.Tags, got.Versioning)
}

// TestLiveCephDGrantSurvivesWebsiteToggle is the regression that matters: the website toggle and per-key
// grants share ONE bucket policy document. Enabling/disabling the website must never destroy a grant.
func TestLiveCephDGrantSurvivesWebsiteToggle(t *testing.T) {
	if os.Getenv("CEPH_S3_WEBSITE_ENDPOINT") == "" {
		t.Skip("CEPH_S3_WEBSITE_ENDPOINT not set")
	}
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)

	if _, err := proj.CreateBucket(ctx, CreateBucketOpts{Name: bucket}); err != nil && !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("CreateBucket: %v", err)
	}
	payload := []byte("grant drill\n")
	if err := proj.UploadBucketObject(ctx, bucket, "hello.txt", "text/plain", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// --- create an extra S3 key (its own RGW user) ---
	uid := proj.ChildUID("drillkey")
	ak, sk, err := proj.CreateChildKey(ctx, uid, "proxcloud-drill-key")
	if err != nil {
		t.Fatalf("CreateChildKey: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = proj.RevokeBucketAccess(bg, bucket, uid)
		if err := proj.DeleteChildKey(bg, uid); err != nil {
			t.Errorf("cleanup: DeleteChildKey: %v", err)
		}
	})
	keyS3 := s3.New(s3.Options{
		Region: cephOf(admin).region, Credentials: credentials.NewStaticCredentialsProvider(ak, sk, ""),
		BaseEndpoint: awsv2.String(cephOf(admin).s3Endpoint), UsePathStyle: true,
		RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
	})
	canRead := func() bool {
		out, err := keyS3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: awsv2.String("hello.txt")})
		if err != nil {
			return false
		}
		out.Body.Close()
		return true
	}

	if canRead() {
		t.Fatal("new key could read before any grant")
	}
	if err := proj.GrantBucketAccess(ctx, bucket, uid, PermissionRead); err != nil {
		t.Fatalf("GrantBucketAccess: %v", err)
	}
	if !canRead() {
		t.Fatal("key cannot read after READ grant")
	}
	t.Log("grant applied; key can read")

	// --- enable then disable the website; the grant must survive BOTH ---
	if _, err := proj.EnableBucketWebsite(ctx, bucket, "index.html", ""); err != nil {
		t.Fatalf("EnableBucketWebsite: %v", err)
	}
	pol, _ := proj.GetBucketPolicyJSON(ctx, bucket)
	if !strings.Contains(pol, sidWebsiteRead) || !strings.Contains(pol, grantSid(uid)) {
		t.Fatalf("policy lost a statement after enabling website: %s", pol)
	}
	if !canRead() {
		t.Fatal("grant broken after enabling website")
	}

	if err := proj.DisableBucketWebsite(ctx, bucket); err != nil {
		t.Fatalf("DisableBucketWebsite: %v", err)
	}
	pol, _ = proj.GetBucketPolicyJSON(ctx, bucket)
	if strings.Contains(pol, sidWebsiteRead) {
		t.Fatalf("website statement survived disable: %s", pol)
	}
	if !strings.Contains(pol, grantSid(uid)) {
		t.Fatalf("DISABLING THE WEBSITE DESTROYED THE KEY GRANT: %s", pol)
	}
	if !canRead() {
		t.Fatal("grant broken after disabling website")
	}
	t.Log("grant survived website enable + disable (the clobber bug is fixed)")

	// --- grants are listable, and revoking removes exactly one ---
	grants, err := proj.ListBucketGrants(ctx, bucket)
	if err != nil || len(grants) != 1 || grants[0].UID != uid || grants[0].Permission != PermissionRead {
		t.Fatalf("ListBucketGrants = %+v err=%v", grants, err)
	}
	if err := proj.RevokeBucketAccess(ctx, bucket, uid); err != nil {
		t.Fatalf("RevokeBucketAccess: %v", err)
	}
	if canRead() {
		t.Fatal("key can still read after revoke")
	}
	t.Log("revoke works; policy left clean")
}

// TestLiveCephDObjectLockPurge answers the operational question the docs do not: can project teardown
// (Admin Ops purge) remove GOVERNANCE-retained objects? Retention is 90 SECONDS so a negative result
// self-clears rather than pinning a bucket for a day.
func TestLiveCephDObjectLockPurge(t *testing.T) {
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)
	lockBucket := bucket + "-lockpurge"

	if _, err := proj.CreateBucket(ctx, CreateBucketOpts{Name: lockBucket, ObjectLockEnabled: true}); err != nil {
		t.Fatalf("CreateBucket(objectLock): %v", err)
	}
	retain := time.Now().UTC().Add(90 * time.Second)
	key := "locked.txt"
	if _, err := cephOf(proj).s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &lockBucket, Key: &key, Body: strings.NewReader("locked"), ContentLength: awsv2.Int64(6),
		ObjectLockMode: s3types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: &retain,
	}); err != nil {
		t.Fatalf("PutObject(with retention): %v", err)
	}

	// The customer-facing delete must be refused while the object is retained.
	if _, err := cephOf(proj).s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &lockBucket, Key: &key, VersionId: nil}); err != nil {
		t.Logf("DeleteObject (delete marker) → %v", err)
	}

	err := admin.ForceDeleteBucket(ctx, lockBucket)
	if err != nil {
		t.Logf("⚠ teardown CANNOT purge a GOVERNANCE-retained bucket: %v", err)
		t.Logf("⚠ project deletion would fail until retention expires (%s)", retain.Format(time.RFC3339))
	} else {
		t.Log("✓ admin purge removed the GOVERNANCE-retained bucket → teardown is safe")
	}
	t.Cleanup(func() { _ = admin.ForceDeleteBucket(context.Background(), lockBucket) })
}
