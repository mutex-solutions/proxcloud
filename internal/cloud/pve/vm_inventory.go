package pve

// VM inventory is a proof for ONE read/reconcile, using the provider's configured
// credential only. Pool GET and cluster/resources both filter invisible VMs.
// Positive members suffice for ordinary refreshes; omissions need a complete
// namespace/ACL proof, never a grace period, root reader or Pool.Audit alone.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

const vmUUIDKey = "pveUUID"

type vmInventory struct {
	svc            *Service
	project        string
	page           []ClusterResource
	pool           *PoolInfo
	configs        map[int]VMConfig
	nodes          []string
	online         map[string]bool
	verified       bool
	addressUnknown map[int]bool
}

func vmInventoryError(typ, id, reason string, cause error) error {
	return &cloud.InventoryUntrustedError{Type: typ, ExternalID: id, Reason: reason, Err: cause}
}

// inventoryRead rejects a missing/null data envelope as well as wrong JSON types.
func (c *Client) inventoryRead(ctx context.Context, path string, q url.Values, out any) error {
	var raw json.RawMessage
	if err := c.get(ctx, path, q, &raw); err != nil {
		return err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("inventory data is missing")
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) inventoryVMPage(ctx context.Context, pool string) ([]ClusterResource, error) {
	var entries []json.RawMessage
	if err := c.inventoryRead(ctx, "/cluster/resources", url.Values{"type": {"vm"}}, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, errors.New("VM inventory is not an array")
	}
	page := make([]ClusterResource, 0, len(entries))
	seen := map[int]bool{}
	for _, entry := range entries {
		var r ClusterResource
		if err := json.Unmarshal(entry, &r); err != nil {
			return nil, err
		}
		if r.Pool == pool && r.Type == "qemu" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(entry, &fields); err != nil {
				return nil, err
			}
			if raw, present := fields["template"]; present {
				var flag int
				if string(raw) == "null" || json.Unmarshal(raw, &flag) != nil || (flag != 0 && flag != 1) {
					return nil, errors.New("template classification is malformed")
				}
			}
			if r.Status != "running" && r.Status != "stopped" {
				return nil, errors.New("project VM status unknown")
			}
			for _, k := range []string{"maxcpu", "maxmem", "maxdisk"} {
				var n float64
				raw, ok := fields[k]
				if !ok || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 || (k != "maxdisk" && n == 0) {
					return nil, errors.New("project VM dimensions incomplete")
				}
			}
		}
		page = append(page, r)
		if (r.Type != "qemu" && r.Type != "lxc") || r.VMID <= 0 || seen[r.VMID] || r.ID != r.Type+"/"+strconv.Itoa(r.VMID) || r.Node == "" || strings.ContainsAny(r.Node, "/\\\x00") {
			return nil, errors.New("invalid or duplicate VM identity")
		}
		seen[r.VMID] = true
	}
	return page, nil
}

func vmPageManifest(page []ClusterResource) string {
	// Ignore counters: they change between two valid reads. Identity, ownership,
	// shape and readiness must agree (including migrations/template conversion).
	rows := make([]string, 0, len(page))
	for _, r := range page {
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|%s|%g|%d|%d", r.ID, r.Node, r.Pool, r.Name, r.Template, r.Tags, r.Lock, r.Status, r.MaxCPU, r.MaxMem, r.MaxDisk))
	}
	slices.Sort(rows)
	return strings.Join(rows, "\n")
}

