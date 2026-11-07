package sandboxproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type fakeRegistryAPI struct {
	nss      []map[string]any
	selector string
	listErr  error
	secrets  map[string][]byte // ns/name -> .dockerconfigjson
	writes   int
}

func (f *fakeRegistryAPI) ListNamespaces(_ context.Context, sel string) ([]map[string]any, error) {
	f.selector = sel
	return f.nss, f.listErr
}

func (f *fakeRegistryAPI) GetSecretData(_ context.Context, ns, name string) (map[string][]byte, error) {
	if b, ok := f.secrets[ns+"/"+name]; ok {
		return map[string][]byte{".dockerconfigjson": b}, nil
	}
	return nil, nil
}

func (f *fakeRegistryAPI) ApplyCore(_ context.Context, _ string, obj map[string]any) error {
	meta := obj["metadata"].(map[string]any)
	if obj["type"] != "kubernetes.io/dockerconfigjson" {
		return errors.New("wrong type")
	}
	data := obj["data"].(map[string]any)[".dockerconfigjson"].(string)
	b, _ := base64.StdEncoding.DecodeString(data)
	f.secrets[meta["namespace"].(string)+"/"+meta["name"].(string)] = b
	f.writes++
	return nil
}

func tenantNS(name, pid string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{
		"proxcloud.io/project": pid, "proxcloud.io/tenant": "true", "app.kubernetes.io/managed-by": "proxcloud"}}}
}

func TestRegistryProjects(t *testing.T) {
	notOurs := tenantNS("sb-c3", "c3")
	notOurs["metadata"].(map[string]any)["labels"].(map[string]any)["app.kubernetes.io/managed-by"] = "someone"
	got := RegistryProjects([]map[string]any{
		tenantNS("sb-b2", "b2"), tenantNS("sb-a1", "a1"), tenantNS("sb-a1", "a1"),
		tenantNS("sb-x", "y"),     // name does not match the label
		tenantNS("sb-*", "*"),     // a glob would widen the pattern
		tenantNS("sb-a/b", "a/b"), // so would a slash
		tenantNS("sb-UP", "UP"),   // not lowercase
		notOurs,
	})
	if strings.Join(got, ",") != "a1,b2" {
		t.Fatalf("projects = %v", got)
	}
}

func TestZotAccess(t *testing.T) {
	ac := ZotAccess([]string{"a1"})
	repos := ac["repositories"].(map[string]any)
	if _, ok := repos["t/a1/**"]; !ok || len(repos) != 3 {
		t.Fatalf("repositories %v", repos)
	}
	lib, _ := json.Marshal(repos["library/**"])
	if strings.Contains(string(lib), RegistryPushUser) || strings.Contains(string(lib), "create") {
		t.Fatalf("library/** must be read-only and not name the push user: %s", lib)
	}
	own, _ := json.Marshal(repos["t/a1/**"])
	if !strings.Contains(string(own), `"pull-a1"`) || !strings.Contains(string(own), `"push"`) {
		t.Fatalf("t/a1/** = %s", own)
	}
	pull, _ := json.Marshal(ac["groups"])
	if !strings.Contains(string(pull), `"pull-a1"`) || !strings.Contains(string(pull), `"pull-nodes"`) {
		t.Fatalf("groups = %s", pull)
	}
}

func newUsers(t *testing.T, api *fakeRegistryAPI) *RegistryUsers {
	t.Helper()
	return &RegistryUsers{
		Base: map[string]any{"storage": map[string]any{"rootDirectory": "/var/lib/registry"}, "http": map[string]any{"port": "5000"}},
		Host: "192.0.2.10", Key: []byte("0123456789abcdef0123"), Dir: t.TempDir(), SystemNS: "sandbox-system", API: api,
		Static: map[string]string{"admin": "adm", "push": "psh", "pull-nodes": "pn"},
	}
}

func readCfg(t *testing.T, dir string) (map[string]any, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	ht, _ := os.ReadFile(filepath.Join(dir, "htpasswd"))
	return cfg, string(ht)
}

