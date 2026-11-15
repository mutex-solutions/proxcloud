package pve

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// A cluster gets its CAPMOX token alone. A refused ACL must not leave tokens behind, and the revoke still takes a CSI
// token a cluster from before the CSI gateway holds.
func TestMintClusterTokens(t *testing.T) {
	var acls, deleted, minted []string
	aclErr := false
	token := func(user string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{"full-tokenid":"%s!pc-c-1","value":"secret-%s"}}`, user, user)
		}
	}
	del := func(name string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			deleted = append(deleted, name)
			fmt.Fprint(w, `{"data":null}`)
		}
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api2/json/access/users/capmox@pve/token/pc-c-1": token("capmox@pve"),
		"POST /api2/json/access/users/csi@pve/token/pc-c-1": func(w http.ResponseWriter, r *http.Request) {
			minted = append(minted, "csi@pve")
			token("csi@pve")(w, r)
		},
		"DELETE /api2/json/access/users/capmox@pve/token/pc-c-1": del("capmox"),
		"DELETE /api2/json/access/users/csi@pve/token/pc-c-1":    del("csi"),
		"PUT /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) {
			if aclErr {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Permission check failed"}`)
				return
			}
			r.ParseForm()
			tok, _ := url.QueryUnescape(r.PostFormValue("tokens"))
			acls = append(acls, r.PostFormValue("path")+" "+r.PostFormValue("roles")+" "+tok)
			fmt.Fprint(w, `{"data":null}`)
		},
	})
	sc := ClusterTokenScope{
		Pool: "pc-p1", TemplatePool: "pc-templates", BootStorage: "local-zfs",
		ISOStorage: "nas", Zone: "z1", Vnet: "v1",
	}
	got, err := c.MintClusterTokens(context.Background(), "pc-c-1", sc)
	if err != nil {
		t.Fatal(err)
	}
	if got.CAPIID != "capmox@pve!pc-c-1" || got.CAPISecret != "secret-capmox@pve" {
		t.Errorf("tokens = %+v", got)
	}
	for _, want := range []string{"/sdn/zones/z1/v1 ProxCloudCAPI capmox@pve!pc-c-1", "/sdn/zones/localnetwork ProxCloudCAPI capmox@pve!pc-c-1",
		"/storage/local-zfs ProxCloudCAPI capmox@pve!pc-c-1"} {
		if !slices.Contains(acls, want) {
			t.Errorf("capi grants = %v, want %s", acls, want)
		}
	}
	// Tenant block volumes go through the CSI gateway: no cluster ever gets a CSI token.
	if minted != nil {
		t.Errorf("tokens minted for %v, want capmox@pve only", minted)
	}
	for _, a := range acls {
		if strings.Contains(a, "csi@pve") || strings.Contains(a, roleCSI) {
			t.Errorf("a CSI grant: %s", a)
		}
	}
	aclErr = true
	if _, err := c.MintClusterTokens(context.Background(), "pc-c-1", sc); err == nil {
		t.Fatal("a refused ACL must fail the mint")
	}
	if !slices.Contains(deleted, "capmox") || !slices.Contains(deleted, "csi") {
		t.Errorf("revoked = %v, want both tokens gone", deleted)
	}
}

func TestIsCSIDisk(t *testing.T) {
	for id, want := range map[string]bool{
		"local-zfs:vm-9999-pvc-0f0e":             true,
		"nas-capacity:9999/vm-9999-pvc-0f0e.raw": true,
		"local-zfs:vm-101-disk-0":                false,
		"nas-capacity:101/vm-101-disk-1.qcow2":   false,
	} {
		if isCSIDisk(id) != want {
			t.Errorf("isCSIDisk(%q) != %v", id, want)
		}
	}
}

