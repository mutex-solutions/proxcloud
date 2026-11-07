package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestBootISOProblem(t *testing.T) {
	caps := map[string]StorageCapability{}
	for _, d := range []StorageDef{
		{Storage: "local", Type: "dir", Content: "iso,vztmpl,backup"},
		{Storage: "local-zfs", Type: "zfspool", Content: "images,rootdir"},
		{Storage: "nas-templates", Type: "nfs", Content: "images,import,iso"},
		{Storage: "cephfs", Type: "cephfs", Content: "iso,backup"},
		{Storage: "dir-shared", Type: "dir", Content: "iso", Shared: 1},
	} {
		caps[d.Storage] = classify(d)
	}
	for _, c := range []struct {
		iso, boot, want string
	}{
		{"local", "local-zfs", ""},
		{"local", "local", ""},
		{"nas-templates", "local-zfs", "is shared"},
		{"cephfs", "local-zfs", "is shared"},
		{"dir-shared", "local-zfs", "is shared"},
		{"local-zfs", "local-zfs", "does not take ISO"},
		{"gone", "local-zfs", "does not exist"},
		// The token sees the boot store too, so a second iso store there makes the pick a coin toss.
		{"local", "nas-templates", "may put the bootstrap ISO on either"},
	} {
		got := BootISOProblem(caps, c.iso, c.boot)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("BootISOProblem(%s, %s) = %q, want %q", c.iso, c.boot, got, c.want)
		}
	}
}

// An old cluster's token moves from the NAS to the node-local store only when no node is still booting from an ISO on
// the old one, and only when its user holds the grant too.
func TestPinClusterISO(t *testing.T) {
	acl := []string{
		`{"path":"/storage/nas-templates","roleid":"PVEDatastoreAdmin","type":"token","ugid":"capmox@pve!pc-c-1","propagate":1}`,
		`{"path":"/storage/local-zfs","roleid":"ProxCloudCAPI","type":"token","ugid":"capmox@pve!pc-c-1","propagate":1}`,
		`{"path":"/storage/nas-templates","roleid":"PVEDatastoreAdmin","type":"token","ugid":"capmox@pve!pc-c-2","propagate":1}`,
	}
	userGrant := `{"path":"/storage/local","roleid":"PVEDatastoreAdmin","type":"user","ugid":"capmox@pve","propagate":0}`
	isos := `[{"volid":"nas-templates:iso/user-data-104.iso","content":"iso"}]`
	var puts []string
	afterRevoke := ""
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(acl, ","))
		},
		"GET /api2/json/nodes": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"node":"pve1","status":"online"},{"node":"pve2","status":"offline"}]}`)
		},
		"GET /api2/json/nodes/pve1/storage/nas-templates/content": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":%s}`, isos)
		},
		"PUT /api2/json/access/acl": func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			puts = append(puts, r.PostFormValue("path")+" "+r.PostFormValue("tokens")+" delete="+r.PostFormValue("delete"))
			if afterRevoke != "" && r.PostFormValue("delete") == "1" {
				isos = afterRevoke // a node starts booting from the old store just as it is taken away
			}
			fmt.Fprint(w, `{"data":null}`)
		},
		"GET /api2/json/access/users/capmox@pve": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"groups":[]}}`)
		},
		"GET /api2/json/access/roles": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"roleid":"PVEDatastoreAdmin","privs":"Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit"}]}`)
		},
	})
	ctx := context.Background()
	if _, err := c.PinClusterISO(ctx, "pc-c-1", "local"); !errors.Is(err, ErrBootISOGrant) || len(puts) > 0 {
		t.Fatalf("no user grant: err %v, writes %v", err, puts)
	}
	acl = append(acl, userGrant)
	if w, err := c.PinClusterISO(ctx, "pc-c-1", "local"); err != nil || w != "nas-templates:iso/user-data-104.iso" || len(puts) > 0 {
		t.Fatalf("a node is booting: waiting %q err %v writes %v", w, err, puts)
	}
	isos = `[{"volid":"nas-templates:iso/ubuntu.iso","content":"iso"},{"volid":"nas-templates:iso/pcseed-104-ab.iso","content":"iso"}]`
	if w, err := c.PinClusterISO(ctx, "pc-c-1", "local"); err != nil || w != "" {
		t.Fatalf("waiting %q err %v", w, err)
	}
	want := []string{"/storage/local capmox@pve!pc-c-1 delete=", "/storage/nas-templates capmox@pve!pc-c-1 delete=1"}
	if !slices.Equal(puts, want) {
		t.Fatalf("writes %v, want %v", puts, want)
	}
	// The race: an ISO lands on the old store between the look and the revoke. The old store comes back, so CAPMOX
	// can still delete the file, and the move waits.
	puts, afterRevoke = nil, `[{"volid":"nas-templates:iso/user-data-105.iso","content":"iso"}]`
	if w, err := c.PinClusterISO(ctx, "pc-c-2", "local"); err != nil || w != "nas-templates:iso/user-data-105.iso" {
		t.Fatalf("race: waiting %q err %v", w, err)
	}
	want = []string{"/storage/local capmox@pve!pc-c-2 delete=", "/storage/nas-templates capmox@pve!pc-c-2 delete=1", "/storage/nas-templates capmox@pve!pc-c-2 delete="}
	if !slices.Equal(puts, want) {
		t.Fatalf("race writes %v, want %v", puts, want)
	}
	isos, afterRevoke = `[]`, ""
	// Pinned already, or a token the portal never minted: nothing to write.
	acl = []string{userGrant, `{"path":"/storage/local","roleid":"PVEDatastoreAdmin","type":"token","ugid":"capmox@pve!pc-c-1","propagate":1}`}
	puts = nil
	for _, id := range []string{"pc-c-1", "pc-c-9"} {
		if w, err := c.PinClusterISO(ctx, id, "local"); err != nil || w != "" || len(puts) > 0 {
			t.Fatalf("%s: waiting %q err %v writes %v", id, w, err, puts)
		}
	}
}

