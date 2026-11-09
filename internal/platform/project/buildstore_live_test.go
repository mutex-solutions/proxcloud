//go:build s3live

package project

// Live check of the template build bucket code against a real S3 store (docs/managed-sandbox.md §7.6): a context that
// fits one part (one PUT), one that needs a multipart upload, both with UNSIGNED-PAYLOAD, read back through a presigned
// GET, then removed with the template's prefix.
//
//	S3_ENDPOINT=http://… S3_BUCKET=… S3_ACCESS=… S3_SECRET=… [S3_REGION=…] go test -tags s3live -run TestBuildStoreLive ./internal/platform/project/

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestBuildStoreLive(t *testing.T) {
	bs, err := buildStorage(sandbox.Config{BuildStorage: sandbox.BuildStorage{Endpoint: os.Getenv("S3_ENDPOINT"), Region: os.Getenv("S3_REGION"),
		Bucket: os.Getenv("S3_BUCKET"), AccessKey: os.Getenv("S3_ACCESS"), SecretKey: os.Getenv("S3_SECRET")}})
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	prefix := "live-" + newToken(4) + "/" + newToken(10) + "/"
	if pod := os.Getenv("S3_POD_ENDPOINT"); pod != "" {
		// Only upload, and print presigned URLs for the endpoint the nodes reach, to fetch from there.
		pb, _ := buildStorage(sandbox.Config{BuildStorage: sandbox.BuildStorage{Endpoint: os.Getenv("S3_ENDPOINT"), PodEndpoint: pod,
			Region: os.Getenv("S3_REGION"), Bucket: os.Getenv("S3_BUCKET"), AccessKey: os.Getenv("S3_ACCESS"), SecretKey: os.Getenv("S3_SECRET")}})
		body := make([]byte, 2*contextPartSize+12345)
		_, _ = rand.Read(body)
		if err := pb.put(ctx, prefix+"m.tar", io.MultiReader(bytes.NewReader(body))); err != nil {
			t.Fatal(err)
		}
		u, _ := pb.fetchURL(ctx, prefix+"m.tar")
		t.Logf("PREFIX %s SHA256 %x URL %s", prefix, sha256.Sum256(body), u)
		return
	}
	defer bs.deletePrefix(ctx, prefix)
	for name, size := range map[string]int{"single": 1 << 20, "multipart": 2*contextPartSize + 12345} {
		body := make([]byte, size)
		_, _ = rand.Read(body)
		key := prefix + name + ".tar"
		// io.MultiReader hides the bytes.Reader: the body is a plain stream, as a request body is.
		if err := bs.put(ctx, key, io.MultiReader(bytes.NewReader(body))); err != nil {
			t.Fatalf("%s put: %v", name, err)
		}
		head, err := bs.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bs.bucket, Key: &key})
		if err != nil {
			t.Fatalf("%s head: %v", name, err)
		}
		etag := strings.Trim(awsv2.ToString(head.ETag), `"`)
		u, err := bs.fetchURL(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		got, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if awsv2.ToInt64(head.ContentLength) != int64(size) {
			t.Fatalf("%s: stored %d bytes of %d", name, awsv2.ToInt64(head.ContentLength), size)
		}
		if resp.StatusCode != http.StatusOK || rerr != nil || sha256.Sum256(got) != sha256.Sum256(body) {
			t.Fatalf("%s presigned GET: %s, %d bytes back of %d (read: %v)", name, resp.Status, len(got), size, rerr)
		}
		t.Logf("%s: %d bytes, ETag %s, presigned GET %s", name, size, etag, resp.Status)
		if name == "multipart" && !strings.HasSuffix(etag, "-3") {
			t.Errorf("multipart ETag %s, want 3 parts", etag)
		}
	}
	bs.deletePrefix(ctx, prefix)
	out, err := bs.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bs.bucket, Prefix: &prefix})
	if err != nil || len(out.Contents) != 0 {
		t.Fatalf("after deletePrefix: %v %d objects", err, len(out.Contents))
	}
}