func (s *Service) readVMInventory(ctx context.Context, project, typ string, sampled []ClusterResource, isSampled bool) (*vmInventory, error) {
	bad := func(err error) (*vmInventory, error) {
		return nil, vmInventoryError(typ, "", "cannot prove VM inventory; cached rows retained", err)
	}
	page, err := s.C.inventoryVMPage(ctx, s.PoolFor(project))
	if err != nil {
		return bad(err)
	}
	if isSampled && vmPageManifest(page) != vmPageManifest(sampled) {
		return bad(errors.New("sample changed or was incomplete"))
	}
	pool, err := s.holderPool(ctx, s.PoolFor(project))
	if err != nil {
		return bad(err)
	}
	var nodes []Node
	if err = s.C.inventoryRead(ctx, "/nodes", nil, &nodes); err != nil || len(nodes) == 0 {
		return bad(errors.Join(err, errors.New("node inventory is unknown")))
	}
	online := map[string]bool{}
	names := []string{}
	seenNodes := map[string]bool{}
	for _, n := range nodes {
		if n.Node == "" || strings.ContainsAny(n.Node, "/\\\x00") || seenNodes[n.Node] {
			return bad(errors.New("invalid node identity"))
		}
		seenNodes[n.Node] = true
		online[n.Node] = n.Status == "online"
		names = append(names, n.Node)
	}
	inv := &vmInventory{svc: s, project: project, page: page, pool: pool, configs: map[int]VMConfig{}, nodes: names, online: online, addressUnknown: map[int]bool{}}
	members := map[int]PoolMember{}
	for _, m := range pool.Members {
		if m.Type == "qemu" || m.Type == "lxc" {
			members[m.VMID] = m
		}
	}
	for _, r := range page {
		if r.Pool != pool.PoolID {
			continue
		}
		m, ok := members[r.VMID]
		if !ok || m.Type != r.Type || m.Node != r.Node {
			return bad(errors.New("pool and VM page disagree"))
		}
		delete(members, r.VMID)
		if r.Type != "qemu" {
			continue
		}
		if !online[r.Node] || rowUnread(r) || r.Status == "unknown" || r.Lock != "" {
			return bad(errors.New("project VM is offline, locked or unreadable"))
		}
		if err := s.C.vmInventoryPermission(ctx, "/vms/"+strconv.Itoa(r.VMID), "VM.Audit", false); err != nil {
			return bad(err)
		}
		cfg, err := s.inventoryCurrentConfig(ctx, r.Node, r.VMID)
		if err != nil {
			return bad(err)
		}
		if err := validVMInventoryConfig(cfg); err != nil {
			return bad(err)
		}
		id := vmConfigUUID(cfg)
		if _, err := uuid.Parse(id); err != nil || id == uuid.Nil.String() || cfg.Str("name") != r.Name || cfg.Int("template") != r.Template || cfg.Str("tags") != r.Tags {
			return bad(errors.New("VM config identity or shape disagrees with page"))
		}
		if restoreCutDown(cfg) {
			return bad(errors.New("restore/create configuration incomplete"))
		}
		inv.configs[r.VMID] = cfg
	}
	if len(members) != 0 {
		return bad(errors.New("pool contains a VM missing from the page"))
	}
	return inv, nil
}

func vmConfigUUID(cfg VMConfig) string {
	for _, kv := range splitCommaKV(cfg.Str("smbios1")) {
		if kv[0] == "uuid" {
			return kv[1]
		}
	}
	return ""
}
func vmRowUUID(cr *cloud.CloudResource) string {
	d := cr.Data
	if cr.Type == cloud.TypeVolume {
		d, _ = d["volume"].(map[string]any)
	}
	id, _ := d[vmUUIDKey].(string)
	return id
}
func vmUUIDChanged(old, fresh *cloud.CloudResource) bool {
	a, b := vmRowUUID(old), vmRowUUID(fresh)
	return a != "" && b != "" && a != b
}

func vmRowID(cr *cloud.CloudResource) (int, error) {
	if cr.Type == cloud.TypeVolume {
		d, _ := cr.Data["volume"].(map[string]any)
		id := intOf(d["vmid"])
		if id > 0 {
			return id, nil
		}
		return 0, errors.New("volume carrier identity unknown")
	}
	id, err := strconv.Atoi(cr.ExternalID)
	if err != nil || id <= 0 {
		return 0, errors.New("VM identity unknown")
	}
	return id, nil
}
func (v *vmInventory) row(id int) *ClusterResource {
	for i := range v.page {
		if v.page[i].VMID == id {
			return &v.page[i]
		}
	}
	return nil
}

