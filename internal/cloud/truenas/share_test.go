package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestEmptyAccessNeverMeansEveryone(t *testing.T) {
	s := &Service{}
	if got := s.exportList(nil); len(got) != 1 || got[0] != "127.0.0.1/32" {
		t.Fatalf("exportList(nil) = %v", got)
	}
	if got := s.exportList([]string{"10.0.0.0/24"}); len(got) != 1 || got[0] != "10.0.0.0/24" {
		t.Fatalf("exportList = %v", got)
	}
}

// A site's NAT address goes on every export, once, and replaces the placeholder that closes an empty one.
func TestExportKeepsSNATSources(t *testing.T) {
	s := &Service{SNATSources: []string{"192.0.2.1/32"}}
	if got := strings.Join(s.exportList([]string{"10.0.0.0/24"}), ","); got != "10.0.0.0/24,192.0.2.1/32" {
		t.Fatalf("exportList = %s", got)
	}
	if got := strings.Join(s.exportList([]string{"192.0.2.1/32"}), ","); got != "192.0.2.1/32" {
		t.Fatalf("duplicate address: %s", got)
	}
	if got := strings.Join(s.exportList(nil), ","); got != "192.0.2.1/32" {
		t.Fatalf("empty list: %s", got)
	}
	if !s.SameExport([]string{"192.0.2.1/32", "10.0.0.0/24"}, []string{"10.0.0.0/24"}) {
		t.Fatal("an export holding the NAT address must count as current")
	}
	if s.SameExport([]string{"10.0.0.0/24"}, []string{"10.0.0.0/24"}) {
		t.Fatal("an export missing the NAT address must be rewritten")
	}
	if (&Service{}).SameExport([]string{"192.0.2.1/32", "10.0.0.0/24"}, []string{"10.0.0.0/24"}) {
		t.Fatal("with the setting gone the NAT address must come off")
	}
}

func TestPlainHTTPRefused(t *testing.T) {
	for _, u := range []string{"http://nas", "ws://nas/api/current"} {
		if _, err := New(Config{URL: u, Username: "u", APIKey: "k"}); err == nil {
			t.Fatalf("%s accepted", u)
		}
	}
	c, err := New(Config{URL: "https://nas:8443/", Username: "u", APIKey: "k"})
	if err != nil || c.cfg.URL != "wss://nas:8443/api/current" {
		t.Fatalf("url = %v, err %v", c, err)
	}
}

func TestSMBUserStaysWithinTheLimit(t *testing.T) {
	if u := smbUser("6aee381651ff712e6b0029bd", "a-very-long-share-name-of-32-char"); len(u) > 32 {
		t.Fatalf("%s is %d long", u, len(u))
	}
}

// Two long names with the same start, in one project, used to truncate to one account.
func TestSMBUsersDoNotCollide(t *testing.T) {
	p := "6aee381651ff712e6b0029bd"
	long := "a-very-long-share-name-of-32-ch"
	if smbUser(p, long+"a") == smbUser(p, long+"b") {
		t.Fatal("same user for two shares")
	}
	if smbUser(p, "data") == smbUser("7aee381651ff712e6b0029bd", "data") {
		t.Fatal("same user for two projects")
	}
}

