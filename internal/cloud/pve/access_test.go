package pve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// The CSI token reaches the tenant cluster, so it may touch only the pool and the stores. A refused
// ACL must not leave tokens behind.
func TestMintClusterTokens(t *testing.T) {
	var acls, deleted []string
	aclErr := false
	token := func(user string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"data":{"full-tokenid":"%s!stc-1","value":"secret-%s"}}`, user, user)
		}
	}
	del := func(name string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			deleted = append(deleted, name)
			fmt.Fprint(w, `{"data":null}`)
		}
	}
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api2/json/access/users/capmox@pve/token/stc-1":   token("capmox@pve"),
		"POST /api2/json/access/users/csi@pve/token/stc-1":      token("csi@pve"),
		"DELETE /api2/json/access/users/capmox@pve/token/stc-1": del("capmox"),
		"DELETE /api2/json/access/users/csi@pve/token/stc-1":    del("csi"),
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
		Pool: "pc-p1", TemplatePool: "pc-templates", Storages: []string{"local-zfs", "ssd"},
		ISOStorage: "nas", Zone: "z1", Vnet: "v1",
	}
	got, err := c.MintClusterTokens(context.Background(), "stc-1", sc)
	if err != nil {
		t.Fatal(err)
	}
	if got.CAPIID != "capmox@pve!stc-1" || got.CSIID != "csi@pve!stc-1" || got.CSISecret != "secret-csi@pve" {
		t.Errorf("tokens = %+v", got)
	}
	var csi []string
	for _, a := range acls {
		if strings.HasSuffix(a, "csi@pve!stc-1") {
			csi = append(csi, a)
		}
	}
	slices.Sort(csi)
	want := []string{
		"/pool/pc-p1 ProxCloudCSI csi@pve!stc-1",
		"/storage/local-zfs ProxCloudCSI csi@pve!stc-1",
		"/storage/ssd ProxCloudCSI csi@pve!stc-1",
	}
	if !slices.Equal(csi, want) {
		t.Errorf("csi grants = %v, want %v", csi, want)
	}
	if !slices.Contains(acls, "/sdn/zones/z1/v1 ProxCloudCAPI capmox@pve!stc-1") {
		t.Errorf("capi grants = %v", acls)
	}
	aclErr = true
	if _, err := c.MintClusterTokens(context.Background(), "stc-1", sc); err == nil {
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