// completeVMVisibility reads the FULL ACL tree only after self Sys.Audit on
// /access. The all-permissions map omits NoAccess, so it cannot replace this.
func (c *Client) completeVMVisibility(ctx context.Context) (string, error) {
	if err := c.vmInventoryPermission(ctx, "/access", "Sys.Audit", false); err != nil {
		return "", err
	}
	var acls []ACLEntry
	if err := c.inventoryRead(ctx, "/access/acl", nil, &acls); err != nil || acls == nil {
		return "", errors.Join(err, errors.New("full ACL tree unknown"))
	}
	paths := map[string]string{"/vms": "VM.Audit", "/pool": "VM.Audit", "/nodes": "Sys.Audit"}
	for _, a := range acls {
		if a.Path == "" || a.RoleID == "" || a.Type == "" || a.UGID == "" || (a.Propagate != 0 && a.Propagate != 1) {
			return "", errors.New("malformed ACL tree")
		}
		for _, root := range []string{"/vms/", "/pool/", "/nodes/"} {
			if strings.HasPrefix(a.Path, root) {
				priv := "VM.Audit"
				if root == "/nodes/" {
					priv = "Sys.Audit"
				}
				paths[a.Path] = priv
			}
		}
	}
	for p, priv := range paths {
		if err := c.vmInventoryPermission(ctx, p, priv, true); err != nil {
			return "", err
		}
	}
	if err := c.vmInventoryPermission(ctx, "/access", "Sys.Audit", false); err != nil {
		return "", err
	}
	rows := make([]string, 0, len(acls))
	for _, a := range acls {
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d", a.Path, a.Type, a.UGID, a.RoleID, a.Propagate))
	}
	slices.Sort(rows)
	h := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(h[:]), nil
}

