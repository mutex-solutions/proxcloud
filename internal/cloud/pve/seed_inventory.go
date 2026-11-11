package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
)

// seedRead is deliberately GET-only. A credential's identity is never returned
// in reference evidence or a cleanup report.
func (c *Client) seedRead(ctx context.Context, p string, q url.Values, out any) error {
	var raw json.RawMessage
	if err := c.get(ctx, p, q, &raw); err != nil {
		return fmt.Errorf("seed inventory request failed (%s)", p)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("seed inventory returned no data (%s)", p)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("seed inventory returned invalid data (%s)", p)
	}
	return nil
}
func privilegeFlag(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == 1
	case bool:
		return x
	case int:
		return x == 1
	}
	return false
}
func (c *Client) seedPermission(ctx context.Context, p, priv string, propagate bool) error {
	var perms map[string]map[string]any
	if err := c.seedRead(ctx, "/access/permissions", url.Values{"path": {p}}, &perms); err != nil {
		return err
	}
	v, ok := perms[p][priv]
	if !ok || (propagate && !privilegeFlag(v)) {
		return fmt.Errorf("seed inventory needs %s on %s (including restrictive ACL paths)", priv, p)
	}
	return nil
}

// proveSeedVisibility uses /access Sys.Audit to obtain the FULL ACL tree. The
// all-effective-permissions API omits empty NoAccess shadows, so it is NOT a
// completeness proof. Every relevant ACL path is checked explicitly instead.
func (c *Client) proveSeedVisibility(ctx context.Context) (string, error) {
	for _, p := range []struct{ path, priv string }{{"/access", "Sys.Audit"}, {"/", "Sys.Audit"}, {"/nodes", "Sys.Audit"}, {"/vms", "VM.Audit"}, {"/pool", "VM.Audit"}, {"/storage", "Datastore.Audit"}} {
		if err := c.seedPermission(ctx, p.path, p.priv, true); err != nil {
			return "", err
		}
	}
	var acls []struct {
		Path      string `json:"path"`
		Type      string `json:"type"`
		UGID      string `json:"ugid"`
		Role      string `json:"roleid"`
		Propagate int    `json:"propagate"`
	}
	if err := c.seedRead(ctx, "/access/acl", nil, &acls); err != nil {
		return "", err
	}
	paths := map[string]bool{}
	for _, a := range acls {
		if a.Path == "" || a.Type == "" || a.Role == "" {
			return "", fmt.Errorf("seed inventory ACL tree is incomplete")
		}
		if strings.HasPrefix(a.Path, "/vms/") || strings.HasPrefix(a.Path, "/pool/") {
			paths[a.Path] = true
		}
		if strings.HasPrefix(a.Path, "/storage/") {
			paths[a.Path] = true
		}
	}
	for p := range paths {
		priv := "VM.Audit"
		if strings.HasPrefix(p, "/storage/") {
			priv = "Datastore.Audit"
		}
		if err := c.seedPermission(ctx, p, priv, true); err != nil {
			return "", err
		}
	}
	// A digest detects changes during a scan without exposing usernames/ACLs.
	rows := make([]string, 0, len(acls))
	for _, a := range acls {
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d", a.Path, a.Type, a.UGID, a.Role, a.Propagate))
	}
	slices.Sort(rows)
	h := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(h[:]), nil
}

