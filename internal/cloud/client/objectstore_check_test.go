package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckRustFS(t *testing.T) {
	adminCode, s3Code := http.StatusOK, http.StatusOK
	var adminPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/rustfs/admin/v3/") {
			adminPath = r.URL.Path
			w.WriteHeader(adminCode)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(s3Code)
		_, _ = w.Write([]byte(`<ListAllMyBucketsResult><Buckets/></ListAllMyBucketsResult>`))
	}))
	defer srv.Close()
	good := ObjectStoreConfig{S3Endpoint: srv.URL, AdminURL: srv.URL, Region: "r1", AdminAccessKey: "AK", AdminSecretKey: "SK"}
	ctx := context.Background()

	if err := CheckObjectStore(ctx, "rustfs", good); err != nil {
		t.Fatalf("good row: %v", err)
	}
	if adminPath != "/rustfs/admin/v3/list-canned-policies" {
		t.Fatalf("admin read went to %q", adminPath)
	}
	blankAdmin := good
	blankAdmin.AdminURL = ""
	if err := CheckRustFS(ctx, blankAdmin); err != nil {
		t.Fatalf("a blank admin URL defaults to the S3 endpoint: %v", err)
	}

	bad := map[string]func(c *ObjectStoreConfig){
		"no key":       func(c *ObjectStoreConfig) { c.AdminAccessKey = "" },
		"no secret":    func(c *ObjectStoreConfig) { c.AdminSecretKey = "" },
		"no S3":        func(c *ObjectStoreConfig) { c.S3Endpoint = "" },
		"not a URL":    func(c *ObjectStoreConfig) { c.S3Endpoint = "s3.example.org" },
		"bad admin":    func(c *ObjectStoreConfig) { c.AdminURL = "ftp://x" },
		"unreachable":  func(c *ObjectStoreConfig) { c.S3Endpoint = "http://127.0.0.1:1" },
		"admin closed": func(c *ObjectStoreConfig) { c.AdminURL = "http://127.0.0.1:1" },
	}
	for name, mutate := range bad {
		c := good
		mutate(&c)
		if err := CheckRustFS(ctx, c); err == nil {
			t.Errorf("%s must fail", name)
		}
	}

	// A wrong key is refused by the S3 leg; a key or endpoint that S3 accepts but the admin API does not, by the admin leg.
	s3Code = http.StatusForbidden
	if err := CheckRustFS(ctx, good); err == nil {
		t.Error("S3 403 must fail")
	}
	s3Code = http.StatusOK
	adminCode = http.StatusForbidden
	if err := CheckRustFS(ctx, good); err == nil || !strings.Contains(err.Error(), "Admin API") {
		t.Errorf("admin 403 must fail on the admin leg, got %v", err)
	}
}
