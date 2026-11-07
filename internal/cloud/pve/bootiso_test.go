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
			fmt.Fprint(w, `{"data":null}`)
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
	// Pinned already, or a token the portal never minted: nothing to write.
	acl = []string{userGrant, `{"path":"/storage/local","roleid":"PVEDatastoreAdmin","type":"token","ugid":"capmox@pve!pc-c-1","propagate":1}`}
	puts = nil
	for _, id := range []string{"pc-c-1", "pc-c-9"} {
		if w, err := c.PinClusterISO(ctx, id, "local"); err != nil || w != "" || len(puts) > 0 {
			t.Fatalf("%s: waiting %q err %v writes %v", id, w, err, puts)
		}
	}
}
