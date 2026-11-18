package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mintPVE is a Proxmox that answers from its ACL list the way PVE does: a token's effective privileges by
// effectiveRoles (the most specific level with a row of its own wins; a privilege-separated token holds its user's
// rights intersected with its own), and an ACL write by PVE::RPCEnvironment::check_perm_modify (Permissions.Modify on
// the path, or Datastore.Allocate on a store, or Pool.Allocate on a pool), refused in PVE's words.
type mintPVE struct {
	acl    []ACLEntry
	roles  map[string][]string
	tokens []string // the tokens created and not deleted
}

func newMintPVE(extra ...ACLEntry) *mintPVE {
	f := &mintPVE{roles: map[string][]string{
		PlatformRole:   PlatformRolePrivileges(),
		roleTemplate:   {"VM.Audit", "VM.Clone"},
		roleISOStorage: {"Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.AllocateTemplate", "Datastore.Audit"},
		"PVEAuditor":   {"Datastore.Audit", "Sys.Audit", "VM.Audit"},
	}}
	for _, r := range ManagedRoles {
		f.roles[r.Name] = r.Privs
	}
	portal, _, _ := strings.Cut(DefaultPlatformTokenID, "!")
	for _, a := range PlatformACLs {
		f.grant(a.Path, a.Role, a.Propagate, portal, DefaultPlatformTokenID)
	}
	for _, a := range ManagedACLs(DefaultPlatformTokenID, "local-lvm", "local", "") {
		f.grant(a.Path, a.Role, a.Propagate, a.User, a.Token)
	}
	f.acl = append(f.acl, extra...)
	return f
}

func (f *mintPVE) grant(path, role string, propagate bool, user, token string) {
	for kind, id := range map[string]string{"user": user, "token": token} {
		if id != "" {
			f.set(ACLEntry{Path: path, RoleID: role, Type: kind, UGID: id, Propagate: map[bool]int{true: 1}[propagate]})
		}
	}
}

// set writes one row as PUT /access/acl does: the same path, principal and role is one row.
func (f *mintPVE) set(row ACLEntry) {
	f.acl = slices.DeleteFunc(f.acl, func(a ACLEntry) bool { return a.Path == row.Path && a.UGID == row.UGID && a.RoleID == row.RoleID })
	f.acl = append(f.acl, row)
}

func (f *mintPVE) privs(tokenID, path string) map[string]bool {
	walk := make([]ACLEntry, len(f.acl)) // effectiveRoles walks one principal; a token is walked the same way as a user
	for i, a := range f.acl {
		a.Type = "user"
		walk[i] = a
	}
	of := func(id string) map[string]bool {
		out := map[string]bool{}
		for _, r := range effectiveRoles(walk, id, nil, path) {
			for _, p := range f.roles[r] {
				out[p] = true
			}
		}
		return out
	}
	user, _, _ := strings.Cut(tokenID, "!")
	u, tok := of(user), of(tokenID)
	for p := range tok {
		if !u[p] {
			delete(tok, p)
		}
	}
	return tok
}

