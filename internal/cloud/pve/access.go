package pve

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Access management: the narrow per-cluster tokens of managed Kubernetes, and their revocation.

// CreateUser creates a PVE-realm user (idempotent: "already exists" is not an error).
func (c *Client) CreateUser(ctx context.Context, userid, comment string) error {
	form := formVals{"userid": userid, "comment": comment}.values()
	err := c.post(ctx, "/access/users", form, nil)
	if err != nil {
		if ae, ok := err.(*APIError); ok && strings.Contains(strings.ToLower(ae.Error()), "already exists") {
			return nil
		}
	}
	return err
}

// CreateAPIToken mints a token for a user. Returns the full token id + secret —
// the secret is shown exactly once by PVE, so the caller must persist it.
// privsep=true keeps the token's rights bounded by its own ACLs, not the user's.
func (c *Client) CreateAPIToken(ctx context.Context, userid, tokenName string) (tokenID, secret string, err error) {
	form := formVals{"privsep": "1"}.values()
	var out struct {
		FullTokenID string `json:"full-tokenid"`
		Value       string `json:"value"`
	}
	path := "/access/users/" + url.PathEscape(userid) + "/token/" + url.PathEscape(tokenName)
	if err := c.post(ctx, path, form, &out); err != nil {
		return "", "", err
	}
	return out.FullTokenID, out.Value, nil
}

// DeleteAPIToken revokes a token. Absent token is not an error (idempotent revoke).
func (c *Client) DeleteAPIToken(ctx context.Context, userid, tokenName string) error {
	path := "/access/users/" + url.PathEscape(userid) + "/token/" + url.PathEscape(tokenName)
	err := c.delete(ctx, path, nil, nil)
	// PVE answers a missing token or user with a 500 "no such ...", not a 404.
	if err != nil && (IsNotFound(err) || strings.Contains(err.Error(), "no such ")) {
		return nil
	}
	return err
}

// SetACL grants (or with del revokes) a role on a path for users/tokens.
// path e.g. "/pool/st-abc123", roles e.g. "PVEVMAdmin".
func (c *Client) SetACL(ctx context.Context, path, roles string, tokens []string, del bool) error {
	form := formVals{
		"path":   path,
		"roles":  roles,
		"tokens": strings.Join(tokens, ","),
		"delete": pbool(del),
	}.values()
	return c.put(ctx, "/access/acl", form, nil)
}

// Users and roles the per-cluster tokens hang off (docs/proxmox-permissions.md).
const (
	capiUser       = "capmox@pve"
	csiUser        = "csi@pve"
	roleCAPI       = "ProxCloudCAPI"
	roleCSI        = "ProxCloudCSI"
	roleTemplate   = "PVETemplateUser"
	roleISOStorage = "PVEDatastoreAdmin"
)

// ClusterTokenScope is what one tenant cluster's tokens may touch.
type ClusterTokenScope struct {
	Pool, TemplatePool string
	BootStorage        string   // where CAPMOX clones the workers' boot disks
	CSIStorages        []string // the stores a StorageClass offers; none = no CSI token
	ISOStorage         string   // where CAPMOX uploads the bootstrap ISO
	Zone, Vnet         string
}

// ClusterTokens are the tokens of one cluster. CAPMOX stays on the management side. A CSI token reaches the
// tenant cluster, and PVE checks only Datastore.Allocate on the store to attach or delete a disk, so it opens
// every disk on those stores, of any project: it is minted only for a provider that accepts that.
type ClusterTokens struct{ CAPIID, CAPISecret, CSIID, CSISecret string }

// MintClusterTokens creates the tokens and their ACLs. Revoking is RevokeClusterTokens; a failure
// half way revokes what was made, so a refused create leaves nothing behind.
func (c *Client) MintClusterTokens(ctx context.Context, clusterID string, sc ClusterTokenScope) (t ClusterTokens, err error) {
	defer func() {
		if err != nil {
			_ = c.RevokeClusterTokens(context.WithoutCancel(ctx), clusterID)
		}
	}()
	if t.CAPIID, t.CAPISecret, err = c.CreateAPIToken(ctx, capiUser, clusterID); err != nil {
		return t, err
	}
	grants := []struct{ path, role, token string }{
		{"/pool/" + sc.Pool, roleCAPI, t.CAPIID},
		{"/storage/" + sc.BootStorage, roleCAPI, t.CAPIID},
		{"/pool/" + sc.TemplatePool, roleTemplate, t.CAPIID},
		{"/nodes", roleCAPI, t.CAPIID},
		// A clone is checked against the NICs of the template, and a node template sits on a node bridge.
		{"/sdn/zones/localnetwork", roleCAPI, t.CAPIID},
		{"/storage/" + sc.ISOStorage, roleISOStorage, t.CAPIID},
	}
	if sc.Zone != "" {
		grants = append(grants, struct{ path, role, token string }{"/sdn/zones/" + sc.Zone + "/" + sc.Vnet, roleCAPI, t.CAPIID})
	}
	if len(sc.CSIStorages) > 0 {
		if t.CSIID, t.CSISecret, err = c.CreateAPIToken(ctx, csiUser, clusterID); err != nil {
			return t, err
		}
		grants = append(grants, struct{ path, role, token string }{"/pool/" + sc.Pool, roleCSI, t.CSIID})
		for _, st := range sc.CSIStorages {
			grants = append(grants, struct{ path, role, token string }{"/storage/" + st, roleCSI, t.CSIID})
		}
	}
	for _, g := range grants {
		if err = c.SetACL(ctx, g.path, g.role, []string{g.token}, false); err != nil {
			return t, err
		}
	}
	return t, nil
}

