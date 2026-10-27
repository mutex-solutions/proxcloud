//go:build cephlive

package client

// Capability probe against the live RGW: which of the Ceph-dashboard bucket features can ProxCloud actually
// expose? Discovery only — it touches no production code, logs a matrix, and cleans up everything it makes
// (one temp bucket + one temp reader user, both dev_-prefixed).
//
//	go test ./internal/cloud/client/ -tags cephlive -run TestLiveCephCCapabilities -v

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func report(t *testing.T, feature string, err error) bool {
	t.Helper()
	if err != nil {
		msg := err.Error()
		if i := strings.Index(msg, "https response error"); i >= 0 {
			msg = msg[i:]
		}
		if len(msg) > 160 {
			msg = msg[:160]
		}
		t.Logf("  ✗ %-28s %s", feature, msg)
		return false
	}
	t.Logf("  ✓ %-28s OK", feature)
	return true
}

func TestLiveCephCCapabilities(t *testing.T) {
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)
	capsBucket := bucket + "-caps"

	// ---------- 0. Is this Ceph Squid (RGW Accounts / real IAM users)? ----------
	var acct any
	err := admin.ceph.adminDo(ctx, "GET", "/account", url.Values{}, nil, &acct)
	t.Log("RGW Accounts (Squid IAM):")
	report(t, "GET /admin/account", err)

	// ---------- 1. Bucket created WITH object lock (implies versioning; create-time only) ----------
	t.Log("Bucket create-time options:")
	_, err = proj.ceph.s3.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: &capsBucket, ObjectLockEnabledForBucket: awsv2.Bool(true),
	})
	lockBucketOK := report(t, "CreateBucket+ObjectLock", err)
	if lockBucketOK {
		t.Cleanup(func() {
			bg := context.Background()
			if _, err := proj.ceph.s3.DeleteBucket(bg, &s3.DeleteBucketInput{Bucket: &capsBucket}); err != nil {
				t.Logf("cleanup: DeleteBucket %s: %v", capsBucket, err)
			}
		})
	}

	if lockBucketOK {
		// ---------- 2. Versioning ----------
		t.Log("Versioning:")
		vout, err := proj.ceph.s3.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &capsBucket})
		if report(t, "GetBucketVersioning", err) {
			t.Logf("      status=%q (object-lock buckets are versioned automatically)", vout.Status)
		}
		_, err = proj.ceph.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: &capsBucket, VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
		})
		report(t, "PutBucketVersioning", err)

		// ---------- 3. Object lock configuration ----------
		t.Log("Object lock:")
		_, err = proj.ceph.s3.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
			Bucket: &capsBucket,
			ObjectLockConfiguration: &s3types.ObjectLockConfiguration{
				ObjectLockEnabled: s3types.ObjectLockEnabledEnabled,
				Rule: &s3types.ObjectLockRule{DefaultRetention: &s3types.DefaultRetention{
					Mode: s3types.ObjectLockRetentionModeGovernance, Days: awsv2.Int32(1),
				}},
			},
		})
		report(t, "PutObjectLockConfiguration", err)
		_, err = proj.ceph.s3.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: &capsBucket})
		report(t, "GetObjectLockConfiguration", err)

		// ---------- 4. Encryption (SSE-S3 default) — needs a KMS/vault backend ----------
		t.Log("Encryption:")
		_, err = proj.ceph.s3.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
			Bucket: &capsBucket,
			ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
				Rules: []s3types.ServerSideEncryptionRule{{
					ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{SSEAlgorithm: s3types.ServerSideEncryptionAes256},
				}},
			},
		})
		report(t, "PutBucketEncryption(AES256)", err)
		// Accepting the config proves nothing. Write a real object and read back its SSE header: RGW's
		// SSE-S3 needs a KMS backend (rgw_crypt_sse_s3_backend), and without one it silently stores
		// PLAINTEXT while still accepting PutBucketEncryption. Never expose an "encrypted" toggle that lies.
		probeKey := "sse-probe.txt"
		if _, err := proj.ceph.s3.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &capsBucket, Key: &probeKey, Body: strings.NewReader("probe"), ContentLength: awsv2.Int64(5),
		}); err != nil {
			t.Logf("      sse probe: PutObject failed: %v", err)
		} else {
			head, herr := proj.ceph.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &capsBucket, Key: &probeKey})
			if herr != nil {
				t.Logf("      sse probe: HeadObject failed: %v", herr)
			} else if head.ServerSideEncryption == "" {
				t.Logf("      ⚠ object stored WITHOUT server-side encryption despite the bucket config → SSE-S3 has no KMS backend")
			} else {
				t.Logf("      ✓ object actually encrypted: ServerSideEncryption=%q", head.ServerSideEncryption)
			}
			_, _ = proj.ceph.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &capsBucket, Key: &probeKey})
		}

		// ---------- 5. Lifecycle ----------
		t.Log("Lifecycle:")
		_, err = proj.ceph.s3.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
			Bucket: &capsBucket,
			LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: []s3types.LifecycleRule{{
				ID: awsv2.String("expire-tmp"), Status: s3types.ExpirationStatusEnabled,
				Filter:     &s3types.LifecycleRuleFilter{Prefix: awsv2.String("tmp/")},
				Expiration: &s3types.LifecycleExpiration{Days: awsv2.Int32(30)},
			}}},
		})
		report(t, "PutBucketLifecycle", err)

		// ---------- 6. CORS + tagging ----------
		t.Log("CORS / tagging:")
		_, err = proj.ceph.s3.PutBucketCors(ctx, &s3.PutBucketCorsInput{
			Bucket: &capsBucket,
			CORSConfiguration: &s3types.CORSConfiguration{CORSRules: []s3types.CORSRule{{
				AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"*"},
			}}},
		})
		report(t, "PutBucketCors", err)
		_, err = proj.ceph.s3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
			Bucket: &capsBucket,
			Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{
				Key: awsv2.String("proxcloud"), Value: awsv2.String("drill"),
			}}},
		})
		report(t, "PutBucketTagging", err)

		// ---------- 7. Replication (single-zone cluster → expected to fail) ----------
		t.Log("Replication:")
		rep, err := proj.ceph.s3.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: &capsBucket})
		if report(t, "GetBucketReplication", err) {
			n := 0
			if rep.ReplicationConfiguration != nil {
				n = len(rep.ReplicationConfiguration.Rules)
			}
			t.Logf("      rules=%d (a single-zone cluster has no peer to replicate to)", n)
		}

		// ---------- 8. Per-bucket quota (RGW Admin Ops, NOT S3) ----------
		t.Log("Bucket quota (admin ops):")
		q := url.Values{
			"quota": {""}, "bucket": {capsBucket}, "uid": {admin.ceph.uid},
			"max-size": {"1073741824"}, "max-objects": {"100"}, "enabled": {"true"},
		}
		report(t, "PUT /admin/bucket?quota", admin.ceph.adminDo(ctx, "PUT", "/bucket", q, nil, nil))
		var st struct {
			BucketQuota struct {
				Enabled    bool  `json:"enabled"`
				MaxSize    int64 `json:"max_size"`
				MaxObjects int64 `json:"max_objects"`
			} `json:"bucket_quota"`
		}
		if err := admin.ceph.adminDo(ctx, "GET", "/bucket", url.Values{"bucket": {capsBucket}, "stats": {"true"}}, nil, &st); err == nil {
			t.Logf("      readback: enabled=%v max_size=%d max_objects=%d",
				st.BucketQuota.Enabled, st.BucketQuota.MaxSize, st.BucketQuota.MaxObjects)
		}
	}

	// ---------- 9. THE BIG ONE: grant a SEPARATE key access to ONE bucket via bucket policy ----------
	t.Log("Per-bucket access for a separate S3 key (IAM-style):")
	readerUID := "dev_proxcloud_reader"
	var created adminUser
	err = admin.ceph.adminDo(ctx, "PUT", "/user",
		url.Values{"uid": {readerUID}, "display-name": {"proxcloud-drill-reader"}}, nil, &created)
	if !report(t, "create reader RGW user", err) || len(created.Keys) == 0 {
		return
	}
	t.Cleanup(func() {
		_ = admin.ceph.adminDo(context.Background(), "DELETE", "/user",
			url.Values{"uid": {readerUID}, "purge-data": {"true"}}, nil, nil)
		t.Logf("cleanup: purged %s", readerUID)
	})
	readerS3 := s3.New(s3.Options{
		Region:       admin.ceph.region,
		Credentials:  credentials.NewStaticCredentialsProvider(created.Keys[0].AccessKey, created.Keys[0].SecretKey, ""),
		BaseEndpoint: awsv2.String(admin.ceph.s3Endpoint), UsePathStyle: true,
		RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
	})

	// (a) before any policy → the reader must be denied.
	_, err = readerS3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: awsv2.String("hello.txt")})
	if err == nil {
		t.Error("reader could read the bucket BEFORE any grant — isolation broken!")
	} else {
		t.Logf("  ✓ %-28s denied before grant (correct)", "reader GetObject")
	}

	// (b) owner attaches a bucket policy naming the reader's IAM ARN as principal.
	policy := `{"Version":"2012-10-17","Statement":[
	 {"Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/` + readerUID + `"]},
	  "Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + bucket + `/*"]},
	 {"Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/` + readerUID + `"]},
	  "Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::` + bucket + `"]}]}`
	_, err = proj.ceph.s3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: &bucket, Policy: &policy})
	if !report(t, "PutBucketPolicy(principal)", err) {
		return
	}
	t.Cleanup(func() {
		if _, err := proj.ceph.s3.DeleteBucketPolicy(context.Background(), &s3.DeleteBucketPolicyInput{Bucket: &bucket}); err != nil {
			t.Errorf("cleanup: DeleteBucketPolicy: %v", err)
		}
		t.Logf("cleanup: bucket policy removed from %s", bucket)
	})

	// (c) now the reader's OWN key can read this ONE bucket.
	out, err := readerS3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: awsv2.String("hello.txt")})
	if report(t, "reader GetObject after grant", err) {
		out.Body.Close()
	}
	_, err = readerS3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket})
	report(t, "reader ListBucket after grant", err)

	// (d) …and STILL cannot touch the caps bucket, which was never granted.
	if lockBucketOK {
		_, err = readerS3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &capsBucket})
		if err == nil {
			t.Error("reader could list a bucket it was never granted — policy scoping broken!")
		} else {
			t.Logf("  ✓ %-28s still denied (grant is per-bucket)", "reader other bucket")
		}
	}
	// (e) reader must not be able to WRITE (we only granted GetObject/ListBucket).
	_, err = readerS3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: awsv2.String("evil.txt"), Body: strings.NewReader("x"), ContentLength: awsv2.Int64(1),
	})
	if err == nil {
		t.Error("reader could WRITE with a read-only grant — action scoping broken!")
	} else {
		t.Logf("  ✓ %-28s denied (action scoping works)", "reader PutObject")
	}

	b, _ := json.Marshal(acct)
	t.Logf("account probe payload: %.120s", string(b))
}