// verify validates ALL listed/cached decisions before Reconcile does ANY write.
// A single unknown member invalidates the whole resource-type/project scope.
func (v *vmInventory) verify(ctx context.Context, typ string, cached, listed []cloud.CloudResource) error {
	bad := func(id, reason string, err error) error { return vmInventoryError(typ, id, reason, err) }
	if v == nil {
		return bad("", "VM proof unavailable", nil)
	}
	if v.svc.HolderOperations != nil {
		op, err := v.svc.HolderOperations.Pending(ctx, v.svc.holderScope(v.project))
		if err != nil || op != nil {
			return bad("", "unresolved holder operation; retain inventory", err)
		}
	}
	fresh := map[string]*cloud.CloudResource{}
	for i := range listed {
		cr := &listed[i]
		if cr.ProjectID != v.project || cr.Type != typ || fresh[cr.ExternalID] != nil {
			return bad(cr.ExternalID, "invalid listed owner/type/identity", nil)
		}
		fresh[cr.ExternalID] = cr
		id, err := vmRowID(cr)
		if err != nil {
			return bad(cr.ExternalID, "listed carrier is unknown", err)
		}
		if v.configs[id] == nil || vmRowUUID(cr) != vmConfigUUID(v.configs[id]) {
			return bad(cr.ExternalID, "listed VM UUID not proved", nil)
		}
	}
	absent := []int{}
	retiring := false
	for i := range cached {
		cr := &cached[i]
		if cr.ProjectID != v.project {
			return bad(cr.ExternalID, "cached owner outside scope", nil)
		}
		if typ == cloud.TypeVolume && CSIClusterOf(cr.Data) != "" {
			if fresh[cr.ExternalID] != nil {
				return bad(cr.ExternalID, "CSI registry row collides with provider disk; retain exact row", nil)
			}
			continue
		}
		d, _ := cr.Data["volume"].(map[string]any)
		if cr.MovingSince != nil || cr.Data["volumeMovingSince"] != nil || d["volumeMovingSince"] != nil || d["holderCarrier"] != nil {
			return bad(cr.ExternalID, "resource move is unresolved", nil)
		}
		id, err := vmRowID(cr)
		if err != nil {
			return bad(cr.ExternalID, "cached carrier is unknown", err)
		}
		if v.svc.SeedArtifacts != nil {
			pending, err := v.svc.SeedArtifacts.ServerMutationUnknown(ctx, v.svc.ServiceID, strconv.Itoa(id))
			if err != nil || pending {
				return bad(cr.ExternalID, "unresolved seed mutation", err)
			}
		}
		if cur := fresh[cr.ExternalID]; cur != nil {
			if typ == cloud.TypeServer && v.addressUnknown[id] && (vmRowUUID(cr) == "" || vmRowUUID(cr) == vmRowUUID(cur)) {
				oldSrv, _ := cr.Data["server"].(map[string]any)
				newSrv, _ := cur.Data["server"].(map[string]any)
				if addressesMissing(oldSrv["addresses"], newSrv["addresses"]) {
					return bad(cr.ExternalID, "guest address read unknown; retain exact previous row", nil)
				}
			}
			if vmUUIDChanged(cr, cur) || supersededByIdentity(cr, cur, func(d map[string]any) string {
				if typ == cloud.TypeVolume {
					d, _ = d["volume"].(map[string]any)
				}
				id, _ := d[dataIdentity].(string)
				return id
			}) {
				retiring = true
			}
			continue
		}
		r := v.row(id)
		if r != nil && r.Pool == v.pool.PoolID && v.configs[id] != nil {
			// Positive template/holder conversion or a removed disk in an unlocked,
			// current config is not absence. Disk migrations/restore are fenced above.
			if typ == cloud.TypeVolume {
				d, _ := cr.Data["volume"].(map[string]any)
				if configHasVolume(v.configs[id], cr.ExternalID) || d["holderCarrier"] != nil || d["volumeMovingSince"] != nil {
					return bad(cr.ExternalID, "disk still exists or carrier transition unresolved", nil)
				}
			}
			retiring = true
			continue
		}
		absent = append(absent, id)
	}
	var before string
	if retiring || len(absent) > 0 {
		for _, node := range v.nodes {
			if !v.online[node] {
				return bad("", "absence cannot be proved while a node is offline", nil)
			}
		}
		var err error
		if err := v.completeNodes(ctx); err != nil {
			return bad("", "complete online node membership required for retirement", err)
		}
		before, err = v.svc.C.completeVMVisibility(ctx)
		if err != nil {
			return bad("", "omission requires complete same-token VM/ACL authority (no automatic grant)", err)
		}
		for _, id := range absent {
			if v.row(id) != nil {
				return bad(strconv.Itoa(id), "VM exists outside the expected pool; owner transition needs operator reconciliation", nil)
			}
			// Even an authoritative page is bracketed by exact by-id absence on EVERY
			// online node: an omitted/migrating config must not retire an old row.
			for _, node := range v.nodes {
				var cfg VMConfig
				err := v.svc.C.inventoryRead(ctx, qemuPath(node, id)+"/config", url.Values{"current": {"1"}}, &cfg)
				if !vmConfigAbsent(err, node, id) {
					return bad(strconv.Itoa(id), "missing VM config is not positively absent", err)
				}
			}
		}
	}
	var nodes []Node
	if err := v.svc.C.inventoryRead(ctx, "/nodes", nil, &nodes); err != nil || len(nodes) != len(v.nodes) {
		return bad("", "node census changed during proof", err)
	}
	for _, n := range nodes {
		was, ok := v.online[n.Node]
		if !ok || was != (n.Status == "online") {
			return bad("", "node readiness changed during proof", nil)
		}
	}
	page, err := v.svc.C.inventoryVMPage(ctx, v.pool.PoolID)
	if err != nil || vmPageManifest(page) != vmPageManifest(v.page) {
		return bad("", "VM census changed during proof", err)
	}
	pool, err := v.svc.holderPool(ctx, v.pool.PoolID)
	if err != nil || !sameVMPool(v.pool, pool) {
		return bad("", "pool membership changed during proof", err)
	}
	for id, cfg := range v.configs {
		r := v.row(id)
		now, err := v.svc.inventoryCurrentConfig(ctx, r.Node, id)
		if err != nil || !reflect.DeepEqual(cfg, now) {
			return bad(strconv.Itoa(id), "VM configuration changed during proof", err)
		}
		if err := v.svc.C.vmInventoryPermission(ctx, "/vms/"+strconv.Itoa(id), "VM.Audit", false); err != nil {
			return bad(strconv.Itoa(id), "VM.Audit revoked during proof", err)
		}
	}
	if before != "" {
		if err := v.completeNodes(ctx); err != nil {
			return bad("", "node membership changed during proof", err)
		}
		after, err := v.svc.C.completeVMVisibility(ctx)
		if err != nil || after != before {
			return bad("", "VM authority changed during proof", err)
		}
	}
	v.verified = true
	return nil
}
func strInventory(v any) string { s, _ := v.(string); return s }
func sameVMPool(a, b *PoolInfo) bool {
	if a == nil || b == nil || a.PoolID != b.PoolID {
		return false
	}
	x, y := slices.Clone(a.Members), slices.Clone(b.Members)
	slices.SortFunc(x, func(a, b PoolMember) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(y, func(a, b PoolMember) int { return strings.Compare(a.ID, b.ID) })
	return slices.Equal(x, y)
}
func vmConfigAbsent(err error, node string, id int) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}

	exact := fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, id)
	if ae.Status == 500 {
		if strings.TrimSpace(ae.Message) == exact {
			return true
		}
		if len(ae.Errors) == 1 && strings.TrimSpace(ae.Errors["vmid"]) == exact {
			return true
		}
	}
	// A masked parameter400 is only evidence AFTER complete authority was proved.
	return ae.Status == 400 && len(ae.Errors) == 1 && ae.Errors["vmid"] == "could not find vmid '"+strconv.Itoa(id)+"'"
}

