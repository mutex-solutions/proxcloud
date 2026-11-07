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
	"slices"
	"strings"
)

// DefaultBootISOStorage is PVE's stock directory store, on every node with `iso` content.
const DefaultBootISOStorage = "local"

// cloudInitISOPrefix is the name go-proxmox gives a bootstrap ISO.
const cloudInitISOPrefix = "user-data-"

// ErrBootISOGrant: capmox@pve itself lacks the ISO rights on the store (PVEDatastoreAdmin gives them). A privsep token
// gets the user's rights intersected with its own, so a cluster token there could neither upload nor delete its ISO.
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

// bootISOPrivs is what CAPMOX does with its ISO: upload it, list it, delete it after the join.
var bootISOPrivs = []string{"Datastore.AllocateTemplate", "Datastore.Audit", "Datastore.Allocate"}

// effectiveRoles is PVE::AccessControl::roles for a user on path, read off the ACL list: walking down from /, the
// user's own entries on a level replace what came before, else its groups' entries do, and an entry above path counts
// only with propagate. The portal token may not ask PVE instead: GET /access/permissions for another user wants
// Sys.Audit on /access.
func effectiveRoles(acls []ACLEntry, user string, groups []string, path string) []string {
	levels := []string{"/"}
	for i := 1; i < len(path); i++ {
		if path[i] == '/' {
			levels = append(levels, path[:i])
		}
	}
	if path != "/" {
		levels = append(levels, path)
	}
	var roles []string
	for _, lvl := range levels {
		pick := func(match func(ACLEntry) bool) []string {
			var out []string
			for _, a := range acls {
				if a.Path == lvl && match(a) && (lvl == path || a.Propagate == 1) {
					out = append(out, a.RoleID)
				}
			}
			return out
		}
		if r := pick(func(a ACLEntry) bool { return a.Type == "user" && a.UGID == user }); r != nil {
			roles = r
		} else if r := pick(func(a ACLEntry) bool { return a.Type == "group" && slices.Contains(groups, a.UGID) }); r != nil {
			roles = r
		}
	}
	return roles
}

// missingBootISOPrivs lists what capmox@pve itself lacks of bootISOPrivs on store, given the ACL list.
func (c *Client) missingBootISOPrivs(ctx context.Context, acls []ACLEntry, store string) ([]string, error) {
	var u struct {
		Groups []string `json:"groups"`
	}
	if err := c.get(ctx, "/access/users/"+capiUser, nil, &u); err != nil {
		return nil, err
	}
	var defs []struct {
		RoleID string `json:"roleid"`
		Privs  string `json:"privs"`
	}
	if err := c.get(ctx, "/access/roles", nil, &defs); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range effectiveRoles(acls, capiUser, u.Groups, "/storage/"+store) {
		for _, d := range defs {
			if d.RoleID == r {
				for _, p := range strings.Split(d.Privs, ",") {
					have[p] = true
				}
			}
		}
	}
	var miss []string
	for _, p := range bootISOPrivs {
		if !have[p] {
			miss = append(miss, p)
		}
	}
	return miss, nil
}

// bootISOGrantErr is ErrBootISOGrant with what is missing, or nil.
func (c *Client) bootISOGrantErr(ctx context.Context, acls []ACLEntry, store string) error {
	miss, err := c.missingBootISOPrivs(ctx, acls, store)
	if err != nil || len(miss) == 0 {
		return err
	}
	return fmt.Errorf("%w %q, missing %s (pveum acl modify /storage/%s --users %s --roles %s --propagate 0)",
		ErrBootISOGrant, store, strings.Join(miss, ", "), store, capiUser, roleISOStorage)
}

// CheckBootISOGrant fails with ErrBootISOGrant when capmox@pve lacks what CAPMOX needs on store.
func (c *Client) CheckBootISOGrant(ctx context.Context, store string) error {
	acls, err := c.ListACL(ctx)
	if err != nil {
		return err
	}
	return c.bootISOGrantErr(ctx, acls, store)
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
	if err := c.bootISOGrantErr(ctx, acls, store); err != nil {
		return "", err
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
	// A node may have started booting between the look above and the revoke, its ISO on an old store: give that
	// store back, so CAPMOX can still delete the file after the join, and move on a later pass.
	// ponytail: an upload PVE admitted before the revoke and wrote after this look still slips by; catching it would
	// need the old store remembered for another pass.
	for _, st := range old {
		if waiting, err = c.firstBootISO(ctx, st); err != nil || waiting != "" {
			if gerr := c.SetACL(ctx, "/storage/"+st, roleISOStorage, []string{token}, false); gerr != nil {
				err = errors.Join(err, gerr)
			}
			return waiting, err
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
