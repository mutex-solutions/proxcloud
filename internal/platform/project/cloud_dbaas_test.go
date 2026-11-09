package project

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestDbaasSpecFromData(t *testing.T) {
	body := func() map[string]any {
		return map[string]any{
			"name": "orders", "engine": "postgresql", "version": "17",
			"replicas": float64(3), "cpu": float64(2), "memoryGiB": float64(8), "storageGiB": float64(100),
			"allowedCidrs": []any{"10.1.0.0/24"},
		}
	}
	spec, err := dbaasSpecFromData("p1", body())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(spec.ID, "pc-d-") || len(spec.ID) != 13 {
		t.Fatalf("id = %q", spec.ID)
	}
	if spec.DisplayName != "orders" || spec.Engine != "postgresql" || spec.Version != "17" ||
		spec.Replicas != 3 || spec.CPU != 2 || spec.MemoryGiB != 8 || spec.StorageGiB != 100 ||
		len(spec.AllowedCIDRs) != 1 || spec.BetaAck {
		t.Fatalf("spec = %+v", spec)
	}

	// Strict decode: missing/typed-wrong numbers are 400s, not zero values.
	for _, k := range []string{"replicas", "cpu", "memoryGiB", "storageGiB"} {
		d := body()
		delete(d, k)
		if _, err := dbaasSpecFromData("p1", d); err == nil {
			t.Errorf("missing %s must be rejected", k)
		}
		d = body()
		d[k] = "three"
		if _, err := dbaasSpecFromData("p1", d); err == nil {
			t.Errorf("string %s must be rejected", k)
		}
	}
	d := body()
	delete(d, "name")
	if _, err := dbaasSpecFromData("p1", d); err == nil {
		t.Error("missing name must be rejected")
	}
	d = body()
	d["allowedCidrs"] = []any{float64(1)}
	if _, err := dbaasSpecFromData("p1", d); err == nil {
		t.Error("non-string cidr must be rejected")
	}
	// Beta ack is strict: a non-bool is a 400, not a silent false.
	d = body()
	d["beta"] = "true"
	if _, err := dbaasSpecFromData("p1", d); err == nil {
		t.Error("non-bool beta must be rejected")
	}
	d = body()
	d["beta"] = true
	if spec, err := dbaasSpecFromData("p1", d); err != nil || !spec.BetaAck {
		t.Error("bool beta must decode")
	}
}

func TestDbaasCIDRs(t *testing.T) {
	out, err := dbaasCIDRs([]any{"10.0.0.0/8", ""})
	if err != nil || len(out) != 1 || out[0] != "10.0.0.0/8" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if _, err := dbaasCIDRs([]any{"10.0.0.1"}); err == nil {
		t.Fatal("bare IP must be rejected")
	}
	if _, err := dbaasCIDRs("10.0.0.0/8"); err == nil {
		t.Fatal("non-array must be rejected")
	}
}

func TestValidateCustomDomainTLS(t *testing.T) {
	mint := func(dnsName string, notAfter time.Time) (certPEM, keyPEM string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: dnsName},
			DNSNames:     []string{dnsName},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     notAfter,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	}

	good, goodKey := mint("search.example.com", time.Now().Add(24*time.Hour))
	if err := validateCustomDomainTLS("search.example.com", good, goodKey); err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if err := validateCustomDomainTLS("other.example.com", good, goodKey); err == nil {
		t.Fatal("cert not covering the domain must be rejected")
	}
	expired, expiredKey := mint("search.example.com", time.Now().Add(-time.Minute))
	if err := validateCustomDomainTLS("search.example.com", expired, expiredKey); err == nil {
		t.Fatal("expired cert must be rejected")
	}
	_, otherKey := mint("search.example.com", time.Now().Add(24*time.Hour))
	if err := validateCustomDomainTLS("search.example.com", good, otherKey); err == nil {
		t.Fatal("mismatched key must be rejected")
	}
	for _, bad := range []string{"", "singlelabel", "UPPER.example.com", "-bad.example.com", "a b.example.com"} {
		if err := validateCustomDomainTLS(bad, good, goodKey); err == nil {
			t.Fatalf("domain %q must be rejected", bad)
		}
	}
	if err := validateCustomDomainTLS("search.example.com", "", ""); err == nil {
		t.Fatal("missing PEM material must be rejected")
	}
}