// A release retries after a partial revoke, and a cluster without proxmox-csi has no CSI token: a token that is gone must
// not fail the revoke. PVE answers it with a 500.
func TestDeleteAPITokenAbsent(t *testing.T) {
	status, body := http.StatusInternalServerError, `{"errors":{"tokenid":"no such token 'pc-c-1' for user 'csi@pve'"}}`
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"DELETE /api2/json/access/users/csi@pve/token/pc-c-1": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		},
	})
	if err := c.DeleteAPIToken(context.Background(), "csi@pve", "pc-c-1"); err != nil {
		t.Fatalf("absent token: %v", err)
	}
	status, body = http.StatusForbidden, `{"message":"Permission check failed"}`
	if err := c.DeleteAPIToken(context.Background(), "csi@pve", "pc-c-1"); err == nil {
		t.Fatal("a refusal must not count as revoked")
	}
	// Our own token gone: an answer with the same words, and the cluster's token is still live.
	status, body = http.StatusUnauthorized, `{"errors":{"token":"no such token 'platform' for user 'proxcloud@pve'"}}`
	if err := c.DeleteAPIToken(context.Background(), "csi@pve", "pc-c-1"); err == nil {
		t.Fatal("a 401 must not count as revoked")
	}
	// PVE's reason goes in the status line, which a fake server cannot set.
	for _, c := range []struct {
		err  *APIError
		want bool
	}{
		{&APIError{Status: 500, Message: "no such token 'pc-c-1' for user 'csi@pve'"}, true},
		{&APIError{Status: 500, Message: "no such user ('csi@pve')"}, true},
		{&APIError{Status: 401, Message: "no such token 'platform' for user 'proxcloud@pve'"}, false},
		{&APIError{Status: 500, Message: "cluster not ready - no quorum?"}, false},
		{&APIError{Status: 500, Message: "no such pool"}, false},
	} {
		if got := isTokenAbsent(c.err); got != c.want {
			t.Errorf("isTokenAbsent(%v) = %v", c.err, got)
		}
	}
}

