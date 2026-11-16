package pve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
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
	if err != nil && (IsNotFound(err) || isTokenAbsent(err)) {
		return nil
	}
	return err
}

// isTokenAbsent: PVE answers a missing token or user with a 500 naming it, not a 404. Only that answer: "no such host" from
// the dialer or a 401 for our own token means the token may still be live, and a release that takes it for revoked
// never comes back to it.
func isTokenAbsent(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusInternalServerError {
		return false
	}
	msg := ae.Error()
	return strings.Contains(msg, "no such token") || strings.Contains(msg, "no such user")
}

// SetACL grants (or with del revokes) a role on a path for users/tokens.
// path e.g. "/pool/tp-abc123", roles e.g. "PVEVMAdmin".
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
	BootStorage        string // where CAPMOX clones the workers' boot disks
	ISOStorage         string // where CAPMOX uploads the bootstrap ISO
	Zone, Vnet         string
}

// ClusterTokens are the tokens of one cluster. CAPMOX stays on the management side. No CSI token is minted: tenant
// block volumes go through the CSI gateway's own token. Clusters from before it may still hold csi@pve!<id>, which
// RevokeClusterTokens deletes too.
type ClusterTokens struct{ CAPIID, CAPISecret string }

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
	return errors.Join(c.DeleteAPIToken(ctx, capiUser, clusterID), c.RevokeClusterCSIToken(ctx, clusterID))
}

// RevokeClusterCSIToken deletes the cluster's csi@pve!<id> token alone: a cluster moved onto the CSI gateway keeps its
// CAPI token. A token already gone counts as revoked.
func (c *Client) RevokeClusterCSIToken(ctx context.Context, clusterID string) error {
	return c.DeleteAPIToken(ctx, csiUser, clusterID)
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
	{Path: "/", Privs: []string{"Sys.Audit", "Sys.Console", "Sys.Modify"}},
	// node firewall options (nftables preflight) and node network list
	// (management-bridge guard) check /nodes/{node}: PVEAuditor on /nodes.
	{Path: "/nodes", Privs: []string{"Sys.Audit"}},
	{Path: "/pool", Privs: []string{
		"Pool.Allocate", "Pool.Audit",
		"VM.Allocate", "VM.Audit", "VM.Clone",
		"VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk",
		"VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options",
		"VM.Backup", "VM.Console", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted", "VM.Migrate", "VM.PowerMgmt",
		"VM.Replicate", "VM.Snapshot", "VM.Snapshot.Rollback",
	}},
	{Path: "/sdn", Privs: []string{"SDN.Allocate", "SDN.Audit", "SDN.Use"}},
	// Read apart from /sdn: an ACL on /sdn/zones replaces what /sdn passes down, and zones and NICs check here.
	{Path: "/sdn/zones", Privs: []string{"SDN.Allocate", "SDN.Audit", "SDN.Use"}},
	{Path: "/storage", Privs: []string{"Datastore.AllocateSpace", "Datastore.AllocateTemplate", "Datastore.Audit"}},
	{Path: "/mapping/pci", Privs: []string{"Mapping.Audit", "Mapping.Use"}},
}

// The portal token's role and the pool it must not reach, as docs/proxmox-permissions.md names them.
const (
	PlatformRole = "ProxCloudPlatform"
	SystemPool   = "proxcloud-system"
)

// PlatformACL is one ACL of the portal token: Role on Path, granted to its user and to the token itself (a
// privilege-separated token holds the intersection of the two).
type PlatformACL struct {
	Path, Role string
	Propagate  bool
	GPUOnly    bool // only a cluster with GPUs needs it
}

