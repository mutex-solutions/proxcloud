//go:build cephlive

package client

// A child S3 key (CreateCephChildUser) must NOT be able to create its own buckets: such buckets sit
// outside the parent's per-bucket grants AND outside syncCephService (which lists only the parent uid),
// so they would never be metered or billed. RGW enforces this via max-buckets=-1 — live-measured, because
// the semantics are counter-intuitive (-1 forbids, 0 allows).
//
//	go test ./internal/cloud/client/ -tags cephlive -run TestLiveCephGChildCannotCreateBucket -v

import (
	"context"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestLiveCephGChildCannotCreateBucket(t *testing.T) {
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)

	uid := proj.ChildUID("nomake")
	ak, sk, err := proj.CreateCephChildUser(ctx, uid, "proxcloud-nomake-drill")
	if err != nil {
		t.Fatalf("CreateCephChildUser: %v", err)
	}
	t.Cleanup(func() { _ = proj.DeleteCephChildUser(context.Background(), uid) })

	childS3 := s3.New(s3.Options{
		Region:                     admin.ceph.region,
		Credentials:                credentials.NewStaticCredentialsProvider(ak, sk, ""),
		BaseEndpoint:               awsv2.String(admin.ceph.s3Endpoint),
		UsePathStyle:               true,
		RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
	})

	own := bucket + "-childmade"
	if _, err := childS3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &own}); err == nil {
		// It managed to create one — clean it up and fail loudly (metering/grant bypass).
		t.Cleanup(func() { _ = admin.ForceDeleteCephBucket(context.Background(), own) })
		t.Fatalf("child key created its own bucket %q — max-buckets guard is not working", own)
	} else {
		t.Logf("child key correctly denied CreateBucket: %v", err)
	}
}