func (f *mintPVE) serve(w http.ResponseWriter, r *http.Request) {
	caller := strings.TrimPrefix(r.Header.Get("Authorization"), "PVEAPIToken=")
	caller = caller[:strings.LastIndex(caller, "=")]
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	_ = r.ParseForm()
	switch {
	case r.Method == http.MethodGet && path == "/access/permissions":
		p := r.Form.Get("path")
		b, _ := json.Marshal(map[string]any{"data": map[string]any{p: f.privs(caller, p)}})
		_, _ = w.Write(b)
	case r.Method == http.MethodPost && strings.Contains(path, "/token/"):
		seg := strings.Split(path, "/") // /access/users/<user>/token/<name>
		id := seg[3] + "!" + seg[5]
		f.tokens = append(f.tokens, id)
		fmt.Fprintf(w, `{"data":{"full-tokenid":%q,"value":"secret"}}`, id)
	case r.Method == http.MethodDelete && strings.Contains(path, "/token/"):
		seg := strings.Split(path, "/")
		id := seg[3] + "!" + seg[5]
		f.tokens = slices.DeleteFunc(f.tokens, func(t string) bool { return t == id })
		f.acl = slices.DeleteFunc(f.acl, func(a ACLEntry) bool { return a.UGID == id })
		fmt.Fprint(w, `{"data":null}`)
	case r.Method == http.MethodPut && path == "/access/acl":
		p, have := r.Form.Get("path"), f.privs(caller, r.Form.Get("path"))
		need := "Permissions.Modify"
		switch {
		case strings.HasPrefix(p, "/storage/"):
			need += "|Datastore.Allocate"
		case strings.HasPrefix(p, "/pool/"):
			need += "|Pool.Allocate"
		}
		if !slices.ContainsFunc(strings.Split(need, "|"), func(n string) bool { return have[n] }) {
			// PVE's refusal is the status line: "403 Permission check failed (<path>, <privileges>)".
			conn, _, _ := w.(http.Hijacker).Hijack()
			fmt.Fprintf(conn, "HTTP/1.1 403 Permission check failed (%s, %s)\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"data\":null}", p, need)
			_ = conn.Close()
			return
		}
		for _, tok := range strings.Split(r.Form.Get("tokens"), ",") {
			f.set(ACLEntry{Path: p, RoleID: r.Form.Get("roles"), Type: "token", UGID: tok, Propagate: 1})
		}
		fmt.Fprint(w, `{"data":null}`)
	default:
		http.Error(w, "no fake for "+r.Method+" "+path, http.StatusNotImplemented)
	}
}