func (p *ServerSyncProvider) VerifyInventory(ctx context.Context, cached, listed []cloud.CloudResource) error {
	return p.inventory.verify(ctx, cloud.TypeServer, cached, listed)
}
func (p *VolumeSyncProvider) VerifyInventory(ctx context.Context, cached, listed []cloud.CloudResource) error {
	return p.inventory.verify(ctx, cloud.TypeVolume, cached, listed)
}
func (p *ImageSyncProvider) VerifyInventory(ctx context.Context, cached, listed []cloud.CloudResource) error {
	return p.inventory.verify(ctx, cloud.TypeImage, cached, listed)
}

// Config payloads are open-ended, but destructive disk omission must never be
// inferred from a null/object/number that VMConfig.Str silently turns into "".
func validVMInventoryConfig(cfg VMConfig) error {
	if value, present := cfg["template"]; present {
		raw, err := json.Marshal(value)
		var flag int
		if err != nil || string(raw) == "null" || json.Unmarshal(raw, &flag) != nil || (flag != 0 && flag != 1) {
			return errors.New("VM template classification is malformed")
		}
	}
	for _, key := range []string{"name", "smbios1", "tags", "lock"} {
		if val, present := cfg[key]; present {
			if _, ok := val.(string); !ok {
				return fmt.Errorf("VM config %s is malformed", key)
			}
		}
	}
	for key, val := range cfg {
		if _, disk := diskSlotPrefix(key); !disk {
			continue
		}
		text, ok := val.(string)
		if !ok || text == "" {
			return fmt.Errorf("VM disk %s has unknown configuration", key)
		}
		vol := diskVol(text)
		if vol != "none" && !strings.Contains(vol, ":") {
			return fmt.Errorf("VM disk %s volume identity unknown", key)
		}
	}
	return nil
}

// /nodes can itself be consistently filtered/partial. Cluster membership is an
// independent census under existing Sys.Audit on /; both sources must agree.
func (v *vmInventory) completeNodes(ctx context.Context) error {
	if err := v.svc.C.vmInventoryPermission(ctx, "/", "Sys.Audit", false); err != nil {
		return err
	}
	var members []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Online  *int   `json:"online"`
		Quorate *int   `json:"quorate"`
	}
	if err := v.svc.C.inventoryRead(ctx, "/cluster/status", nil, &members); err != nil || len(members) == 0 {
		return errors.Join(err, errors.New("cluster membership unknown"))
	}
	seen := map[string]bool{}
	for _, m := range members {
		if m.Type == "cluster" {
			if m.Quorate == nil || *m.Quorate != 1 {
				return errors.New("cluster is not quorate")
			}
			continue
		}
		if m.Type != "node" || m.Name == "" || seen[m.Name] || m.Online == nil || *m.Online != 1 || !v.online[m.Name] {
			return errors.New("cluster node membership unknown or offline")
		}
		seen[m.Name] = true
	}
	if len(seen) != len(v.nodes) {
		return errors.New("node list is incomplete")
	}
	return nil
}

// The flag is propagation, not a grant boolean: 0/false still grants the exact
// path. Unknown JSON or a missing flag is not evidence of effective permission.
func (c *Client) vmInventoryPermission(ctx context.Context, path, priv string, propagate bool) error {
	var perms map[string]map[string]json.RawMessage
	if err := c.inventoryRead(ctx, "/access/permissions", url.Values{"path": {path}}, &perms); err != nil {
		return err
	}
	flag := string(perms[path][priv])
	if flag != "0" && flag != "1" && flag != "false" && flag != "true" {
		return fmt.Errorf("configured token cannot prove %s on %s", priv, path)
	}
	if propagate && flag != "1" && flag != "true" {
		return fmt.Errorf("configured token cannot prove propagated %s on %s", priv, path)
	}
	return nil
}