// PlatformACLs are the ACLs that give the portal token PlatformPrivileges and nothing on the management VMs. It is
// what PlatformSetupScript writes; installer/playbooks/10-pve-identity.yml and docs/proxmox-permissions.md list the
// same rows.
var PlatformACLs = []PlatformACL{
	// Sys.* only, and not propagated: they must not reach /nodes.
	{Path: "/", Role: PlatformRole},
	{Path: "/nodes", Role: "PVEAuditor", Propagate: true},
	{Path: "/pool", Role: PlatformRole, Propagate: true},
	// The more specific path wins, and NoAccess cancels every other role.
	{Path: "/pool/" + SystemPool, Role: "NoAccess", Propagate: true},
	{Path: "/sdn", Role: PlatformRole, Propagate: true},
	{Path: "/storage", Role: PlatformRole, Propagate: true},
	{Path: "/mapping/pci", Role: PlatformRole, Propagate: true, GPUOnly: true},
}

// Managed services (docs/proxmox-permissions.md "Managed services"): the platform's own CAPMOX and proxmox-csi
// tokens, the pool of the seed VM, and the portal token's right to mint the per-cluster tokens of tenant clusters.
const (
	SeedPool     = "proxcloud-seed" // the seed VM alone; the portal and the CSI tokens get NoAccess there
	KubeMintRole = "ProxCloudKubeMint"
	// KubeTokenRole is User.Modify alone, granted on /access/groups/KubeMintGroup and nowhere else. PVE lets the
	// holder of User.Modify on /access/groups create a token for every user of the cluster, root@pam included
	// (PVE::RPCEnvironment, "userid-group"); on a group's path, for the members of that group only.
	KubeTokenRole = "ProxCloudKubeToken"
	KubeMintGroup = "proxcloud-kube"
	CAPITokenID   = capiUser + "!capi"
	CSITokenID    = csiUser + "!csi"
)

// vmConfig is stage 10's {{ vm_config }}.
var vmConfig = []string{"VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk", "VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options"}

// ManagedRole is a role of the managed-services identities and its privileges.
type ManagedRole struct {
	Name  string
	Privs []string
}

// ManagedRoles are the roles ManagedSetupScript creates, with the privileges installer stage 10 gives them.
var ManagedRoles = []ManagedRole{
	{roleCAPI, slices.Concat([]string{"Sys.Audit", "Pool.Audit", "VM.Allocate", "VM.Audit", "VM.Clone"}, vmConfig,
		[]string{"VM.GuestAgent.Audit", "VM.Migrate", "VM.PowerMgmt", "Datastore.AllocateSpace", "Datastore.Audit", "SDN.Use"})},
	{roleCSI, []string{"VM.Audit", "VM.Config.Disk", "Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.Audit"}},
	{KubeMintRole, []string{"Permissions.Modify"}},
	{KubeTokenRole, []string{"User.Modify"}},
}

// KubeMintGroupMembers are the users of KubeMintGroup, the only ones the portal token may create and delete tokens
// for: capmox@pve for the tenant clusters' tokens, csi@pve for the csi@pve!<id> tokens of clusters from before the CSI
// gateway, which a release or a move still deletes. Never the portal's own user, nor any user that is not the product's.
var KubeMintGroupMembers = []string{capiUser, csiUser}

// ManagedACL is one ACL row of the managed services, in stage 10's shape: Role on Path for User and for Token, ""
// for no row of that one. A privilege-separated token holds the intersection of its user's rights and its own.
type ManagedACL struct {
	Path, Role  string
	Propagate   bool
	User, Token string
}