// Production, 2026-11-17 (v0.8.0): the portal's user and token held older rows of their own on single stores
// (ProxCloudPlatform on /storage/nvme-u2). In Proxmox a row on a path replaces what its parent passes down, so the mint
// role on /storage did not reach that store, PVE refused the tenant cluster token's row there, and every cluster create
// failed, while the Check read /storage alone and was green. The Check now reads the store, the script repeats the mint
// role next to each such row, and the mint then passes.
func TestMintWithAnOwnRowOnTheBootStore(t *testing.T) {
	portal, _, _ := strings.Cut(DefaultPlatformTokenID, "!")
	own := func(path, role, kind, id string, propagate int) ACLEntry {
		return ACLEntry{Path: path, RoleID: role, Type: kind, UGID: id, Propagate: propagate}
	}
	f := newMintPVE(
		own("/storage/nvme-u2", PlatformRole, "user", portal, 1), own("/storage/nvme-u2", PlatformRole, "token", DefaultPlatformTokenID, 1),
		// The user alone: its token has no row there and keeps what /storage passes down.
		own("/storage/tank", "PVEAuditor", "user", portal, 0),
		// A zone whose rows do not propagate: its vnets still inherit from /sdn/zones, and must after the script too.
		own("/sdn/zones/lan", PlatformRole, "user", portal, 0), own("/sdn/zones/lan", PlatformRole, "token", DefaultPlatformTokenID, 0),
		// A vnet: a mint writes there. A bridge of localnetwork and a VLAN below a vnet: no mint does, so the script
		// must not put Permissions.Modify there.
		own("/sdn/zones/pc/v7", "PVEAuditor", "user", portal, 0),
		own("/sdn/zones/localnetwork/vmbr0", "PVEAuditor", "user", portal, 0), own("/sdn/zones/pc/v7/100", "PVEAuditor", "user", portal, 0),
	)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIURL: srv.URL, TokenID: DefaultPlatformTokenID, Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := ClusterTokenScope{Pool: "pc-p1", TemplatePool: TemplatePool, BootStorage: "nvme-u2", ISOStorage: "local", Zone: "pc", Vnet: "v1"}

	// The parents alone, as the Check read them in v0.8.0: complete.
	if missing, err := c.MissingPrivileges(ctx, KubeMintPrivileges(nil, nil)); err != nil || len(missing) != 0 {
		t.Fatalf("the parents: %v, %v", missing, err)
	}
	// The paths a mint writes: the stores with a row of their own fail, by name, and nothing else does.
	checks := KubeMintPrivileges([]string{"local", "nvme-u2", "tank"}, []string{"pc", "lan"})
	missing, err := c.MissingPrivileges(ctx, checks)
	if want := []string{"Permissions.Modify on /sdn/zones/lan", "Permissions.Modify on /storage/nvme-u2", "Permissions.Modify on /storage/tank"}; err != nil || !slices.Equal(sorted(missing), want) {
		t.Fatalf("the Check reads %v, %v; want %v", missing, err, want)
	}
	// The mint is refused on the store, in PVE's words, and leaves no token behind.
	if _, err := c.MintClusterTokens(ctx, "pc-c-1", scope); !IsForbidden(err) || !strings.Contains(err.Error(), "(/storage/nvme-u2, Permissions.Modify|Datastore.Allocate)") {
		t.Fatalf("a mint on a store whose own row hides the mint role: %v", err)
	}
	if len(f.tokens) != 0 {
		t.Fatalf("a refused mint left %v", f.tokens)
	}

	// The script, run as on a node: pveum answers `acl list` from the same ACL and records every call.
	for _, bin := range []string{"sh", "perl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s", bin)
		}
	}
	script, err := ManagedSetupScript(DefaultPlatformTokenID, false, "", "local-lvm", "local")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fake := "#!/bin/sh\necho \"$*\" >> " + calls + "\nif [ \"$1 $2\" = 'acl list' ]; then printf '%s' \"$FAKE_ACL\"; fi\n"
	for _, bin := range []string{"pveum", "pvenode"} {
		if err := os.WriteFile(filepath.Join(dir, bin), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := json.Marshal(f.acl)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "FAKE_ACL=" + string(list)}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script: %v\n%s", err, out)
	}
	log, _ := os.ReadFile(calls)
	var repeated []string
	for _, l := range strings.Split(string(log), "\n") {
		// acl modify <path> --users|--tokens <id> --roles <role> --propagate <n>
		w := strings.Fields(l)
		if len(w) != 9 || w[0]+" "+w[1] != "acl modify" {
			continue
		}
		if strings.HasPrefix(w[2], "/storage/") && w[2] != "/storage/local" && w[2] != "/storage/local-lvm" || strings.HasPrefix(w[2], "/sdn/zones/") {
			repeated = append(repeated, strings.Join(w[2:], " "))
		}
		f.set(ACLEntry{Path: w[2], RoleID: w[6], Type: map[string]string{"--users": "user", "--tokens": "token"}[w[3]], UGID: w[4], Propagate: map[string]int{"1": 1}[w[8]]})
	}
	// One row per row that hid the role, for the one that holds it, propagated as that row is. Nothing for the token
	// on /storage/tank: a mint row there would replace what the token inherits from /storage.
	if want := []string{
		"/sdn/zones/lan --tokens proxcloud@pve!platform --roles ProxCloudKubeMint --propagate 0",
		"/sdn/zones/lan --users proxcloud@pve --roles ProxCloudKubeMint --propagate 0",
		"/sdn/zones/pc/v7 --users proxcloud@pve --roles ProxCloudKubeMint --propagate 0",
		"/storage/nvme-u2 --tokens proxcloud@pve!platform --roles ProxCloudKubeMint --propagate 1",
		"/storage/nvme-u2 --users proxcloud@pve --roles ProxCloudKubeMint --propagate 1",
		"/storage/tank --users proxcloud@pve --roles ProxCloudKubeMint --propagate 0",
	}; !slices.Equal(sorted(repeated), want) {
		t.Fatalf("the script repeated the mint role on\n%s\nwant\n%s", strings.Join(sorted(repeated), "\n"), strings.Join(want, "\n"))
	}

	if missing, err := c.MissingPrivileges(ctx, checks); err != nil || len(missing) != 0 {
		t.Fatalf("after the script the Check reads %v, %v", missing, err)
	}
	// What the rows gave stays, and a vnet of the zone still inherits from /sdn/zones.
	if have := f.privs(DefaultPlatformTokenID, "/storage/nvme-u2"); !have["Datastore.AllocateSpace"] {
		t.Errorf("the portal token lost its own role on the store: %v", have)
	}
	if have := f.privs(DefaultPlatformTokenID, "/sdn/zones/lan/v9"); !have["SDN.Use"] || !have["Permissions.Modify"] {
		t.Errorf("a vnet of the zone no longer inherits from /sdn/zones: %v", have)
	}
	toks, err := c.MintClusterTokens(ctx, "pc-c-1", scope)
	if err != nil || toks.CAPIID != "capmox@pve!pc-c-1" || !slices.Equal(f.tokens, []string{"capmox@pve!pc-c-1"}) {
		t.Fatalf("a mint after the script: %+v, %v (tokens %v)", toks, err, f.tokens)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}
