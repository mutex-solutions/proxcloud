package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// mockRGW emulates the RGW Admin Ops JSON API (/admin/*) + a minimal S3 endpoint on one server, so the
// ceph backend can be exercised without a live cluster. It records the last Authorization header so a test
// can assert the request was SigV4-signed.
type mockRGW struct {
	lastAuth   string
	lastSHA256 string
	// userGetStatus overrides the GET /admin/user status (0 = 404 "absent"). Set to 403 to prove an
	// auth failure is NOT mistaken for an absent user.
	userGetStatus int
	// bucketOwner overrides the owner returned for single-bucket admin reads ("" = the client's uid). Set
	// to a different uid to prove the ownership guard rejects a foreign bucket.
	bucketOwner string
	// sawBucketDelete records that a DELETE /admin/bucket was issued (the owner guard must prevent it for
	// a foreign bucket).
	sawBucketDelete bool
}

func (m *mockRGW) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.lastAuth = r.Header.Get("Authorization")
		if strings.HasPrefix(r.URL.Path, "/admin") {
			// RGW rejects SigV4 without x-amz-content-sha256; it must also be in SignedHeaders.
			m.lastSHA256 = r.Header.Get("X-Amz-Content-Sha256")
			if m.lastSHA256 == "" {
				t.Errorf("admin %s %s sent without X-Amz-Content-Sha256", r.Method, r.URL.Path)
			}
			if !strings.Contains(m.lastAuth, "x-amz-content-sha256") {
				t.Errorf("x-amz-content-sha256 not in SignedHeaders: %q", m.lastAuth)
			}
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/admin/user"):
			switch r.Method {
			case http.MethodGet:
				status := m.userGetStatus
				if status == 0 {
					status = http.StatusNotFound // default: absent → force create
				}
				http.Error(w, `{"Code":"NoSuchUser"}`, status)
			case http.MethodPut:
				if r.URL.Query().Has("quota") {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"user_id":"proj1","keys":[{"user":"proj1","access_key":"AKPROJ","secret_key":"SKPROJ"}]}`))
			case http.MethodDelete:
				w.WriteHeader(http.StatusOK)
			}
		case strings.HasPrefix(r.URL.Path, "/admin/bucket"):
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodDelete {
				m.sawBucketDelete = true
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.URL.Query().Get("uid") != "" { // list
				_, _ = w.Write([]byte(`[{"bucket":"b1","owner":"proj1","usage":{"rgw.main":{"size":2147483648,"num_objects":3}}}]`))
				return
			}
			// Single-bucket info. Buckets live in RGW's default tenant, so the ref is the plain name and no
			// `tenant` param is ever sent — enforce that contract so a regression is caught.
			if got := r.URL.Query().Get("bucket"); got != "b1" {
				t.Errorf("admin bucket ref = %q; want b1", got)
			}
			if r.URL.Query().Has("tenant") {
				t.Error("we do not use RGW tenants; no tenant param must be sent")
			}
			// bucketOwner returns whatever owner the mock is configured with (default = the client's uid).
			owner := m.bucketOwner
			if owner == "" {
				owner = "proj1"
			}
			_, _ = w.Write([]byte(`{"bucket":"b1","owner":"` + owner + `","usage":{"rgw.main":{"size":1073741824,"num_objects":1}}}`))
		default: // treat as S3
			m.s3(t, w, r)
		}
	})
}

func (m *mockRGW) s3(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Name>b1</Name><Prefix></Prefix><Delimiter>/</Delimiter><IsTruncated>false</IsTruncated>
<Contents><Key>file.txt</Key><Size>5</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>
<CommonPrefixes><Prefix>folder/</Prefix></CommonPrefixes>
</ListBucketResult>`))
		return
	}
	if r.Method == http.MethodPut && r.URL.Path == "/taken-bucket" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>BucketAlreadyExists</Code>` +
			`<Message>bucket exists</Message></Error>`))
		return
	}
	// CreateBucket / PutObject / DeleteObject / DeleteBucket — accept.
	w.WriteHeader(http.StatusOK)
}

func newTestCeph(t *testing.T, url string, withProjectKeys bool) *Client {
	cfg := ObjectStoreConfig{
		S3Endpoint: url, AdminURL: url + "/admin", Region: "us-east-1",
		AdminAccessKey: "ADMINAK", AdminSecretKey: "ADMINSK",
		RGWUID: "proj1", DefaultQuotaBytes: 100 << 30,
	}
	if withProjectKeys {
		cfg.ProjectAccessKey, cfg.ProjectSecretKey = "AKPROJ", "SKPROJ"
	}
	cc, err := NewCephS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewCephS3: %v", err)
	}
	return cc
}

func TestCephBucketStatsTotals(t *testing.T) {
	st := adminBucketStat{Usage: map[string]struct {
		Size       int64 `json:"size"`
		SizeActual int64 `json:"size_actual"`
		NumObjects int64 `json:"num_objects"`
	}{
		"rgw.main":      {Size: 1000, NumObjects: 2},
		"rgw.multimeta": {SizeActual: 50, NumObjects: 1}, // size 0 → falls back to size_actual
	}}
	bytes, objects := st.totals()
	if bytes != 1050 || objects != 3 {
		t.Fatalf("totals = %d,%d; want 1050,3", bytes, objects)
	}
}

func TestCephEnsureUserAndQuota(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false) // admin-only client

	access, secret, err := cc.EnsureProjectUser(context.Background(), "proxcloud-proj1")
	if err != nil {
		t.Fatalf("EnsureProjectUser: %v", err)
	}
	if access != "AKPROJ" || secret != "SKPROJ" {
		t.Fatalf("keys = %q/%q; want AKPROJ/SKPROJ", access, secret)
	}
	if !strings.HasPrefix(m.lastAuth, "AWS4-HMAC-SHA256") {
		t.Fatalf("admin request not SigV4-signed: %q", m.lastAuth)
	}
}

// A 403 on the existence probe must surface as an error — never be treated as "user absent" (which would
// blindly attempt a create and report the wrong cause).
func TestCephEnsureUserAuthErrorSurfaces(t *testing.T) {
	m := &mockRGW{userGetStatus: http.StatusForbidden}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false)

	_, _, err := cc.EnsureProjectUser(context.Background(), "proxcloud-proj1")
	if err == nil {
		t.Fatal("EnsureProjectUser should fail when the user probe is denied")
	}
	if !strings.Contains(err.Error(), "lookup user") {
		t.Fatalf("want lookup-user error, got %v", err)
	}
}

func TestCephListBucketsStats(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false)

	bs, err := cc.ListBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("got %d buckets; want 1", len(bs))
	}
	if bs[0]["bucketName"] != "b1" {
		t.Fatalf("bucketName = %v", bs[0]["bucketName"])
	}
	if bs[0]["sizeInBytes"].(int64) != 2147483648 || bs[0]["objectCount"].(int64) != 3 {
		t.Fatalf("usage = %v/%v; want 2147483648/3", bs[0]["sizeInBytes"], bs[0]["objectCount"])
	}
	if s := bs[0]["sizeInGb"].(interface{ String() string }).String(); s != "2" {
		t.Fatalf("sizeInGb = %s; want 2", s)
	}
}

func TestCephGetBucket(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false)

	b, err := cc.GetBucket(context.Background(), "b1")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if b["sizeInBytes"].(int64) != 1073741824 || b["objectCount"].(int64) != 1 {
		t.Fatalf("usage = %v/%v; want 1073741824/1", b["sizeInBytes"], b["objectCount"])
	}
}

// The RGW namespace is global and the admin key can address any bucket by name, so an admin-by-name read
// (GetBucket stats) MUST refuse a bucket owned by another uid — otherwise a recycled name on a stale cache
// row leaks the other project's object count/size (finding F4).
func TestCephGetBucketRefusesForeignOwner(t *testing.T) {
	m := &mockRGW{bucketOwner: "someone-else"}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false) // this client's uid is "proj1"

	if _, err := cc.GetBucket(context.Background(), "b1"); !errors.Is(err, ErrBucketNotOwned) {
		t.Fatalf("GetBucket on a foreign bucket: want ErrBucketNotOwned, got %v", err)
	}
}

// Teardown force-delete must not purge a recycled bucket name now owned by another project (finding F3):
// it is a no-op success (so the caller archives the stale row) and NO delete is issued.
func TestCephForceDeleteRefusesForeignOwner(t *testing.T) {
	m := &mockRGW{bucketOwner: "someone-else"}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false)

	if err := cc.ForceDeleteBucket(context.Background(), "b1"); err != nil {
		t.Fatalf("ForceDeleteBucket on a foreign bucket should be a no-op, got %v", err)
	}
	if m.sawBucketDelete {
		t.Fatal("a DELETE /admin/bucket was issued for a bucket owned by another project")
	}
}

// The happy path still deletes when the bucket IS ours.
func TestCephForceDeleteOwnBucket(t *testing.T) {
	m := &mockRGW{} // owner defaults to the client's uid
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false)

	if err := cc.ForceDeleteBucket(context.Background(), "b1"); err != nil {
		t.Fatalf("ForceDeleteBucket on our own bucket: %v", err)
	}
	if !m.sawBucketDelete {
		t.Fatal("expected a DELETE /admin/bucket for our own bucket")
	}
}

// A bucket/key name with characters outside the admin allow-list must be rejected BEFORE any network
// call — attacker-influenced input must not shape the Admin Ops request URL (CWE-918). The mock server
// fails the test if it is ever reached with such a name.
func TestCephAdminRejectsUnsafeNameBeforeRequest(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin") {
			reached = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false)

	for _, bad := range []string{"a b", "a/b", "a?x=1", "a#f", "http://evil", "a\r\nHost: x"} {
		if _, err := cc.GetBucket(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Errorf("GetBucket(%q): want an 'unsafe' rejection, got %v", bad, err)
		}
	}
	if reached {
		t.Fatal("an admin request was sent with an unsafe bucket name (guard did not fire before the network call)")
	}
}

func TestCephListBucketObjects(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, true) // needs project S3 keys

	objs, err := cc.ListBucketObjects(context.Background(), "b1", "")
	if err != nil {
		t.Fatalf("ListBucketObjects: %v", err)
	}
	var dirs, files int
	for _, o := range objs {
		if o["directory"].(bool) {
			dirs++
			if o["name"] != "folder/" || o["displayName"] != "folder" {
				t.Fatalf("dir row = %v", o)
			}
		} else {
			files++
			if o["name"] != "file.txt" || o["sizeInBytes"].(int64) != 5 {
				t.Fatalf("file row = %v", o)
			}
		}
	}
	if dirs != 1 || files != 1 {
		t.Fatalf("got %d dirs, %d files; want 1,1", dirs, files)
	}
}

func TestCephCreateBucketAdminOnlyFails(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, false) // no project keys → data ops must error, not panic

	if _, err := cc.CreateBucket(context.Background(), CreateBucketOpts{Name: "b1"}); err == nil {
		t.Fatal("CreateBucket on admin-only client should error")
	}
}

func TestCephCreateBucket(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, true)

	data, err := cc.CreateBucket(context.Background(), CreateBucketOpts{Name: "b1"})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if data["bucketName"] != "b1" || data["sizeInBytes"].(int64) != 0 {
		t.Fatalf("fresh bucket data = %v", data)
	}
	if data["storageBackend"] != BackendCephS3 {
		t.Fatalf("storageBackend = %v; want %s", data["storageBackend"], BackendCephS3)
	}
}

// The website URL is virtual-hosted (<bucket>.<websiteHost>), which is why the wildcard cert/DNS exists —
// it is NOT the path-style S3 data endpoint. And the public-read policy must name the TENANTED bucket ARN.
func TestCephWebsiteNeedsAnEndpoint(t *testing.T) {
	cfg := ObjectStoreConfig{S3Endpoint: "https://s3.example", ProjectAccessKey: "a", ProjectSecretKey: "b"}
	b, err := newCephBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if b.Caps().Website {
		t.Fatal("no s3WebsiteEndpoint: website must not be offered")
	}
	if _, err := b.EnableBucketWebsite(context.Background(), "b", "", ""); !errors.Is(err, ErrWebsiteUnsupported) {
		t.Fatalf("enable err = %v", err)
	}
	cfg.S3WebsiteEndpoint = "https://s3-website.example"
	if b, err = newCephBackend(cfg); err != nil || !b.Caps().Website {
		t.Fatalf("with an endpoint: caps %+v err %v", b.Caps(), err)
	}
	notAllowed := &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}}}
	if !errors.Is(websiteErr(notAllowed), ErrWebsiteUnsupported) {
		t.Fatal("a 405 means the gateway has no website API")
	}
}

func TestCephWebsiteURLAndPolicy(t *testing.T) {
	cc, err := NewCephS3(context.Background(), ObjectStoreConfig{
		S3Endpoint: "https://s3.example", S3WebsiteEndpoint: "https://s3-website.example/",
		AdminURL: "https://s3.example/admin", Region: "us-east-1", RGWUID: "proj1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cephOf(cc).websiteURL("my-site"); got != "https://my-site.s3-website.example" {
		t.Errorf("websiteURL = %q", got)
	}
	if got := cephOf(cc).bucketARN("my-site"); got != "arn:aws:s3:::my-site/*" {
		t.Errorf("bucketARN = %q; want untenanted form", got)
	}
	stmt := cephOf(cc).websiteStmt("my-site")
	if stmt.Sid != sidWebsiteRead || stmt.Effect != "Allow" {
		t.Errorf("website stmt = %+v", stmt)
	}
	if !strings.Contains(string(stmt.Action), "s3:GetObject") || !strings.Contains(string(stmt.Resource), "arn:aws:s3:::my-site/*") {
		t.Errorf("website stmt action/resource = %s / %s", stmt.Action, stmt.Resource)
	}
	// No website endpoint configured → no URL (the provider does not offer website hosting).
	bare, _ := NewCephS3(context.Background(), ObjectStoreConfig{S3Endpoint: "https://s3.example", Region: "us-east-1"})
	if got := cephOf(bare).websiteURL("b"); got != "" {
		t.Errorf("websiteURL without endpoint = %q; want empty", got)
	}
}
func TestCephCreateBucketNameTaken(t *testing.T) {
	m := &mockRGW{}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()
	cc := newTestCeph(t, srv.URL, true)

	_, err := cc.CreateBucket(context.Background(), CreateBucketOpts{Name: "taken-bucket"})
	if !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("want ErrBucketNameTaken, got %v", err)
	}
}
