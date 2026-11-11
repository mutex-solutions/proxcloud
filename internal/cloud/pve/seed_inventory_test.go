package pve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSeedName = "pcseed-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.iso"

// A complete fake models EXACT rights, including empty NoAccess shadows. It
// never treats an empty /cluster/resources alone as proof of absence.
func seedInventoryFake(t *testing.T, mode string) (*Client, *SeedArtifact, func()) {
	t.Helper()
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/access/permissions", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		priv := "VM.Audit"
		if p == "/" || p == "/nodes" || p == "/access" {
			priv = "Sys.Audit"
		}
		if strings.HasPrefix(p, "/storage") {
			priv = "Datastore.Audit"
		}
		out := map[string]int{priv: 1}
		if mode == "hidden" && p == "/pool/hidden" {
			out = map[string]int{}
		}
		reply(w, map[string]any{p: out})
	})
	mux.HandleFunc("/api2/json/access/acl", func(w http.ResponseWriter, r *http.Request) {
		reply(w, []map[string]any{{"path": "/pool/hidden", "type": "token", "ugid": "reader@pve!read", "roleid": "NoAccess", "propagate": 1}})
	})
	mux.HandleFunc("/api2/json/storage/iso", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"type": "nfs", "server": "192.0.2.1", "export": "/seeds", "shared": 1, "content": "iso"})
	})
	mux.HandleFunc("/api2/json/cluster/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, []map[string]any{{"type": "cluster", "name": "test", "quorate": 1}, {"type": "node", "name": "n1", "nodeid": 1, "ip": "192.0.2.10"}})
	})
	reads := 0
	mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		reads++
		node := "n1"
		if mode == "migrate" && reads > 1 {
			node = "n2"
		}
		if mode == "null" {
			reply(w, nil)
			return
		}
		reply(w, []ClusterResource{{VMID: 7, Node: node, Type: "qemu"}})
	})
	mux.HandleFunc("/api2/json/nodes/n1/qemu/7/config", func(w http.ResponseWriter, r *http.Request) {
		if mode == "403" {
			http.Error(w, "denied", 403)
			return
		}
		cfg := map[string]any{"name": "foreign", "digest": "d"}
		if mode == "cfglock" {
			cfg["lock"] = "restore"
			cfg["memory"] = 128
		}
		q := r.URL.Query()
		if (mode == "current" && q.Get("current") == "1") || (mode == "merged" && q.Get("current") == "0") || (mode == "snapshot" && q.Get("snapshot") == "keep") {
			cfg["sata5"] = "alias:iso/" + testSeedName + ",media=cdrom"
		}
		reply(w, cfg)
	})
	mux.HandleFunc("/api2/json/nodes/n1/qemu/7/pending", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		if mode == "pending" {
			out = append(out, map[string]any{"key": "ide3", "pending": "other:iso/" + testSeedName})
		}
		reply(w, out)
	})
	mux.HandleFunc("/api2/json/nodes/n1/qemu/7/snapshot", func(w http.ResponseWriter, r *http.Request) {
		reply(w, []map[string]string{{"name": "current"}, {"name": "keep"}})
	})
	mux.HandleFunc("/api2/json/nodes/n1/qemu/7/snapshot/keep/config", func(w http.ResponseWriter, r *http.Request) {
		cfg := map[string]any{"name": "snapshot"}
		if mode == "snapshot" {
			cfg["ide1"] = "alias:iso/" + testSeedName + ",media=cdrom"
		}
		reply(w, cfg)
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{APIURL: srv.URL, TokenID: "reader@pve!read", Secret: "not-printed"})
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := c.seedClusterIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backing, err := c.seedBacking(context.Background(), "n1", "iso")
	if err != nil {
		t.Fatal(err)
	}
	return c, &SeedArtifact{Volume: "iso:iso/" + testSeedName, Node: "n1", Cluster: cluster, Backing: backing}, srv.Close
}
func TestSeedReferencesFailClosed(t *testing.T) {
	for _, mode := range []string{"clear", "current", "merged", "pending", "snapshot", "hidden", "403", "null", "migrate", "cfglock"} {
		t.Run(mode, func(t *testing.T) {
			c, a, done := seedInventoryFake(t, mode)
			defer done()
			err := c.SeedReferences(context.Background(), c, a)
			if mode == "clear" && err != nil {
				t.Fatal(err)
			}
			if mode != "clear" && err == nil {
				t.Fatal("absence was not proven")
			}
		})
	}
}
func TestOwnedSeedVolumeStrict(t *testing.T) {
	valid := "iso:iso/" + testSeedName
	if _, _, _, _, ok := ownedSeedVolume(valid); !ok {
		t.Fatal("valid owned name rejected")
	}
	for _, bad := range []string{"iso:iso/pcseed-7-x.iso", "iso:iso/../" + testSeedName, "iso:iso/sub/" + testSeedName, valid + ",media=cdrom", "x:" + valid, "iso:images/" + testSeedName, "iso:iso/evil" + testSeedName} {
		if _, _, _, _, ok := ownedSeedVolume(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
func TestSeedReferenceAliasesAndAllSlots(t *testing.T) {
	for _, key := range []string{"ide3", "sata5", "scsi29", "unused7", "arbitrary"} {
		if !seedMentions(map[string]any{key: "different-store:iso/" + testSeedName + ",media=cdrom"}, testSeedName) {
			t.Fatalf("missed %s alias", key)
		}
	}
}