// The user's grant on the ISO store can come from a group or from a parent with propagate, and a deeper entry for the
// user replaces what its parents and groups pass down (PVE::AccessControl::roles).
func TestEffectiveRoles(t *testing.T) {
	row := func(path, typ, ugid, role string, prop int) ACLEntry {
		return ACLEntry{Path: path, Type: typ, UGID: ugid, RoleID: role, Propagate: prop}
	}
	for _, c := range []struct {
		name string
		acls []ACLEntry
		want []string
	}{
		{"exact", []ACLEntry{row("/storage/local", "user", "capmox@pve", "PVEDatastoreAdmin", 0)}, []string{"PVEDatastoreAdmin"}},
		{"parent propagates", []ACLEntry{row("/storage", "user", "capmox@pve", "PVEDatastoreAdmin", 1)}, []string{"PVEDatastoreAdmin"}},
		{"parent without propagate", []ACLEntry{row("/storage", "user", "capmox@pve", "PVEDatastoreAdmin", 0)}, nil},
		{"group", []ACLEntry{row("/", "group", "capi-admins", "Administrator", 1)}, []string{"Administrator"}},
		{"other group", []ACLEntry{row("/", "group", "others", "Administrator", 1)}, nil},
		{"user beats group on a level", []ACLEntry{
			row("/storage/local", "group", "capi-admins", "PVEDatastoreAdmin", 0),
			row("/storage/local", "user", "capmox@pve", "ProxCloudCAPI", 0)}, []string{"ProxCloudCAPI"}},
		{"child shadows parent", []ACLEntry{
			row("/storage", "user", "capmox@pve", "PVEDatastoreAdmin", 1),
			row("/storage/local", "user", "capmox@pve", "ProxCloudCAPI", 0)}, []string{"ProxCloudCAPI"}},
		{"roles on one level add up", []ACLEntry{
			row("/storage/local", "user", "capmox@pve", "ProxCloudCAPI", 0),
			row("/storage/local", "user", "capmox@pve", "PVEDatastoreAdmin", 0),
			row("/storage/local", "token", "capmox@pve!capi", "Administrator", 0)}, []string{"ProxCloudCAPI", "PVEDatastoreAdmin"}},
	} {
		if got := effectiveRoles(c.acls, "capmox@pve", []string{"capi-admins"}, "/storage/local"); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
