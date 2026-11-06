//go:build cephlive

package client

// Live drill: key rotation must issue a working new key AND retire the old one, while leaving the RGW
// user's bucket grants intact (grants attach to the user, not the key).

import (
	"context"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestLiveCephERotateKey(t *testing.T) {
	ctx := context.Background()
	admin, proj, bucket := liveClients(t, ctx)

	uid := proj.ChildUID("rotkey")
	oldAK, oldSK, err := proj.CreateChildKey(ctx, uid, "proxcloud-rotate-drill")
	if err != nil {
		t.Fatalf("CreateChildKey: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = proj.RevokeBucketAccess(bg, bucket, uid)
		_ = proj.DeleteChildKey(bg, uid)
	})
	if err := proj.GrantBucketAccess(ctx, bucket, uid, PermissionRead); err != nil {
		t.Fatalf("GrantBucketAccess: %v", err)
	}

	s3For := func(ak, sk string) *s3.Client {
		return s3.New(s3.Options{
			Region: cephOf(admin).region, Credentials: credentials.NewStaticCredentialsProvider(ak, sk, ""),
			BaseEndpoint: awsv2.String(cephOf(admin).s3Endpoint), UsePathStyle: true,
			RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
		})
	}
	canList := func(c *s3.Client) bool {
		_, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket})
		return err == nil
	}

	if !canList(s3For(oldAK, oldSK)) {
		t.Fatal("old key cannot list before rotation")
	}

	newAK, newSK, err := proj.RotateKey(ctx, uid, oldAK)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if newAK == oldAK {
		t.Fatal("rotation returned the same access key")
	}
	if !canList(s3For(newAK, newSK)) {
		t.Fatal("NEW key does not work after rotation")
	}
	if canList(s3For(oldAK, oldSK)) {
		t.Fatal("OLD key still works after rotation — it was not retired")
	}
	t.Logf("rotated %s: %s… → %s… ; old key rejected, grant intact", uid, oldAK[:4], newAK[:4])

	grants, err := proj.ListBucketGrants(ctx, bucket)
	if err != nil || len(grants) != 1 || grants[0].UID != uid {
		t.Fatalf("grant lost across rotation: %+v err=%v", grants, err)
	}
}