// seedBacking hashes the complete physical storage definition, not just its
// friendly ID. New/unknown properties fail closed on a changed definition.
func (c *Client) seedBacking(ctx context.Context, node, store string) (string, error) {
	var def map[string]any
	if err := c.seedRead(ctx, "/storage/"+url.PathEscape(store), nil, &def); err != nil {
		return "", err
	}
	if len(def) == 0 || def["type"] == nil {
		return "", fmt.Errorf("seed storage identity is unavailable")
	}
	delete(def, "digest")
	delete(def, "storage")
	// Shared means the same ID is accessible on each node; local IDs name a
	// different file per node and are consequently bound to their original node.
	shared := privilegeFlag(def["shared"])
	if !shared {
		def["seedNode"] = node
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// SeedReferences returns only a veto, never VM configs or guest data. Every
// reader failure makes absence unproven. All matching basenames veto even on
// another store: conservative alias handling covers the same NFS export or
// CephFS subdirectory registered under multiple storage IDs, without DNS/SSRF.
func (c *Client) SeedReferences(ctx context.Context, owner *Client, a *SeedArtifact) error {
	store, name, _, _, ok := ownedSeedVolume(a.Volume)
	if !ok {
		return fmt.Errorf("invalid owned seed volume")
	}
	before, err := c.proveSeedVisibility(ctx)
	if err != nil {
		return err
	}
	cluster, err := c.seedClusterIdentity(ctx)
	if err != nil {
		return err
	}
	writerCluster, err := owner.seedClusterIdentity(ctx)
	if err != nil {
		return err
	}
	if cluster != a.Cluster || writerCluster != a.Cluster {
		return fmt.Errorf("seed cluster identity changed")
	}
	backing, err := c.seedBacking(ctx, a.Node, store)
	if err != nil {
		return err
	}
	if backing != a.Backing {
		return fmt.Errorf("seed storage definition changed")
	}
	var vms []ClusterResource
	if err := c.seedRead(ctx, "/cluster/resources", url.Values{"type": {"vm"}}, &vms); err != nil {
		return err
	}
	manifest := func(v []ClusterResource) string {
		ids := []string{}
		for _, r := range v {
			ids = append(ids, fmt.Sprintf("%s:%d:%s:%s", r.Type, r.VMID, r.Node, r.Lock))
		}
		slices.Sort(ids)
		return strings.Join(ids, "|")
	}
	start := manifest(vms)
	for _, vm := range vms {
		if vm.VMID <= 0 || vm.Node == "" || vm.Lock != "" {
			return fmt.Errorf("seed inventory includes an unknown or busy guest")
		}
		if vm.Type != "qemu" {
			return fmt.Errorf("seed inventory cannot prove references of guest type %s", vm.Type)
		}
		if err := c.seedPermission(ctx, "/vms/"+strconv.Itoa(vm.VMID), "VM.Audit", false); err != nil {
			return err
		}
		base := qemuPath(vm.Node, vm.VMID)
		for _, q := range []url.Values{{"current": {"1"}}, {"current": {"0"}}} {
			var cfg map[string]any
			if err := c.seedRead(ctx, base+"/config", q, &cfg); err != nil {
				return err
			}
			if len(cfg) == 0 {
				return fmt.Errorf("seed inventory has an empty guest config")
			}
			if lock, ok := cfg["lock"]; ok && lock != nil && lock != "" {
				return fmt.Errorf("seed inventory guest config is locked")
			}
			if seedMentions(cfg, name) {
				return fmt.Errorf("seed remains referenced by a guest configuration")
			}
		}
		var pending []map[string]any
		if err := c.seedRead(ctx, base+"/pending", nil, &pending); err != nil {
			return err
		}
		if seedMentions(pending, name) {
			return fmt.Errorf("seed remains referenced by pending guest configuration")
		}
		var snaps []struct {
			Name      string `json:"name"`
			SnapState string `json:"snapstate"`
			Lock      string `json:"lock"`
		}
		if err := c.seedRead(ctx, base+"/snapshot", nil, &snaps); err != nil {
			return err
		}
		for _, sn := range snaps {
			if sn.Lock != "" || sn.SnapState != "" {
				return fmt.Errorf("seed inventory snapshot is busy")
			}
			if sn.Name == "current" {
				continue
			}
			if sn.Name == "" {
				return fmt.Errorf("seed inventory has an unnamed snapshot")
			}
			cfg, err := c.SnapshotConfig(ctx, vm.Node, vm.VMID, sn.Name)
			if err != nil {
				return fmt.Errorf("seed inventory could not read a snapshot config")
			}
			if len(cfg) == 0 {
				return fmt.Errorf("seed inventory has an empty snapshot config")
			}
			if cfg.Str("lock") != "" || cfg.Str("snapstate") != "" {
				return fmt.Errorf("seed inventory snapshot config is busy")
			}
			if seedMentions(map[string]any(cfg), name) {
				return fmt.Errorf("seed remains referenced by a guest snapshot")
			}
		}
	}
	var end []ClusterResource
	if err := c.seedRead(ctx, "/cluster/resources", url.Values{"type": {"vm"}}, &end); err != nil {
		return err
	}
	if manifest(end) != start {
		return fmt.Errorf("guest inventory changed during seed reference scan")
	}
	after, err := c.proveSeedVisibility(ctx)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("ACL tree changed during seed reference scan")
	}
	return nil
}
func seedMentions(v any, name string) bool {
	switch x := v.(type) {
	case string:
		// Scan all fields (including unused disks, custom paths and snapshots),
		// not the limited slot list used to locate our normal cloud-init drive.
		for _, part := range strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '=' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
			if path.Base(part) == name || strings.Contains(part, "/"+name) {
				return true
			}
		}
	case map[string]any:
		for _, it := range x {
			if seedMentions(it, name) {
				return true
			}
		}
	case []any:
		for _, it := range x {
			if seedMentions(it, name) {
				return true
			}
		}
	case []map[string]any:
		for _, it := range x {
			if seedMentions(it, name) {
				return true
			}
		}
	}
	return false
}

// seedClusterIdentity pins the quorate cluster membership, not just a mutable
// URL. Replacing/reforming the cluster or adding nodes deliberately requires
// operator reconciliation before an old artifact may be erased.
func (c *Client) seedClusterIdentity(ctx context.Context) (string, error) {
	var rows []map[string]any
	if err := c.seedRead(ctx, "/cluster/status", nil, &rows); err != nil {
		return "", err
	}
	identity := []string{}
	cluster := false
	for _, r := range rows {
		switch r["type"] {
		case "cluster":
			if !privilegeFlag(r["quorate"]) || r["name"] == nil {
				return "", fmt.Errorf("seed cluster is not proven quorate")
			}
			cluster = true
			identity = append(identity, fmt.Sprint(r["name"]))
		case "node":
			if r["name"] == nil || r["ip"] == nil || r["nodeid"] == nil {
				return "", fmt.Errorf("seed cluster membership identity is incomplete")
			}
			identity = append(identity, fmt.Sprintf("%v:%v:%v", r["name"], r["ip"], r["nodeid"]))
		}
	}
	if !cluster || len(identity) < 2 {
		return "", fmt.Errorf("seed cleanup requires a named quorate cluster")
	}
	slices.Sort(identity)
	h := sha256.Sum256([]byte(strings.Join(identity, "|")))
	return hex.EncodeToString(h[:]), nil
}

// SeedVMExists asks the same complete reader whether a server ID still exists.
// A missing cache row cannot authorize revoking a live seed or its rebuild data.
func (c *Client) SeedVMExists(ctx context.Context, id string) (bool, error) {
	if _, err := c.proveSeedVisibility(ctx); err != nil {
		return false, err
	}
	var rows []ClusterResource
	if err := c.seedRead(ctx, "/cluster/resources", url.Values{"type": {"vm"}}, &rows); err != nil {
		return false, err
	}
	for _, r := range rows {
		if strconv.Itoa(r.VMID) == id {
			return true, nil
		}
	}
	return false, nil
}