func TestDbaasSpecFromDataDay2Fields(t *testing.T) {
	d := map[string]any{
		"name": "search", "engine": "opensearch", "version": "3.3.0",
		"replicas": float64(3), "cpu": float64(2), "memoryGiB": float64(8), "storageGiB": float64(100),
		"dashboards": true, "readEndpoint": true,
		"sso":         map[string]any{"connectUrl": "https://idp.example.com/.well-known/openid-configuration", "clientId": "dash"},
		"restoreFrom": map[string]any{"sourceDatabaseId": " pc-d-0abc1234 ", "targetTime": "2026-11-01T09:30:00Z", "backupName": "b1"},
	}
	spec, err := dbaasSpecFromData("p1", d)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.DashboardsEnabled || !spec.ReadEndpoint || spec.SSO["clientId"] != "dash" {
		t.Fatalf("spec = %+v", spec)
	}
	if r := spec.RestoreFrom; r == nil || r.SourceID != "pc-d-0abc1234" || r.TargetTime != "2026-11-01T09:30:00Z" || r.BackupName != "b1" || r.PITR {
		t.Fatalf("restoreFrom = %+v (PITR is decided server-side, never by the customer)", r)
	}
	for k, v := range map[string]any{"dashboards": "yes", "readEndpoint": "yes", "restoreFrom": "pc-d-1", "sso": []any{}} {
		bad := map[string]any{"name": "x", "engine": "postgresql", "version": "17", "replicas": float64(1), "cpu": float64(1), "memoryGiB": float64(1), "storageGiB": float64(1)}
		bad[k] = v
		if _, err := dbaasSpecFromData("p1", bad); err == nil {
			t.Errorf("%s: malformed value must be rejected", k)
		}
	}
	// SSO needs both halves or nothing.
	if _, err := dbaasSSOFields(map[string]any{"clientId": "dash"}); err == nil {
		t.Error("sso without connectUrl must be rejected")
	}
}

func TestDbaasAccessFromData(t *testing.T) {
	dbs, users, roles, err := dbaasAccessFromData(map[string]any{
		"databases": []any{map[string]any{"name": " orders ", "owner": "alice"}},
		"users": []any{map[string]any{
			"name": "alice", "databases": []any{"orders"}, "roles": []any{"readall"},
		}},
		"roles": []any{map[string]any{"name": "logs", "indexPatterns": []any{"logs-*"}, "actions": []any{"read"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 1 || dbs[0].Name != "orders" || dbs[0].Owner != "alice" {
		t.Fatalf("dbs = %+v", dbs)
	}
	if len(users) != 1 || users[0].Name != "alice" || users[0].Databases[0] != "orders" || users[0].Roles[0] != "readall" {
		t.Fatalf("users = %+v", users)
	}
	if len(roles) != 1 || roles[0].IndexPatterns[0] != "logs-*" || roles[0].Actions[0] != "read" {
		t.Fatalf("roles = %+v", roles)
	}
	for name, body := range map[string]map[string]any{
		"databases not an array": {"databases": "orders"},
		"user not an object":     {"users": []any{"alice"}},
		"grants not names":       {"users": []any{map[string]any{"name": "a", "databases": []any{1}}}},
	} {
		if _, _, _, err := dbaasAccessFromData(body); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestDbaasIndexPoliciesFromData(t *testing.T) {
	out, err := dbaasIndexPoliciesFromData(map[string]any{"policies": []any{map[string]any{
		"name": "logs30", "indexPatterns": []any{"logs-*"}, "deleteAfterDays": float64(30), "rolloverGiB": float64(50),
	}}})
	if err != nil || len(out) != 1 || out[0].DeleteAfterDays != 30 || out[0].RolloverGiB != 50 || out[0].IndexPatterns[0] != "logs-*" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if _, err := dbaasIndexPoliciesFromData(map[string]any{"policies": "all"}); err == nil {
		t.Fatal("non-array must be rejected")
	}
}