// ManagedACLs are the rows ManagedSetupScript writes for the portal token portalTokenID, with boot the store of the
// platform VMs' disks (and of proxmox-csi's volumes), iso the node-local store of their bootstrap ISOs and importStorage
// PlatformSetupScript's (none when ""). Installer stage 10 writes the same rows in split_acls and nas_mint_acls,
// without the SeedPool ones and with two for the CSI gateway's token, which has no listener on Compose (a test keeps
// the two equal).
func ManagedACLs(portalTokenID, boot, iso, importStorage string) []ManagedACL {
	portal, _, _ := strings.Cut(portalTokenID, "!")
	acls := []ManagedACL{
		// The platform's CAPMOX: the platform pools only. The user keeps /pool, /storage and /sdn/zones for the
		// tenant clusters' tokens the portal mints under it, and its own rows on the platform pools win over any
		// grant on /pool written for it later (the mint grant below can write one).
		{"/", roleCAPI, false, capiUser, CAPITokenID},
		{"/nodes", roleCAPI, true, capiUser, CAPITokenID},
		{"/pool", roleCAPI, true, capiUser, ""},
		{"/pool/" + SystemPool, roleCAPI, true, capiUser, CAPITokenID},
		{"/pool/" + SeedPool, roleCAPI, true, capiUser, CAPITokenID},
		{"/pool/" + TemplatePool, roleTemplate, true, capiUser, CAPITokenID},
		{"/sdn/zones", roleCAPI, true, capiUser, CAPITokenID},
		// CAPMOX puts each bootstrap ISO on the first iso store its token sees (bootiso.go): it sees boot and iso alone.
		{"/storage", roleCAPI, true, capiUser, ""},
		{"/storage/" + boot, roleCAPI, false, "", CAPITokenID},
		{"/storage/" + iso, roleISOStorage, false, capiUser, CAPITokenID},
		// The platform's proxmox-csi: disks of the workload VMs on boot. NoAccess on the user caps every csi@pve
		// token off the seed VM, the CSI gateway's included. Not off its disk on boot: Datastore.Allocate there
		// opens every volume on the store, whatever VM owns it (docs/proxmox-permissions.md).
		{"/pool", roleCSI, true, csiUser, ""},
		{"/pool/" + SystemPool, roleCSI, true, csiUser, CSITokenID},
		{"/pool/" + SeedPool, "NoAccess", true, csiUser, ""},
		{"/storage", roleCSI, true, csiUser, ""},
		{"/storage/" + boot, roleCSI, false, "", CSITokenID},
		// The portal mints and grants the tenant clusters' tokens: tokens for the members of KubeMintGroup alone
		// (never a row on /access/groups itself: KubeTokenRole), ACL writes on the paths below. An ACL on a path
		// replaces what its parent passes down, so /sdn/zones repeats the platform role.
		{"/access/groups/" + KubeMintGroup, KubeTokenRole, false, portal, portalTokenID},
		{"/pool", KubeMintRole, true, portal, portalTokenID},
		{"/storage", KubeMintRole, true, portal, portalTokenID},
		{"/sdn/zones", KubeMintRole, true, portal, portalTokenID},
		{"/sdn/zones", PlatformRole, true, portal, portalTokenID},
		{"/nodes", KubeMintRole, false, portal, portalTokenID},
		{"/pool/" + SeedPool, "NoAccess", true, portal, portalTokenID},
	}
	if importStorage != "" {
		// The PVEDatastoreAdmin row there replaces what /storage passes down, and a tenant cluster's ISO or boot store
		// may be that store: without the mint role the portal could not write the cluster token's row on it.
		acls = append(acls, ManagedACL{"/storage/" + importStorage, KubeMintRole, false, portal, portalTokenID})
	}
	return acls
}

// CAPIPrivileges is what CAPITokenID must hold, per ACL path, for the platform's CAPMOX and for the job that builds
// the seed (MissingPrivileges with a client on that token). boot and iso as for ManagedACLs.
func CAPIPrivileges(boot, iso string) []PrivCheck {
	return []PrivCheck{
		{Path: "/", Privs: []string{"Sys.Audit"}},
		{Path: "/nodes", Privs: []string{"Sys.Audit"}},
		{Path: "/pool/" + SystemPool, Privs: ManagedRoles[0].Privs},
		{Path: "/pool/" + SeedPool, Privs: ManagedRoles[0].Privs},
		{Path: "/pool/" + TemplatePool, Privs: []string{"VM.Audit", "VM.Clone"}},
		{Path: "/sdn/zones", Privs: []string{"SDN.Use"}},
		{Path: "/storage/" + boot, Privs: []string{"Datastore.AllocateSpace", "Datastore.Audit"}},
		{Path: "/storage/" + iso, Privs: bootISOPrivs},
	}
}