// sizeGb=2^34 made size<<30 wrap to 0, which ZFS reads as "no quota".
func TestShareSizeCappedBeforeTheShift(t *testing.T) {
	c, err := New(Config{URL: "https://127.0.0.1:1", Username: "u", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{C: c, Parent: "tank/shares"}
	if _, err := s.Create(context.Background(), "p", "data", ProtocolNFS, 17179869184, []string{"10.0.0.0/24"}); !errors.Is(err, ErrShare) {
		t.Fatalf("create: %v", err)
	}
	if err := s.Resize(context.Background(), "p", "tank/shares/p-data", 17179869184); !errors.Is(err, ErrShare) {
		t.Fatalf("resize: %v", err)
	}
}

func TestSameExport(t *testing.T) {
	s := &Service{}
	if !s.SameExport([]string{"b", "a"}, []string{"a", "b"}) {
		t.Fatal("order must not matter")
	}
	if s.SameExport([]string{"a", "b"}, []string{"a"}) {
		t.Fatal("a stale address must be a difference")
	}
	if !s.SameExport([]string{"127.0.0.1/32"}, nil) {
		t.Fatal("an empty list is written as localhost")
	}
}

// fakeNAS answers the JSON-RPC calls of one SMB share p-long and records them.
type fakeNAS struct {
	mu    sync.Mutex
	calls map[string][]string
	users []map[string]any
	nfs   []map[string]any // sharing.nfs.query rows; a share listed here is NFS
}

func (f *fakeNAS) got(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeNAS) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]string{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			var req struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				return
			}
			f.mu.Lock()
			f.calls[req.Method] = append(f.calls[req.Method], string(req.Params))
			users, nfs := f.users, f.nfs
			f.mu.Unlock()
			var result any = true
			switch req.Method {
			case "auth.login_ex":
				result = map[string]any{"response_type": "SUCCESS"}
			case "pool.dataset.query":
				if strings.Contains(string(req.Params), "tank/shares/p-") {
					result = []any{map[string]any{"id": "tank/shares/p-long", "user_properties": map[string]any{
						projectProp: map[string]any{"rawvalue": "p"}, protoProp: map[string]any{"rawvalue": map[bool]string{true: ProtocolNFS, false: ProtocolSMB}[nfs != nil]},
					}}}
				} else {
					result = []any{map[string]any{"id": "tank/shares", "available": map[string]any{"rawvalue": "999999999999999"}}}
				}
			case "filesystem.setperm":
				result = 7
			case "core.get_jobs":
				result = []any{map[string]any{"state": "SUCCESS"}}
			case "sharing.nfs.create", "sharing.smb.create":
				result = map[string]any{"id": 11}
			case "sharing.nfs.query":
				result = []any{}
				if nfs != nil {
					result = nfs
				}
			case "sharing.smb.query":
				result = []any{map[string]any{"id": 11, "path": "/mnt/tank/shares/p-long"}}
				if nfs != nil {
					result = []any{}
				}
			case "user.query":
				result = users
			}
			if err := wsjson.Write(r.Context(), conn, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, Username: "u", APIKey: "k", InsecureTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestSetAccessSquashesAnOldExport: an export from before root squash loses maproot, and its top directory
// opens first so a client's root does not lose its way in.
func TestSetAccessSquashesAnOldExport(t *testing.T) {
	for _, maproot := range []string{"root", ""} {
		f := &fakeNAS{nfs: []map[string]any{{"id": 12, "path": "/mnt/tank/shares/p-long", "networks": []string{"10.0.0.1/32"}, "maproot_user": maproot}}}
		s := &Service{C: f.serve(t), Parent: "tank/shares"}
		list, err := s.List(context.Background(), "p")
		if err != nil || len(list) != 1 {
			t.Fatalf("list: %+v, %v", list, err)
		}
		if list[0].MapsRoot != (maproot != "") {
			t.Fatalf("maproot %q: MapsRoot = %v", maproot, list[0].MapsRoot)
		}
		if err := s.SetAccess(context.Background(), "p", "tank/shares/p-long", []string{"10.0.0.2/32"}); err != nil {
			t.Fatal(err)
		}
		update := f.got("sharing.nfs.update")
		if len(update) != 1 || !strings.Contains(update[0], `"maproot_user":null`) || !strings.Contains(update[0], `"maproot_group":null`) {
			t.Fatalf("update = %v", update)
		}
		perms := f.got("filesystem.setperm")
		if maproot == "" && len(perms) != 0 {
			t.Fatalf("an export already squashing had its directory changed: %v", perms)
		}
		if maproot != "" && (len(perms) != 1 || !strings.Contains(perms[0], `"mode":"777"`) || strings.Contains(perms[0], "recursive")) {
			t.Fatalf("setperm = %v", perms)
		}
	}
}

func TestNFSExportSquashesRoot(t *testing.T) {
	f := &fakeNAS{}
	s := &Service{C: f.serve(t), Parent: "tank/shares"}
	if _, err := s.Create(context.Background(), "p", "data", ProtocolNFS, 1, []string{"10.0.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	exports := f.got("sharing.nfs.create")
	if len(exports) != 1 || strings.Contains(exports[0], "root") {
		t.Fatalf("export = %v", exports)
	}
	if perm := f.got("filesystem.setperm"); len(perm) != 1 || !strings.Contains(perm[0], `"777"`) {
		t.Fatalf("setperm = %v", perm)
	}
}

// A new export carries the NAT address from the start: a K8s RWX dataset is created this way and mounted right after.
func TestCreateExportsSNATSources(t *testing.T) {
	f := &fakeNAS{}
	s := &Service{C: f.serve(t), Parent: "tank/shares", SNATSources: []string{"192.0.2.1/32"}}
	sh, err := s.Create(context.Background(), "p", "k8s-abc", ProtocolNFS, 1, []string{"10.0.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if exports := f.got("sharing.nfs.create"); len(exports) != 1 || !strings.Contains(exports[0], "192.0.2.1/32") || !strings.Contains(exports[0], "10.0.0.0/24") {
		t.Fatalf("export = %v", exports)
	}
	if data := s.ShareData("p", sh)["share"].(map[string]any); strings.Join(data["sharedNat"].([]string), ",") != "192.0.2.1/32" {
		t.Fatalf("row = %v", data)
	}
}

// A legacy truncated name held by another share: deleting this share must leave that account alone.
func TestDeleteLeavesAnotherSharesUser(t *testing.T) {
	f := &fakeNAS{users: []map[string]any{{"id": 42, "full_name": smbUserFullName("a-different-share")}}}
	s := &Service{C: f.serve(t), Parent: "tank/shares"}
	if err := s.Delete(context.Background(), "p", "tank/shares/p-long"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.got("user.delete")); n != 0 {
		t.Fatalf("user.delete called %d times", n)
	}
	if n := len(f.got("pool.dataset.delete")); n != 1 {
		t.Fatalf("dataset delete called %d times", n)
	}

	f.mu.Lock()
	f.users = []map[string]any{{"id": 42, "full_name": smbUserFullName("long")}}
	f.mu.Unlock()
	if err := s.Delete(context.Background(), "p", "tank/shares/p-long"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.got("user.delete")); n != 1 {
		t.Fatalf("own user: user.delete called %d times", n)
	}
}
