package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitEnv(t *testing.T) {
	user, list := splitEnv([]string{"A=1", "PROXCLOUD_START_CMD=x=y", "PATH=/bin", "B=a=b", "junk", "HOME=/root"})
	if !reflect.DeepEqual(user, map[string]string{"A": "1", "PATH": "/bin", "B": "a=b"}) {
		t.Fatalf("user=%v", user)
	}
	if !reflect.DeepEqual(list, []string{"A=1", "PATH=/bin", "B=a=b", "HOME=/root"}) {
		t.Fatalf("list=%v", list)
	}
}

func TestInitPayload(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b, _ := initPayload(map[string]string{"A": "1"}, "tok", "user", "/home/user", ts)
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["accessToken"] != "tok" || m["defaultUser"] != "user" || m["defaultWorkdir"] != "/home/user" ||
		m["timestamp"] != "2026-01-02T03:04:05Z" || m["envVars"].(map[string]any)["A"] != "1" {
		t.Fatalf("%v", m)
	}
	b, _ = initPayload(nil, "", "root", "/root", ts)
	if strings.Contains(string(b), "accessToken") {
		t.Fatalf("token must be omitted: %s", b)
	}
}

func TestEnsureUser(t *testing.T) {
	d := t.TempDir()
	pw, gr := filepath.Join(d, "passwd"), filepath.Join(d, "group")
	os.WriteFile(pw, []byte("root:x:0:0::/root:/bin/sh\napp:x:1000:1000::/app:/bin/sh"), 0o644) // no trailing newline
	os.WriteFile(gr, []byte("root:x:0:\napp:x:1000:\n"), 0o644)
	chown = func(string, int, int) error { return nil }
	uid, gid, home, err := ensureUser(pw, gr, filepath.Join(d, "home"), "user")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 1001 || gid != 1001 || home != filepath.Join(d, "home", "user") {
		t.Fatalf("%d %d %s", uid, gid, home)
	}
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		t.Fatal("home not created")
	}
	p, _ := os.ReadFile(pw)
	g, _ := os.ReadFile(gr)
	if !strings.Contains(string(p), "\nuser:x:1001:1001::"+home+":/bin/sh\n") || !strings.HasSuffix(string(g), "user:x:1001:\n") {
		t.Fatalf("passwd=%q group=%q", p, g)
	}
	// idempotent: existing entry returned untouched
	u2, g2, h2, err := ensureUser(pw, gr, filepath.Join(d, "home"), "user")
	p2, _ := os.ReadFile(pw)
	if err != nil || u2 != 1001 || g2 != 1001 || h2 != home || string(p2) != string(p) {
		t.Fatalf("not idempotent: %d %d %s %v", u2, g2, h2, err)
	}
}

func TestEnsureUserMissingFiles(t *testing.T) {
	d := t.TempDir()
	uid, _, _, err := ensureUser(filepath.Join(d, "passwd"), filepath.Join(d, "group"), filepath.Join(d, "h"), "user")
	if err != nil || uid != 1000 {
		t.Fatalf("%d %v", uid, err)
	}
}

func TestReadyCheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ready")
	if readyCheck(p) != 1 {
		t.Fatal("want 1 when missing")
	}
	os.WriteFile(p, nil, 0o644)
	if readyCheck(p) != 0 {
		t.Fatal("want 0 when present")
	}
}
