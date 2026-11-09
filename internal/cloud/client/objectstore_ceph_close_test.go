package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGateway holds one bucket's policy and answers website calls with websiteStatus: 405 is a gateway
// running without rgw_enable_static_website.
type fakeGateway struct {
	policy        string
	websiteStatus int
	websiteCalls  int
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case q.Has("website"):
		g.websiteCalls++
		w.WriteHeader(g.websiteStatus)
	case q.Has("policy") && r.Method == http.MethodGet:
		if g.policy == "" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchBucketPolicy</Code></Error>`)
			return
		}
		_, _ = io.WriteString(w, g.policy)
	case q.Has("policy") && r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		g.policy = string(b)
		w.WriteHeader(http.StatusNoContent)
	case q.Has("policy") && r.Method == http.MethodDelete:
		g.policy = ""
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusOK) // the bucket ACL
	}
}

func (g *fakeGateway) publish(t *testing.T, sids ...string) {
	t.Helper()
	doc := policyDoc{Version: "2012-10-17"}
	for _, sid := range sids {
		doc.Statement = append(doc.Statement, json.RawMessage(`{"Sid":"`+sid+`","Effect":"Allow","Principal":"*","Action":"s3:GetObject"}`))
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	g.policy = string(raw)
}

func gatewayClient(t *testing.T, g *fakeGateway, websiteEndpoint string) *cephBackend {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	b, err := newCephBackend(ObjectStoreConfig{
		S3Endpoint: srv.URL, S3WebsiteEndpoint: websiteEndpoint, Region: "us-east-1",
		ProjectAccessKey: "a", ProjectSecretKey: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A bucket published through a website must close on every kind of gateway, leaving the customer's own
// statement alone.
func TestCephWebsiteCloses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		websiteStatus   int
		websiteEndpoint string
		wantErr         bool
	}{
		{name: "gateway without the website API", websiteStatus: http.StatusMethodNotAllowed, websiteEndpoint: "https://w.example"},
		{name: "provider without a website endpoint", websiteStatus: http.StatusNoContent},
		// The statement goes even when the config cannot: the caller sees the error on a closed bucket.
		{name: "website delete fails", websiteStatus: http.StatusForbidden, websiteEndpoint: "https://w.example", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGateway{websiteStatus: tc.websiteStatus}
			g.publish(t, sidWebsiteRead, "customer")
			b := gatewayClient(t, g, tc.websiteEndpoint)

			if err := b.DisableBucketWebsite(ctx, "b"); (err != nil) != tc.wantErr {
				t.Fatalf("disable err = %v; want error %v", err, tc.wantErr)
			}
			if strings.Contains(g.policy, sidWebsiteRead) || !strings.Contains(g.policy, "customer") {
				t.Fatalf("policy after disable = %s", g.policy)
			}
		})
	}
}

// The panel must show a website the gateway can no longer describe, or it offers Enable on a public bucket.
func TestCephWebsiteStateFollowsTheStatement(t *testing.T) {
	ctx := context.Background()
	g := &fakeGateway{websiteStatus: http.StatusMethodNotAllowed}
	g.publish(t, sidWebsiteRead)
	b := gatewayClient(t, g, "https://w.example")

	site, err := b.GetBucketWebsite(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if !site.Enabled || !site.PublicObjects || site.URL != "" {
		t.Fatalf("published on a 405 gateway: %+v", site)
	}
	if err := b.DisableBucketWebsite(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if site, err = b.GetBucketWebsite(ctx, "b"); err != nil || site.Enabled || site.PublicObjects {
		t.Fatalf("after closing: %+v err %v", site, err)
	}
}

// Private removes every anonymous statement; a bucket that was never a website sees no website call.
func TestCephMakePrivateClosesTheWebsite(t *testing.T) {
	ctx := context.Background()
	g := &fakeGateway{websiteStatus: http.StatusMethodNotAllowed}
	g.publish(t, sidPublicRead, sidWebsiteRead)
	b := gatewayClient(t, g, "")

	if err := b.SetBucketRead(ctx, "b", ""); err != nil {
		t.Fatal(err)
	}
	public, err := b.hasAnonymousReadStmt(ctx, "b")
	if err != nil || public {
		t.Fatalf("still public = %v, err %v, policy %s", public, err, g.policy)
	}
	if g.websiteCalls != 1 {
		t.Fatalf("website calls = %d; want the one config delete", g.websiteCalls)
	}

	g = &fakeGateway{websiteStatus: http.StatusMethodNotAllowed}
	g.publish(t, sidPublicRead)
	b = gatewayClient(t, g, "")
	if err := b.SetBucketRead(ctx, "b", ""); err != nil {
		t.Fatal(err)
	}
	if g.websiteCalls != 0 {
		t.Fatalf("website calls = %d on a bucket with no website", g.websiteCalls)
	}
}

// The registration check must fail on everything that would otherwise fail each project's bootstrap later.
func TestCheckCephS3(t *testing.T) {
	ctx := context.Background()
	all := `{"caps":[{"type":"users","perm":"*"},{"type":"buckets","perm":"*"},{"type":"usage","perm":"*"}]}`
	for _, tc := range []struct {
		name       string
		userStatus int
		userBody   string
		s3Status   int
		endpoint   string // "" = the test server
		secret     string
		want       string // "" = passes
	}{
		{name: "good", userStatus: 200, userBody: all, s3Status: 200, secret: "s"},
		{name: "blank endpoint", endpoint: "-", secret: "s", want: "http(s) URL"},
		{name: "no scheme", endpoint: "s3.example", secret: "s", want: "http(s) URL"},
		{name: "no secret", userStatus: 200, userBody: all, s3Status: 200, want: "required"},
		{name: "bad key on S3", s3Status: 403, secret: "s", want: "S3 endpoint"},
		{name: "not RGW", userStatus: 404, s3Status: 200, secret: "s", want: "Admin Ops"},
		{name: "no admin caps", userStatus: 403, s3Status: 200, secret: "s", want: "Admin Ops"},
		{name: "read-only users cap", userStatus: 200, userBody: strings.Replace(all, `"users","perm":"*"`, `"users","perm":"read"`, 1), s3Status: 200, secret: "s", want: "users=*"},
		{name: "no usage cap", userStatus: 200, userBody: `{"caps":[{"type":"users","perm":"*"},{"type":"buckets","perm":"*"}]}`, s3Status: 200, secret: "s", want: "usage=*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/admin/user") {
					if got := r.URL.Query().Get("access-key"); got != "AK" {
						t.Errorf("access-key = %q", got)
					}
					w.WriteHeader(tc.userStatus)
					_, _ = io.WriteString(w, tc.userBody)
					return
				}
				w.WriteHeader(tc.s3Status)
				if tc.s3Status == 200 {
					_, _ = io.WriteString(w, `<ListAllMyBucketsResult><Buckets/></ListAllMyBucketsResult>`)
				}
			}))
			defer srv.Close()
			endpoint := srv.URL
			if tc.endpoint == "-" {
				endpoint = ""
			} else if tc.endpoint != "" {
				endpoint = tc.endpoint
			}
			err := CheckCephS3(ctx, ObjectStoreConfig{
				S3Endpoint: endpoint, AdminURL: endpoint + "/admin", Region: "us-east-1",
				AdminAccessKey: "AK", AdminSecretKey: tc.secret,
			})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("want pass, got %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
		})
	}
}