func TestRegistryUsersPass(t *testing.T) {
	api := &fakeRegistryAPI{nss: []map[string]any{tenantNS("sb-a1", "a1")}, secrets: map[string][]byte{}}
	r := newUsers(t, api)
	ctx := context.Background()
	if changed, err := r.Pass(ctx); err != nil || !changed {
		t.Fatalf("first pass: %v %v", changed, err)
	}
	cfg, ht := readCfg(t, r.Dir)
	if cfg["storage"].(map[string]any)["rootDirectory"] != "/var/lib/registry" {
		t.Fatalf("base config lost: %v", cfg)
	}
	auth := cfg["http"].(map[string]any)["auth"].(map[string]any)["htpasswd"].(map[string]any)["path"]
	if auth != filepath.Join(r.Dir, "htpasswd") {
		t.Fatalf("htpasswd path %v", auth)
	}
	pw := RegistryPassword(r.Key, "pull-a1")
	if !htpasswdOK(ht, "pull-a1", pw) || !htpasswdOK(ht, "push", "psh") || htpasswdOK(ht, "pull-a1", "wrong") {
		t.Fatalf("htpasswd does not hold the derived password:\n%s", ht)
	}
	var dc map[string]map[string]map[string]string
	_ = json.Unmarshal(api.secrets["sb-a1/sandbox-template-pull"], &dc)
	if dc["auths"]["192.0.2.10"]["password"] != pw || dc["auths"]["192.0.2.10"]["username"] != "pull-a1" {
		t.Fatalf("project secret %s", api.secrets["sb-a1/sandbox-template-pull"])
	}
	if !strings.Contains(string(api.secrets["sandbox-system/sandbox-registry-nodes"]), `"pull-nodes"`) {
		t.Fatalf("nodes secret %s", api.secrets["sandbox-system/sandbox-registry-nodes"])
	}

	// Same projects: no file change, no Secret write.
	writes := api.writes
	if changed, err := r.Pass(ctx); err != nil || changed || api.writes != writes {
		t.Fatalf("no-op pass: changed %v err %v writes %d -> %d", changed, err, writes, api.writes)
	}

	// The API fails: keep what Zot has.
	api.listErr, api.nss = errors.New("down"), nil
	if changed, err := r.Pass(ctx); err == nil || changed {
		t.Fatalf("failed list must keep the users: %v %v", changed, err)
	}
	if _, ht := readCfg(t, r.Dir); !strings.Contains(ht, "pull-a1:") {
		t.Fatal("a failed list dropped pull-a1")
	}

	// The project goes: its user goes.
	api.listErr = nil
	if changed, err := r.Pass(ctx); err != nil || !changed {
		t.Fatalf("drop pass: %v %v", changed, err)
	}
	if cfg, ht := readCfg(t, r.Dir); strings.Contains(ht, "pull-a1:") || cfg["http"].(map[string]any)["accessControl"].(map[string]any)["repositories"].(map[string]any)["t/a1/**"] != nil {
		t.Fatal("pull-a1 stayed after its namespace went")
	}
}

func TestRegistryUsersServiceSelector(t *testing.T) {
	api := &fakeRegistryAPI{secrets: map[string][]byte{}}
	r := newUsers(t, api)
	if changed, err := r.Pass(context.Background()); !changed || err != nil {
		t.Fatalf("first pass with no project: %v %v", changed, err)
	}
	if api.selector != "proxcloud.io/tenant=true,app.kubernetes.io/managed-by=proxcloud" {
		t.Fatalf("selector %q", api.selector)
	}
	// No project yet is a state like any other: the next pass does not rewrite (the lab saw a reload every 30 s).
	if changed, _ := r.Pass(context.Background()); changed {
		t.Fatal("a cluster without projects rewrote Zot's config on every pass")
	}
	r.ServiceID = "svc1"
	_, _ = r.Pass(context.Background())
	if !strings.HasSuffix(api.selector, ",proxcloud.io/service=svc1") {
		t.Fatalf("selector with a service %q", api.selector)
	}
}

func TestRegistryUsersFirstPassWithoutAPI(t *testing.T) {
	api := &fakeRegistryAPI{listErr: errors.New("down"), secrets: map[string][]byte{}}
	r := newUsers(t, api)
	if changed, err := r.Pass(context.Background()); err == nil || !changed {
		t.Fatalf("first pass without the API must still write the static users: %v %v", changed, err)
	}
	if _, ht := readCfg(t, r.Dir); !htpasswdOK(ht, "admin", "adm") {
		t.Fatalf("htpasswd %s", ht)
	}
}

func htpasswdOK(ht, user, pw string) bool {
	for _, line := range strings.Split(ht, "\n") {
		if u, h, ok := strings.Cut(line, ":"); ok && u == user {
			return bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)) == nil
		}
	}
	return false
}
