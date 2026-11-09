//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// Revoke after the production handler has already read state, during a genuine
// upstream HTTP availability request. No test-only production hook is needed.
func TestReviewerDNSCreateRevokeDuringPreflight(t *testing.T) {
	for _, kind := range []string{"project-disable", "provider-disable", "provider-detach", "binding-change"} {
		t.Run(kind, func(t *testing.T) {
			f := newDNSFixture(t)
			d := f.challenge(t, "p1", "dns", "late.example.org")
			f.publish(d)
			f.verify(t, "p1", "dns", d)
			original := f.server.Config.Handler
			var fired atomic.Bool
			var revokeErr error
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && fired.CompareAndSwap(false, true) {
					var table string
					var filter, set pgdoc.M
					switch kind {
					case "project-disable":
						table, filter, set = "project", pgdoc.M{"_id": "p1"}, pgdoc.M{"status": project.StatusDisabled}
					case "provider-disable":
						table, filter, set = "externalService", pgdoc.M{"_id": "dns"}, pgdoc.M{"status": "DISABLED"}
					case "provider-detach":
						table, filter, set = "project", pgdoc.M{"_id": "p1"}, pgdoc.M{"services": []any{pgdoc.M{"serviceId": "dns2", "region": "r1"}}}
					case "binding-change":
						table, filter, set = "externalService", pgdoc.M{"_id": "dns"}, pgdoc.M{"config.nameservers": []string{"ns.changed.net"}}
					}
					_, revokeErr = f.db.C(table).SetFields(context.Background(), filter, set, nil)
				}
				original.ServeHTTP(w, r)
			})
			code, body := f.create("p1", "dns", "late.example.org", d["id"].(string))
			if revokeErr != nil {
				t.Fatal(revokeErr)
			}
			if !fired.Load() {
				t.Fatal("upstream hook not reached")
			}
			f.mu.Lock()
			posts := f.posts
			f.mu.Unlock()
			if code == 200 || posts != 0 {
				t.Fatalf("revoked %s during pre-consumption HTTP yet create HTTP %d upstream POST=%d body=%s", kind, code, posts, body)
			}
		})
	}
}

func TestReviewerDNSVerifyRevokeDuringFinalPreflight(t *testing.T) {
	f := newDNSFixture(t)
	d := f.challenge(t, "p1", "dns", "lateverify.example.org")
	f.publish(d)
	original := f.server.Config.Handler
	var gets atomic.Int32
	var revokeErr error
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && gets.Add(1) == 3 {
			_, revokeErr = f.db.C("project").SetFields(context.Background(), pgdoc.M{"_id": "p1"}, pgdoc.M{"status": project.StatusDisabled}, nil)
		}
		original.ServeHTTP(w, r)
	})
	path := fmt.Sprintf("/project/p1/dns-ownership/%s/verify", d["id"].(string))
	code, body := f.request(suspendedSub, "POST", path, map[string]string{"serviceId": "dns"})
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	if gets.Load() < 3 {
		t.Fatal("upstream hook not reached")
	}
	if code == 200 || strings.Contains(body, `"state":"VERIFIED"`) {
		t.Fatalf("project disabled during final upstream read, still verified: HTTP %d body=%s", code, body)
	}
}
