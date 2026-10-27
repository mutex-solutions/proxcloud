//go:build cephlive

package client

// The "make bucket public" toggle must actually publish the OBJECTS (anonymous GET → 200), not merely set
// a bucket ACL that leaves every object 403. And it must not disturb the website statement or key grants.

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func anonCode(t *testing.T, url string) int {
	t.Helper()
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("anon GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestLiveCephFPublicToggleReallyPublishesObjects(t *testing.T) {
	ctx := context.Background()
	_, proj, bucket := liveClients(t, ctx)
	objURL := strings.TrimRight(os.Getenv("CEPH_S3_ENDPOINT"), "/") + "/" + bucket + "/hello.txt"

	body := []byte("public toggle drill\n")
	if err := proj.UploadBucketObject(ctx, bucket, "hello.txt", "text/plain", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { _ = proj.SetBucketRead(context.Background(), bucket, "") })

	if pub, err := proj.IsBucketPublic(ctx, bucket); err != nil || pub {
		t.Fatalf("bucket reported public before toggle: pub=%v err=%v", pub, err)
	}
	if code := anonCode(t, objURL); code == 200 {
		t.Fatal("object anonymously readable before toggle")
	}

	if err := proj.SetBucketRead(ctx, bucket, ".r:*,.rlistings"); err != nil {
		t.Fatalf("SetBucketRead(public): %v", err)
	}
	if pub, err := proj.IsBucketPublic(ctx, bucket); err != nil || !pub {
		t.Fatalf("IsBucketPublic=false after making public: err=%v", err)
	}
	if code := anonCode(t, objURL); code != 200 {
		t.Fatalf("object NOT anonymously readable after 'make public' → the toggle lies (code %d)", code)
	}
	t.Log("public toggle: anonymous GET → 200 (objects really are public)")

	if err := proj.SetBucketRead(ctx, bucket, ""); err != nil {
		t.Fatalf("SetBucketRead(private): %v", err)
	}
	if pub, _ := proj.IsBucketPublic(ctx, bucket); pub {
		t.Fatal("still public after making private")
	}
	if code := anonCode(t, objURL); code == 200 {
		t.Fatal("object still anonymously readable after making private")
	}
	t.Log("private toggle: anonymous GET denied again")
}
