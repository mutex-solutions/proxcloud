package pve

// bootiso.go — where a worker's bootstrap ISO lands. CAPMOX writes the node's user-data, kubeadm join token
// included, to `user-data-<vmid>.iso` and mounts it as a CD until the node has joined, then deletes it. CAPMOX
// v0.9.2 calls go-proxmox v0.8.2's CloudInit without a storage, so the ISO goes to the first enabled iso store
// in GET /nodes/{node}/storage as its token sees it, and PVE lists stores in Perl hash order (`keys %ids` in
// PVE::Storage::storage_ids). The token must therefore see exactly one iso store, and a node-local one: anyone
// who can mount a shared store reads the join tokens of nodes still booting.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DefaultBootISOStorage is PVE's stock directory store, on every node with `iso` content.
const DefaultBootISOStorage = "local"

// cloudInitISOPrefix is the name go-proxmox gives a bootstrap ISO.
const cloudInitISOPrefix = "user-data-"

// ErrBootISOGrant: capmox@pve itself lacks PVEDatastoreAdmin on the store. A privsep token gets the user's rights
// intersected with its own, so a cluster token there could neither upload nor delete its ISO.
var ErrBootISOGrant = errors.New("capmox@pve lacks PVEDatastoreAdmin on the bootstrap ISO store")

// BootISOProblem says why iso cannot hold the bootstrap ISOs of workers booting from boot, or "".
func BootISOProblem(caps map[string]StorageCapability, iso, boot string) string {
	c, ok := caps[iso]
	switch {
	case !ok:
		return fmt.Sprintf("storage %q does not exist", iso)
	case !c.HasContent("iso"):
		return fmt.Sprintf("storage %q does not take ISO images", iso)
	case c.Shared:
		return fmt.Sprintf("storage %q is shared, and whoever reaches it reads the join tokens of booting nodes: use a node-local store such as %q", iso, DefaultBootISOStorage)
	}
	if b, ok := caps[boot]; ok && boot != iso && b.HasContent("iso") {
		return fmt.Sprintf("boot storage %q also takes ISO images, so CAPMOX may put the bootstrap ISO on either", boot)
	}
	return ""
}

// ACLEntry is one row of GET /access/acl.
type ACLEntry struct {
	Path      string `json:"path"`
	RoleID    string `json:"roleid"`
	Type      string `json:"type"` // user | group | token
	UGID      string `json:"ugid"`
	Propagate int    `json:"propagate"`
}

// ListACL lists the ACL entries the token may modify.
func (c *Client) ListACL(ctx context.Context) ([]ACLEntry, error) {
	var out []ACLEntry
	if err := c.get(ctx, "/access/acl", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func hasBootISOGrant(acls []ACLEntry, store string) bool {
	for _, a := range acls {
		if a.Type == "user" && a.UGID == capiUser && a.RoleID == roleISOStorage && a.Path == "/storage/"+store {
			return true
		}
	}
	return false
}

// CheckBootISOGrant fails with ErrBootISOGrant when capmox@pve holds no PVEDatastoreAdmin on store.
func (c *Client) CheckBootISOGrant(ctx context.Context, store string) error {
	acls, err := c.ListACL(ctx)
	if err != nil {
		return err
	}
	if !hasBootISOGrant(acls, store) {
		return fmt.Errorf("%w %q (pveum acl modify /storage/%s --users %s --roles %s --propagate 0)", ErrBootISOGrant, store, store, capiUser, roleISOStorage)
	}
	return nil
}

// PinClusterISO moves a cluster's ISO grant onto store, for clusters minted with another one. It waits, returning
// the ISO's volid, while any bootstrap ISO is still on an old store: CAPMOX deletes one after the join and only on a
// store its token still sees, so revoking first would leave that node's join token behind.
func (c *Client) PinClusterISO(ctx context.Context, clusterID, store string) (waiting string, err error) {
	acls, err := c.ListACL(ctx)
	if err != nil {
		return "", err
	}
	token := capiUser + "!" + clusterID
	have := false
	var old []string
	for _, a := range acls {
		st, ok := strings.CutPrefix(a.Path, "/storage/")
		if !ok || a.Type != "token" || a.UGID != token || a.RoleID != roleISOStorage {
			continue
		}
		if st == store {
			have = true
		} else {
			old = append(old, st)
		}
	}
	if len(old) == 0 {
		return "", nil // pinned already, or not a token the portal minted
	}
	if !hasBootISOGrant(acls, store) {
		return "", fmt.Errorf("%w %q", ErrBootISOGrant, store)
	}
	for _, st := range old {
		// ponytail: any cluster's ISO on the old store holds every move; per-pool matching if that ever stalls one.
		if waiting, err = c.firstBootISO(ctx, st); err != nil || waiting != "" {
			return waiting, err
		}
	}
	if !have {
		if err := c.SetACL(ctx, "/storage/"+store, roleISOStorage, []string{token}, false); err != nil {
			return "", err
		}
	}
	for _, st := range old {
		if err := c.SetACL(ctx, "/storage/"+st, roleISOStorage, []string{token}, true); err != nil {
			return "", err
		}
	}
	return "", nil
}

// firstBootISO names a bootstrap ISO on store, asking every online node (a local store has one copy per node).
func (c *Client) firstBootISO(ctx context.Context, store string) (string, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		items, err := c.StorageContents(ctx, n.Node, store, "iso", 0)
		if err != nil {
			if IsNotFound(err) || isStorageAbsent(err) {
				continue
			}
			return "", err
		}
		for _, it := range items {
			if _, file, _ := strings.Cut(it.VolID, "iso/"); strings.HasPrefix(file, cloudInitISOPrefix) {
				return it.VolID, nil
			}
		}
	}
	return "", nil
}

// isStorageAbsent: a store gone, or restricted to other nodes, answers "storage '<id>' does not exist" or "is not
// available on node".
func isStorageAbsent(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	msg := ae.Error()
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "not available on node")
}
