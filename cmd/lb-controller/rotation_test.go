package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

// rotating is the gateway's credential side for a cluster whose credential the operator replaces ("Enable load
// balancer controller" again): only the secret last minted passes.
type rotating struct {
	mu     sync.Mutex
	secret string
}

func (r *rotating) Credential(_ context.Context, id string) (*lbgw.Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum, now := sha256.Sum256([]byte(r.secret)), time.Now()
	return &lbgw.Credential{ID: id, ProjectID: "p1", SecretSHA256: hex.EncodeToString(sum[:]), LastSeenAt: &now, Protocol: lbgw.Protocol}, nil
}

func (r *rotating) Seen(context.Context, string, time.Time, int) error { return nil }

// The operator's "Enable load balancer controller" on a cluster that has one replaces its credential: the old token
// stops at once, and the kubelet writes the new one into the controller's Secret volume some seconds later. The
// calls in between are refused by the gateway's own handler, 401. Drill R2b-o5 of v0.11.1-rc.2: every Service got
// one Warning GatewayRefused "authentication failure" for it. Now no Service is told while the refusal is younger
// than authGrace; the turn fails, so the next one comes after the interval and reads the token again, and with the
// new token it is served and says nothing new. A credential that stays refused (a wrong Secret, a cluster that was
// released) is said on every Service once the grace is over.
func TestCredentialRotationIsNoWarningOnTheServices(t *testing.T) {
	f := &fakeTenant{services: map[string]map[string]any{}}
	f.add("shop", "web", "u-1", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
	f.nodes = []map[string]any{node("w1", "10.0.0.11", nil)}
	reg := &rotating{secret: "s3cr3t"}
	ts := httptest.NewTLSServer(&lbgw.Handler{Reg: reg, Portal: &readyPortal{}, Log: quiet()})
	defer ts.Close()
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	c := &controller{api: f, put: g.put, log: quiet(), authGrace: 300 * time.Millisecond}
	reasons := func() []string {
		var out []string
		for _, ev := range f.events {
			out = append(out, ev["type"].(string)+" "+ev["reason"].(string))
		}
		return out
	}
	if err := c.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	served := reasons()
	if !slices.Equal(served, []string{"Normal EnsuredLoadBalancer"}) {
		t.Fatalf("events before the rotation: %v", served)
	}

	// The rotation: the portal takes the new secret now, the controller's file still holds the old one.
	reg.mu.Lock()
	reg.secret = "r0tated"
	reg.mu.Unlock()
	for range 2 {
		if err := c.turn(context.Background()); err == nil {
			t.Fatal("a turn with the replaced credential was accepted")
		}
	}
	if got := reasons(); !slices.Equal(got, served) {
		t.Fatalf("the Service was told of the refused calls of a rotation: %v", got)
	}
	// The kubelet writes the new token: the next turn is served, and the Service hears nothing it did not know.
	if err := os.WriteFile(filepath.Join(g.dir, "token"), []byte("lb@proxcloud!pc-c-1=r0tated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := reasons(); !slices.Equal(got, served) || !hasIngress(ingress(f.svc("shop", "web")), "203.0.113.5") {
		t.Fatalf("after the rotation: events %v, ingress %v", got, ingress(f.svc("shop", "web")))
	}

	// A credential that stays refused: nothing during the grace, the Warning after it, once.
	reg.mu.Lock()
	reg.secret = "another"
	reg.mu.Unlock()
	if err := c.turn(context.Background()); err == nil {
		t.Fatal("a turn with a wrong credential was accepted")
	}
	if got := reasons(); !slices.Equal(got, served) {
		t.Fatalf("told within the grace: %v", got)
	}
	time.Sleep(c.authGrace + 50*time.Millisecond)
	for range 2 {
		if err := c.turn(context.Background()); err == nil {
			t.Fatal("a turn with a wrong credential was accepted")
		}
	}
	if got := reasons(); !slices.Equal(got, append(slices.Clone(served), "Warning GatewayRefused")) {
		t.Fatalf("a refusal that lasts: events %v, want one Warning GatewayRefused", got)
	}
}