// RevokeClusterTokens deletes the tokens. PVE stops honouring a deleted token's ACL entries at once and drops
// them from user.cfg at the next write of that file; until then `pveum` warns "ignore invalid acl token".
func (c *Client) RevokeClusterTokens(ctx context.Context, clusterID string) error {
	return errors.Join(c.DeleteAPIToken(ctx, capiUser, clusterID), c.DeleteAPIToken(ctx, csiUser, clusterID))
}

// PrivCheck is one ACL path and the privileges the token must hold there.
type PrivCheck struct {
	Path  string
	Privs []string
}

// PlatformPrivileges is the ProxCloudPlatform role as the portal token needs it,
// per ACL path (docs/proxmox-permissions.md keeps the pveum commands in sync).
// /pool stands in for every tenant pool: pools are created at runtime and
// inherit from it. Names verified against the PVE 9 pveum docs and API schema.
// Lab-verified (PVE 9.2): pool inheritance, and the NoAccess override on
// /pool/proxcloud-system (VM.Audit and VM.PowerMgmt are refused on a VM in it).
var PlatformPrivileges = []PrivCheck{
	// cluster status, PCI scan, datacenter firewall options and the cluster
	// security groups all check "/" itself (granted with propagate=0).
	{Path: "/", Privs: []string{"Sys.AccessNetwork", "Sys.Audit", "Sys.Modify"}},
	// node firewall options (nftables preflight) and node network list
	// (management-bridge guard) check /nodes/{node}: PVEAuditor on /nodes.
	{Path: "/nodes", Privs: []string{"Sys.Audit"}},
	{Path: "/pool", Privs: []string{
		"Pool.Allocate", "Pool.Audit",
		"VM.Allocate", "VM.Audit", "VM.Clone",
		"VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk",
		"VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options",
		"VM.Backup", "VM.Console", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted", "VM.Migrate", "VM.PowerMgmt",
		"VM.Snapshot", "VM.Snapshot.Rollback",
	}},
	{Path: "/sdn", Privs: []string{"SDN.Allocate", "SDN.Audit", "SDN.Use"}},
	// Read apart from /sdn: an ACL on /sdn/zones replaces what /sdn passes down, and zones and NICs check here.
	{Path: "/sdn/zones", Privs: []string{"SDN.Allocate", "SDN.Audit", "SDN.Use"}},
	{Path: "/storage", Privs: []string{"Datastore.AllocateSpace", "Datastore.AllocateTemplate", "Datastore.Audit"}},
	{Path: "/mapping/pci", Privs: []string{"Mapping.Audit", "Mapping.Use"}},
}

// Permissions returns the calling token's effective privileges on path
// (GET /access/permissions?path=…; a token may always read its own).
// Lab-verified (PVE 9.2): a path with no ACL of its own reports the inherited privileges
// and a privsep token gets the user∩token set.
func (c *Client) Permissions(ctx context.Context, path string) (map[string]bool, error) {
	var out map[string]map[string]any // priv → propagate flag
	if err := c.get(ctx, "/access/permissions", formVals{"path": path}.values(), &out); err != nil {
		return nil, err
	}
	privs := map[string]bool{}
	for p := range out[path] {
		privs[p] = true
	}
	return privs, nil
}

// MissingPrivileges checks each path and returns one line per path that lacks
// privileges, e.g. "SDN.Allocate, SDN.Use on /sdn". Empty means all present.
func (c *Client) MissingPrivileges(ctx context.Context, checks []PrivCheck) ([]string, error) {
	var out []string
	for _, chk := range checks {
		have, err := c.Permissions(ctx, chk.Path)
		if err != nil {
			return nil, err
		}
		var miss []string
		for _, p := range chk.Privs {
			if !have[p] {
				miss = append(miss, p)
			}
		}
		if len(miss) > 0 {
			out = append(out, strings.Join(miss, ", ")+" on "+chk.Path)
		}
	}
	return out, nil
}
