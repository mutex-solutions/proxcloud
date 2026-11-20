// Package pvetest holds fakes of the Proxmox API for tests, which answer as Proxmox does where a kinder fake would
// hide a defect.
package pvetest

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Backup is one vzdump archive on a store.
type Backup struct {
	VMID  int
	CTime int64
	Notes string
}

// BackupSite is a Proxmox node's backup stores as each API token sees them. Proxmox does not refuse the listing of a
// store's backups to a token that may not read some of them: it answers 200 and leaves those out
// (PVE::API2::Storage::Content index runs every volume through PVE::Storage::check_volume_access). A backup of a VM
// is listed to a token that holds Datastore.Allocate on the store, or Datastore.AllocateSpace on the store with
// VM.Backup on the VM. A token sees a store, and may list it, with Datastore.Audit or Datastore.AllocateSpace on it.
// Seen on a lab (v0.10.0-rc.1): a token without VM.Backup on VM 102 got `{"data":[]}` with today's archive of 102 on
// the store.
type BackupSite struct {
	Mu sync.Mutex
	// ACL is the effective privileges of each token ("user@pve!name") on each path ("/storage/nas", "/vms/106").
	ACL map[string]map[string][]string
	// Stores is each store's content types ("backup,iso"); Backups its archives; VMCTime the creation time in the
	// config of each VM (read with VM.Audit).
	Stores  map[string]string
	Backups map[string][]Backup
	VMCTime map[int]int64
}

func (s *BackupSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	token, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "PVEAPIToken="), "=")
	has := func(path string, privs ...string) bool {
		return slices.ContainsFunc(privs, func(p string) bool { return slices.Contains(s.ACL[token][path], p) })
	}
	data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api2/json"), "/"), "/")
	switch {
	case r.Method != http.MethodGet:
		w.WriteHeader(http.StatusNotImplemented)
	case len(parts) == 2 && parts[0] == "access" && parts[1] == "permissions":
		path, privs := r.URL.Query().Get("path"), map[string]int{}
		for _, p := range s.ACL[token][path] {
			privs[p] = 1
		}
		data(map[string]any{path: privs})
	case len(parts) == 3 && parts[0] == "nodes" && parts[2] == "storage":
		out := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(s.Stores)) {
			if has("/storage/"+name, "Datastore.Audit", "Datastore.AllocateSpace") {
				out = append(out, map[string]any{"storage": name, "content": s.Stores[name]})
			}
		}
		data(out)
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "storage" && parts[4] == "content":
		store := parts[3]
		if _, ok := s.Stores[store]; !ok || !has("/storage/"+store, "Datastore.Audit", "Datastore.AllocateSpace") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		only, _ := strconv.Atoi(r.URL.Query().Get("vmid"))
		out := []map[string]any{}
		for _, b := range s.Backups[store] {
			if only != 0 && b.VMID != only {
				continue
			}
			if has("/storage/"+store, "Datastore.Allocate") || (has("/storage/"+store, "Datastore.AllocateSpace") && has(fmt.Sprintf("/vms/%d", b.VMID), "VM.Backup")) {
				out = append(out, map[string]any{"volid": fmt.Sprintf("%s:backup/vzdump-qemu-%d-%d.vma.zst", store, b.VMID, b.CTime), "content": "backup",
					"vmid": b.VMID, "ctime": b.CTime, "notes": b.Notes})
			}
		}
		data(out)
	case len(parts) == 5 && parts[0] == "nodes" && parts[2] == "qemu" && parts[4] == "config":
		vmid, _ := strconv.Atoi(parts[3])
		if ct, ok := s.VMCTime[vmid]; ok && has("/vms/"+parts[3], "VM.Audit") {
			data(map[string]any{"meta": fmt.Sprintf("creation-qemu=10.0.2,ctime=%d", ct)})
			return
		}
		w.WriteHeader(http.StatusForbidden)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// ProductACL is what the product's own setup gives its two tokens for a seed VM and the stores named: the portal token
// reads every store and nothing of the seed VM (NoAccess on its pool) and manages the import store; the CAPI token sees
// the boot and the ISO store alone, manages the ISO store, and holds no VM.Backup.
func ProductACL(portal, capi string, seedVMID int, boot, iso, importStore string, others ...string) map[string]map[string][]string {
	read := []string{"Datastore.AllocateSpace", "Datastore.AllocateTemplate", "Datastore.Audit"}
	admin := append([]string{"Datastore.Allocate"}, read...)
	acl := map[string]map[string][]string{
		portal: {"/storage/" + boot: read, "/storage/" + iso: read},
		capi: {"/storage/" + boot: {"Datastore.AllocateSpace", "Datastore.Audit"}, "/storage/" + iso: admin,
			fmt.Sprintf("/vms/%d", seedVMID): {"VM.Audit", "VM.Allocate", "VM.Clone", "VM.PowerMgmt"}},
	}
	for _, s := range others {
		acl[portal]["/storage/"+s] = read
	}
	if importStore != "" {
		acl[portal]["/storage/"+importStore] = admin
	}
	return acl
}
