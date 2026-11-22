//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve/lbcerttest"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type lbCertLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lbCertLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// The four routes of a project's TLS certificates, against a real PostgreSQL, with real certificates: an upload is
// checked and answered with what the certificate says; it is the project's alone; one a listener names is not
// deleted; without the portal's key nothing is taken; a project that is deleted leaves none. And the key: every
// response of the flow, every log line, every audit event and every stored row is searched for it.
func TestLBCertificateRoutes(t *testing.T) {
	ctx := context.Background()
	logs := &lbCertLog{}
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(before)

	f := newSbxFixture(t, nil)
	keys, err := textcrypt.NewBlobKeys("default-v1", map[string]string{"default-v1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	store := f.db.C("lb_certificates")
	f.h.SetLBCertificates(store, keys)

	rsaKey := lbcerttest.RSA(t, 2048)
	good := lbcerttest.New(t, rsaKey, time.Now().Add(72*time.Hour), "www.example.org", "example.org")
	expired := lbcerttest.New(t, rsaKey, time.Now().Add(-time.Hour), "old.example.org")
	other := lbcerttest.New(t, lbcerttest.RSA(t, 2048), time.Now().Add(72*time.Hour), "other.example.org")
	secrets := []string{strings.Split(good.Key, "\n")[2], strings.Split(other.Key, "\n")[2]}
	holdsKey := func(s string) bool {
		return strings.Contains(s, "PRIVATE KEY") || slices.ContainsFunc(secrets, func(k string) bool { return strings.Contains(s, k) })
	}
	var responses []string
	do := func(sub, method, path string, body any) (int, string) {
		t.Helper()
		raw := ""
		switch b := body.(type) {
		case string:
			raw = b
		case nil:
		default:
			enc, _ := json.Marshal(b)
			raw = string(enc)
		}
		code, out := f.do(sub, method, path, raw)
		responses = append(responses, out)
		return code, out
	}
	type view struct {
		ID, Name, Subject, Issuer, Fingerprint, KeyType, File string
		DNSNames, UsedBy, Warnings                            []string
		NotAfter                                              time.Time
		ChainLength                                           int
	}
	read := func(body string, into any) {
		t.Helper()
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil || json.Unmarshal(env.Data, into) != nil {
			t.Fatalf("answer %s", body)
		}
	}
	upload := func(name string, c lbcerttest.Cert, key string) (int, string) {
		return do(sbxA, http.MethodPost, "/project/pa/certificates", map[string]string{"name": name, "certificate": c.Chain, "privateKey": key})
	}

	// An upload, and what it answers.
	code, body := upload("web", good, good.Key)
	var made view
	if code != http.StatusOK {
		t.Fatalf("upload: %d %s", code, body)
	}
	read(body, &made)
	if made.ID == "" || made.Name != "web" || made.Subject != "CN=www.example.org" || made.KeyType != "RSA 2048" || made.ChainLength != 2 ||
		!slices.Equal(made.DNSNames, []string{"www.example.org", "example.org"}) || len(made.Fingerprint) != 64 || !made.NotAfter.Equal(good.Leaf.NotAfter) ||
		len(made.Warnings) != 0 || made.UsedBy == nil {
		t.Errorf("the answer of an upload: %s", body)
	}
	// An expired one is taken, with a warning.
	code, body = upload("old", expired, expired.Key)
	var old view
	read(body, &old)
	if code != http.StatusOK || len(old.Warnings) != 1 || !strings.Contains(old.Warnings[0], "expired on") {
		t.Errorf("an expired certificate: %d %s", code, body)
	}
	// What is refused, each with its reason and never with the key.
	for name, c := range map[string]struct {
		code int
		body any
		want string
	}{
		"another certificate's key": {400, map[string]string{"name": "x", "certificate": good.Chain, "privateKey": other.Key}, "does not belong to the certificate"},
		"a name taken":              {409, map[string]string{"name": "web", "certificate": good.Chain, "privateKey": good.Key}, "already has a certificate named web"},
		"a name off the rule":       {400, map[string]string{"name": "My Cert", "certificate": good.Chain, "privateKey": good.Key}, "lowercase letters"},
		"no key":                    {400, map[string]string{"name": "x", "certificate": good.Chain}, "one key in it"},
		"a body that is not JSON":   {400, good.Chain + good.Key, "as JSON text fields"},
		"a key larger than allowed": {400, map[string]string{"name": "x", "certificate": good.Chain, "privateKey": good.Key + strings.Repeat("\n", 17<<10)}, "larger than 16 KiB"},
	} {
		if code, body := do(sbxA, http.MethodPost, "/project/pa/certificates", c.body); code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}

	// The list and one certificate; the project's alone.
	code, body = do(sbxA, http.MethodGet, "/project/pa/certificates", nil)
	var list []view
	read(body, &list)
	if code != http.StatusOK || len(list) != 2 || list[0].Name != "old" || list[1].ID != made.ID {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body := do(sbxA, http.MethodGet, "/project/pa/certificates/"+made.ID, nil); code != http.StatusOK || !strings.Contains(body, made.Fingerprint) {
		t.Errorf("get: %d %s", code, body)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/project/pa/certificates"}, {http.MethodGet, "/project/pa/certificates/" + made.ID},
		{http.MethodDelete, "/project/pa/certificates/" + made.ID}, {http.MethodPost, "/project/pa/certificates"},
	} {
		if code, body := do(sbxB, c.method, c.path, `{"name":"x"}`); code < 400 || strings.Contains(body, made.Fingerprint) {
			t.Errorf("another tenant, %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} { // the id under the other tenant's own project
		if code, body := do(sbxB, method, "/project/pb/certificates/"+made.ID, nil); code != http.StatusNotFound {
			t.Errorf("%s of project pa's certificate through project pb: %d %s", method, code, body)
		}
	}
	if code, body := do(sbxB, http.MethodGet, "/project/pb/certificates", nil); code != http.StatusOK || strings.Contains(body, made.ID) {
		t.Errorf("project pb's list: %d %s", code, body)
	}

	// A certificate a listener names: not deleted, and the answer says which balancer.
	row, err := f.rows.Insert(ctx, &cloud.CloudResource{ExternalID: "lb-abc", ServiceID: "sbx", ProjectID: "pa", Type: cloud.TypeLoadBalancer,
		Data: map[string]any{"loadbalancer": map[string]any{"id": "abc", "name": "shop", "listeners": []any{
			map[string]any{"port": 443, "protocol": "https", "pool": "web", "certificates": []any{made.ID}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := do(sbxA, http.MethodDelete, "/project/pa/certificates/"+made.ID, nil); code != http.StatusConflict || !strings.Contains(body, "load balancer shop") {
		t.Errorf("delete of a certificate in use: %d %s", code, body)
	}
	code, body = do(sbxA, http.MethodGet, "/project/pa/certificates/"+made.ID, nil)
	var used view
	read(body, &used)
	if code != http.StatusOK || !slices.Equal(used.UsedBy, []string{"shop"}) {
		t.Errorf("who uses it: %d %s", code, body)
	}
	if code, body := do(sbxA, http.MethodDelete, "/project/pa/certificates/"+old.ID, nil); code != http.StatusOK { // one no listener names
		t.Errorf("delete of a certificate not in use: %d %s", code, body)
	}
	if err := f.rows.DeleteAndArchive(ctx, row, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code, body := do(sbxA, http.MethodDelete, "/project/pa/certificates/"+made.ID, nil); code != http.StatusOK {
		t.Errorf("delete after the balancer went: %d %s", code, body)
	}
	if code, _ := do(sbxA, http.MethodGet, "/project/pa/certificates/"+made.ID, nil); code != http.StatusNotFound {
		t.Errorf("get after delete: %d", code)
	}

	// Without the portal's key: an upload is refused and stores nothing; the list still answers.
	f.h.SetLBCertificates(store, nil)
	if code, body := upload("nokey", good, good.Key); code != http.StatusServiceUnavailable || !strings.Contains(body, "no encryption key") {
		t.Errorf("an upload without the portal's key: %d %s", code, body)
	}
	if code, _ := do(sbxA, http.MethodGet, "/project/pa/certificates", nil); code != http.StatusOK {
		t.Errorf("list without the portal's key: %d", code)
	}
	f.h.SetLBCertificates(store, keys)
	if n, err := store.Count(ctx, pgdoc.M{}); err != nil || n != 0 {
		t.Fatalf("certificates left: %d %v", n, err)
	}

	// A deleted project leaves none; another project keeps its own.
	if code, body := upload("web", good, good.Key); code != http.StatusOK {
		t.Fatalf("upload: %d %s", code, body)
	}
	if code, body := do(sbxB, http.MethodPost, "/project/pb/certificates", map[string]string{"name": "web", "certificate": other.Chain, "privateKey": other.Key}); code != http.StatusOK {
		t.Fatalf("upload to pb: %d %s", code, body)
	}
	// At rest, while two are stored: no row holds a key in clear.
	var stored []json.RawMessage
	if err := store.Find(ctx, pgdoc.M{}, &stored); err != nil || len(stored) != 2 {
		t.Fatalf("stored: %d %v", len(stored), err)
	}
	for _, doc := range stored {
		if holdsKey(string(doc)) || !strings.Contains(string(doc), `"sealed"`) {
			t.Error("a stored certificate is not sealed")
		}
	}
	if err := f.h.TeardownProject(ctx, "pa"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if a, _ := store.Count(ctx, pgdoc.M{"projectId": "pa"}); a != 0 {
		t.Errorf("project pa's certificates after its teardown: %d", a)
	}
	if b, _ := store.Count(ctx, pgdoc.M{"projectId": "pb"}); b != 1 {
		t.Errorf("project pb's certificates after pa's teardown: %d", b)
	}

	// The audit events of the flow: three uploads and two deletes by tenant A, one upload by tenant B, each with the
	// name and the fingerprint. They are written in the background.
	var events []json.RawMessage
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		events = nil
		if err := f.db.C("auditEvent").Find(ctx, pgdoc.M{}, &events); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ev := range events {
			if strings.Contains(string(ev), `"lbCertificate"`) {
				n++
			}
		}
		if n == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d certificate audit events, want 6: %s", n, events)
		}
	}
	all := ""
	for _, ev := range events {
		all += string(ev)
	}
	if !strings.Contains(all, made.Fingerprint) || !strings.Contains(all, `"name": "web"`) {
		t.Errorf("the audit events name no certificate: %s", all)
	}

	// The key, nowhere: no response, no log line, no audit event.
	for _, r := range responses {
		if holdsKey(r) {
			t.Errorf("a response holds the key: %.200s", r)
		}
	}
	if len(responses) < 25 {
		t.Errorf("only %d responses were searched", len(responses))
	}
	for _, ev := range events {
		if holdsKey(string(ev)) {
			t.Errorf("an audit event holds the key: %.200s", ev)
		}
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if holdsKey(logs.buf.String()) {
		t.Error("a log line holds the key")
	}
}
