package pve

import (
	"context"
	"net/url"
	"strings"
)

// Access management — used by Phase K to mint narrow per-cluster tokens
// (proxcloud-capi@pve!<clusterID>) and by teardown to revoke them.

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
	if err != nil && IsNotFound(err) {
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
	{Path: "/", Privs: []string{"Sys.Audit", "Sys.Modify"}},
	// node firewall options (nftables preflight) and node network list
	// (management-bridge guard) check /nodes/{node}: PVEAuditor on /nodes.
	{Path: "/nodes", Privs: []string{"Sys.Audit"}},
	{Path: "/pool", Privs: []string{
		"Pool.Allocate", "Pool.Audit",
		"VM.Allocate", "VM.Audit", "VM.Clone",
		"VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk",
		"VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options",
		"VM.Console", "VM.GuestAgent.Audit", "VM.Migrate", "VM.PowerMgmt",
		"VM.Snapshot", "VM.Snapshot.Rollback",
	}},
	{Path: "/sdn", Privs: []string{"SDN.Allocate", "SDN.Audit", "SDN.Use"}},
	{Path: "/storage", Privs: []string{"Datastore.AllocateSpace", "Datastore.Audit"}},
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