// CSIPrivileges is what CSITokenID must hold, per ACL path, for the platform's proxmox-csi on boot.
func CSIPrivileges(boot string) []PrivCheck {
	return []PrivCheck{
		{Path: "/pool/" + SystemPool, Privs: []string{"VM.Audit", "VM.Config.Disk"}},
		{Path: "/storage/" + boot, Privs: []string{"Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.Audit"}},
	}
}

// KubeMintPrivileges is what the portal token must hold, per ACL path, to mint the tenant clusters' tokens
// (MintClusterTokens): a token create wants User.Modify on a group of the token's user, an ACL write
// Permissions.Modify on its path. importStorage as for ManagedACLs: its own row shadows /storage, so it is checked
// apart.
func KubeMintPrivileges(importStorage string) []PrivCheck {
	checks := []PrivCheck{
		{Path: "/access/groups/" + KubeMintGroup, Privs: ManagedRoles[3].Privs},
		{Path: "/pool", Privs: ManagedRoles[2].Privs},
		{Path: "/storage", Privs: ManagedRoles[2].Privs},
		{Path: "/sdn/zones", Privs: ManagedRoles[2].Privs},
		{Path: "/nodes", Privs: ManagedRoles[2].Privs},
	}
	if importStorage != "" {
		checks = append(checks, PrivCheck{Path: "/storage/" + importStorage, Privs: ManagedRoles[2].Privs})
	}
	return checks
}

// KubeMintForbidden is what the portal token must hold nowhere, per ACL path (HeldPrivileges): User.Modify on
// /access/groups reaches every user of the cluster, and Permissions.Modify on any of the three lets the token write
// itself that row. Sites set up before KubeMintGroup hold ProxCloudKubeMint on /access/groups.
var KubeMintForbidden = []PrivCheck{
	{Path: "/", Privs: []string{"User.Modify", "Permissions.Modify"}},
	{Path: "/access", Privs: []string{"User.Modify", "Permissions.Modify"}},
	{Path: "/access/groups", Privs: []string{"User.Modify", "Permissions.Modify"}},
}

// KubeMintNarrowing is what to tell the operator of a site where HeldPrivileges(KubeMintForbidden) names something.
const KubeMintNarrowing = "with it a stolen portal token can create an API token for any user of the cluster, root@pam included. " +
	"Run the managed-services script again as root on a node (GET /admin/setup/proxmox-script?managedServices=true; an installer site: make upgrade, stage 10): " +
	"it grants " + KubeTokenRole + " on /access/groups/" + KubeMintGroup + " instead and removes the " + KubeMintRole + " rows on /access/groups. " +
	"If this still fails afterwards, another row grants it: pveum acl list | grep -F '<the portal user>', then pveum acl delete <path> --users|--tokens '<id>' --roles <role> for a row on /, /access or /access/groups (docs/proxmox-permissions.md)"

// PlatformRolePrivileges is the privilege list of PlatformRole: everything PlatformPrivileges asks for, once.
func PlatformRolePrivileges() []string {
	var out []string
	for _, chk := range PlatformPrivileges {
		for _, p := range chk.Privs {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
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
	return c.privileges(ctx, checks, false)
}

// HeldPrivileges is the opposite: one line per path where the token holds any of the privileges named.
func (c *Client) HeldPrivileges(ctx context.Context, checks []PrivCheck) ([]string, error) {
	return c.privileges(ctx, checks, true)
}

func (c *Client) privileges(ctx context.Context, checks []PrivCheck, held bool) ([]string, error) {
	var out []string
	for _, chk := range checks {
		have, err := c.Permissions(ctx, chk.Path)
		if err != nil {
			return nil, err
		}
		var miss []string
		for _, p := range chk.Privs {
			if have[p] == held {
				miss = append(miss, p)
			}
		}
		if len(miss) > 0 {
			out = append(out, strings.Join(miss, ", ")+" on "+chk.Path)
		}
	}
	return out, nil
}