// The dialer has its own "no such host".
func TestDeleteAPITokenUnresolvableHost(t *testing.T) {
	c, err := New(Config{APIURL: "https://pve.invalid:8006", TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAPIToken(context.Background(), "csi@pve", "pc-c-1"); err == nil {
		t.Fatal("a dial error must not count as revoked")
	}
	if isTokenAbsent(errors.New("pve: DELETE /x: dial tcp: lookup pve: no such host")) {
		t.Fatal("no such host")
	}
}

// What the managed-services rows give each token, worked out as PVE does (effectiveRoles: the most specific level
// wins, a privilege-separated token holds its user's rights intersected with its own): the privilege tables hold, and
// the fences do. Lab check L1 asks PVE the same. The import store is the ISO store, as on a Compose site without a NAS
// (docs/quickstart.md): a tenant cluster's ISO row lands where the portal's PVEDatastoreAdmin row shadows /storage.
func TestManagedACLsGrantThePrivilegeTables(t *testing.T) {
	const boot, iso, imp = "local-lvm", "local", "local"
	roles := map[string][]string{ // a role not named here (NoAccess) gives nothing
		PlatformRole:   PlatformRolePrivileges(),
		roleTemplate:   {"VM.Audit", "VM.Clone"},
		roleISOStorage: {"Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.AllocateTemplate", "Datastore.Audit"},
	}
	for _, r := range ManagedRoles {
		roles[r.Name] = r.Privs
	}
	rows := func(extra ...ManagedACL) []ACLEntry {
		all := slices.Concat(ManagedACLs(DefaultPlatformTokenID, boot, iso, imp), extra)
		for _, a := range PlatformACLs {
			all = append(all, ManagedACL{a.Path, a.Role, a.Propagate, "proxcloud@pve", DefaultPlatformTokenID})
		}
		all = append(all, ManagedACL{"/storage/" + imp, roleISOStorage, false, "proxcloud@pve", DefaultPlatformTokenID}) // PlatformSetupScript's
		var out []ACLEntry
		for _, a := range all {
			for _, id := range []string{a.User, a.Token} {
				if id != "" { // effectiveRoles walks one principal; a token is walked the same way as a user
					out = append(out, ACLEntry{Path: a.Path, RoleID: a.Role, Type: "user", UGID: id, Propagate: map[bool]int{true: 1}[a.Propagate]})
				}
			}
		}
		return out
	}
	effective := func(acls []ACLEntry, tokenID, path string) map[string]bool {
		privs := func(id string) map[string]bool {
			out := map[string]bool{}
			for _, r := range effectiveRoles(acls, id, nil, path) {
				for _, p := range roles[r] {
					out[p] = true
				}
			}
			return out
		}
		user, _, _ := strings.Cut(tokenID, "!")
		u, tok := privs(user), privs(tokenID)
		for p := range tok {
			if !u[p] {
				delete(tok, p)
			}
		}
		return tok
	}
	// The CSI gateway's rows of an installer site, which the seed's NoAccess on csi@pve must cap too.
	acls := rows(ManagedACL{"/pool", roleCSI, true, csiUser, "csi@pve!gateway"}, ManagedACL{"/storage", roleCSI, true, csiUser, "csi@pve!gateway"})
	if !slices.ContainsFunc(KubeMintPrivileges(imp), func(c PrivCheck) bool { return c.Path == "/storage/"+imp }) {
		t.Errorf("KubeMintPrivileges does not check /storage/%s, where its own row shadows /storage", imp)
	}
	for tokenID, checks := range map[string][]PrivCheck{
		CAPITokenID:            CAPIPrivileges(boot, iso),
		CSITokenID:             CSIPrivileges(boot),
		DefaultPlatformTokenID: KubeMintPrivileges(imp),
	} {
		for _, chk := range checks {
			have := effective(acls, tokenID, chk.Path)
			for _, p := range chk.Privs {
				if !have[p] {
					t.Errorf("%s lacks %s on %s", tokenID, p, chk.Path)
				}
			}
		}
	}
	// Nothing where a token must hold nothing: the portal on both platform pools, the CSI token on the seed's pool,
	// the platform tokens on a tenant pool and on another store. The seed's pool fences the seed VM only: its disk is on
	// boot, where CSIPrivileges itself wants Datastore.Allocate, and PVE lets that open every volume on the store.
	for _, c := range []struct{ tokenID, path string }{
		{DefaultPlatformTokenID, "/pool/" + SystemPool}, {DefaultPlatformTokenID, "/pool/" + SeedPool},
		{CSITokenID, "/pool/" + SeedPool}, {"csi@pve!gateway", "/pool/" + SeedPool},
		{CAPITokenID, "/pool/pc-p1"}, {CSITokenID, "/pool/pc-p1"},
		{CAPITokenID, "/storage/nas-images"}, {CSITokenID, "/storage/nas-images"},
	} {
		if have := effective(acls, c.tokenID, c.path); len(have) > 0 {
			t.Errorf("%s holds %v on %s, want nothing", c.tokenID, slices.Sorted(maps.Keys(have)), c.path)
		}
	}
	// A stolen portal token can mint a token for capmox@pve or csi@pve with a /pool row of its own and grant the user
	// /pool too (ProxCloudKubeMint). The users' own rows on the platform pools still cap such a token there; the
	// existing capi token keeps exactly its role.
	rogue := rows(ManagedACL{"/pool", PlatformRole, true, capiUser, "capmox@pve!x"}, ManagedACL{"/pool", PlatformRole, true, csiUser, "csi@pve!x"})
	for _, c := range []struct{ tokenID, pool, atMost string }{
		{"capmox@pve!x", SystemPool, roleCAPI}, {"capmox@pve!x", SeedPool, roleCAPI},
		{CAPITokenID, SystemPool, roleCAPI}, {CAPITokenID, SeedPool, roleCAPI},
		{"csi@pve!x", SystemPool, roleCSI}, {"csi@pve!x", SeedPool, "NoAccess"},
	} {
		have := effective(rogue, c.tokenID, "/pool/"+c.pool)
		for p := range have {
			if !slices.Contains(roles[c.atMost], p) {
				t.Errorf("after a /pool grant %s holds %v on /pool/%s, more than %s", c.tokenID, slices.Sorted(maps.Keys(have)), c.pool, c.atMost)
				break
			}
		}
	}
	for _, pool := range []string{SystemPool, SeedPool} {
		if have := effective(rogue, CAPITokenID, "/pool/"+pool); len(have) != len(ManagedRoles[0].Privs) {
			t.Errorf("after a /pool grant %s holds %v on /pool/%s, want %s", CAPITokenID, slices.Sorted(maps.Keys(have)), pool, roleCAPI)
		}
	}
}
