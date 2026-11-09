package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// Observation DTOs deliberately contain no config, credential, address, raw error
// or platform incident dependency. Values are owned copies, not inventory maps.
type ObservationTrust string

const (
	ObservationVerified    ObservationTrust = "VERIFIED"
	ObservationUntrusted   ObservationTrust = "VISIBILITY_UNTRUSTED"
	ObservationUnreachable ObservationTrust = "UNREACHABLE_UNKNOWN"
	ObservationMalformed   ObservationTrust = "MALFORMED"
)

type ObservedNode struct{ NodeID, Status string }
type NodeObservation struct {
	ProviderTrust, Trust ObservationTrust
	Nodes                []ObservedNode
}
type ObservedServer struct {
	ProjectID, ServerID, InstanceID, NodeID string
	Enabled, HA                             bool
	Mode                                    string
	LastSync                                int64
	FailCount                               int
}
type ServerObservation struct {
	VMTrust, ProtectionTrust ObservationTrust
	Servers                  []ObservedServer
	LinksComplete            bool
}

func observationTrust(err error) ObservationTrust {
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status >= 500 {
			return ObservationUnreachable
		}
		return ObservationUntrusted
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return ObservationUnreachable
	}
	return ObservationMalformed
}
func observationID(s string) bool { return s != "" && !strings.ContainsAny(s, "/\\\x00") }

// nodeAuthority is independent of VM.Audit: loss of VM visibility cannot suppress
// a node fact whose SAME token still proves complete node membership visibility.
func (c *Client) nodeAuthority(ctx context.Context) (string, error) {
	if err := c.vmInventoryPermission(ctx, "/", "Sys.Audit", false); err != nil {
		return "", err
	}
	if err := c.vmInventoryPermission(ctx, "/access", "Sys.Audit", false); err != nil {
		return "", err
	}
	var acls []ACLEntry
	if err := c.inventoryRead(ctx, "/access/acl", nil, &acls); err != nil {
		return "", err
	}
	if acls == nil {
		return "", errors.New("ACL unavailable")
	}
	paths := map[string]bool{"/nodes": true}
	for _, a := range acls {
		if a.Path == "" || a.RoleID == "" || a.Type == "" || a.UGID == "" || (a.Propagate != 0 && a.Propagate != 1) {
			return "", errors.New("ACL malformed")
		}
		if strings.HasPrefix(a.Path, "/nodes/") {
			paths[a.Path] = true
		}
	}
	for path := range paths {
		if err := c.vmInventoryPermission(ctx, path, "Sys.Audit", true); err != nil {
			return "", err
		}
	}
	slices.SortFunc(acls, func(a, b ACLEntry) int {
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		return strings.Compare(string(aa), string(bb))
	})
	b, _ := json.Marshal(acls)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func (c *Client) nodeCensus(ctx context.Context) ([]ObservedNode, error) {
	var nodes []Node
	if err := c.inventoryRead(ctx, "/nodes", nil, &nodes); err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("node census missing")
	}
	byName := map[string]string{}
	for _, n := range nodes {
		if !observationID(n.Node) || byName[n.Node] != "" || (n.Status != "online" && n.Status != "offline") {
			return nil, errors.New("node census malformed")
		}
		byName[n.Node] = n.Status
	}
	var members []struct {
		Type, Name      string
		Online, Quorate *int
	}
	if err := c.inventoryRead(ctx, "/cluster/status", nil, &members); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	cluster := false
	for _, m := range members {
		if m.Type == "cluster" {
			if cluster || m.Quorate == nil || (*m.Quorate != 0 && *m.Quorate != 1) {
				return nil, errors.New("cluster census malformed")
			}
			cluster = true
			continue
		}
		if m.Type != "node" || !observationID(m.Name) || seen[m.Name] || m.Online == nil || (*m.Online != 0 && *m.Online != 1) {
			return nil, errors.New("membership malformed")
		}
		want := "offline"
		if *m.Online == 1 {
			want = "online"
		}
		if byName[m.Name] != want {
			return nil, errors.New("membership incomplete")
		}
		seen[m.Name] = true
	}
	if !cluster || len(seen) != len(byName) {
		return nil, errors.New("membership incomplete")
	}
	out := make([]ObservedNode, 0, len(nodes))
	for n, status := range byName {
		fact := "ONLINE"
		if status == "offline" {
			fact = "OBSERVED_OFFLINE"
		}
		out = append(out, ObservedNode{n, fact})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// ObserveNodes is GET-only and allows explicit OFFLINE facts; completeNodes used
// by destructive inventory proof intentionally requires every node ONLINE instead.
func (s *Service) ObserveNodes(ctx context.Context) NodeObservation {
	out := NodeObservation{ProviderTrust: ObservationUntrusted, Trust: ObservationUntrusted}
	before, err := s.C.nodeAuthority(ctx)
	if err != nil {
		out.ProviderTrust, out.Trust = observationTrust(err), observationTrust(err)
		return out
	}
	out.ProviderTrust = ObservationVerified
	nodes, err := s.C.nodeCensus(ctx)
	if err != nil {
		out.Trust = observationTrust(err)
		return out
	}
	after, err := s.C.nodeAuthority(ctx)
	if err != nil || after != before {
		if err != nil {
			out.Trust = observationTrust(err)
		}
		return out
	}
	again, err := s.C.nodeCensus(ctx)
	if err != nil || !slices.Equal(nodes, again) {
		if err != nil {
			out.Trust = observationTrust(err)
		}
		return out
	}
	out.Trust, out.Nodes = ObservationVerified, nodes
	return out
}

// observationSharedDisks never infers shared mode from a missing replication job.
// Every current disk/CD dependency must positively name enabled shared storage
// visible on the current cluster nodes, and definitions must stay unchanged.
func (s *Service) observationSharedDisks(ctx context.Context, current string, cfg VMConfig, nodes []string) bool {
	var defs []StorageDef
	if err := s.C.inventoryRead(ctx, "/storage", nil, &defs); err != nil || len(defs) == 0 {
		return false
	}
	byID := map[string]StorageDef{}
	for _, d := range defs {
		if d.Storage == "" || byID[d.Storage].Storage != "" {
			return false
		}
		byID[d.Storage] = d
	}
	required := map[string]bool{}
	for _, d := range ParseDisks(cfg) {
		if strings.Contains(d.VolID, "cloudinit") {
			continue
		}
		required[d.Storage] = true
	}
	for k, value := range cfg {
		text, ok := value.(string)
		_, slot := diskSlotPrefix(k)
		if ok && (slot || k == "efidisk0" || k == "tpmstate0") && !strings.HasPrefix(text, "none") {
			if storage, _, ok := strings.Cut(strings.Split(text, ",")[0], ":"); ok {
				required[storage] = true
			}
		}
	}
	if len(required) == 0 || !slices.Contains(nodes, current) {
		return false
	}
	for storage := range required {
		d, ok := byID[storage]
		if !ok || d.Type == "" || d.Shared != 1 || d.Disable != 0 || d.Type == "zfspool" || d.Type == "zfs" || d.Type == "lvmthin" {
			return false
		}
		for _, node := range nodes {
			if d.Nodes != "" && !slices.Contains(strings.Split(d.Nodes, ","), node) {
				return false
			}
		}
	}
	for _, node := range nodes {
		var rows []NodeStorage
		if err := s.C.inventoryRead(ctx, "/nodes/"+node+"/storage", nil, &rows); err != nil || rows == nil {
			return false
		}
		seen := map[string]bool{}
		for _, r := range rows {
			if required[r.Storage] {
				if seen[r.Storage] || r.Active != 1 || r.Enabled != 1 || r.Shared != 1 || r.Type != byID[r.Storage].Type {
					return false
				}
				seen[r.Storage] = true
			}
		}
		if len(seen) != len(required) {
			return false
		}
	}
	var after []StorageDef
	if err := s.C.inventoryRead(ctx, "/storage", nil, &after); err != nil {
		return false
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Storage < defs[j].Storage })
	sort.Slice(after, func(i, j int) bool { return after[i].Storage < after[j].Storage })
	return slices.Equal(defs, after)
}

// ObserveVerified exports ONLY platform refs that match a previously verified
// current pool/config UUID, then re-proves the same snapshot after fresh GET-only
// protection reads. An unknown source exports no guessed affected identities.
func (p *ServerSyncProvider) ObserveVerified(ctx context.Context, refs []cloud.CloudResource) ServerObservation {
	out := ServerObservation{VMTrust: ObservationUntrusted, ProtectionTrust: ObservationUntrusted}
	v := p.inventory
	if v == nil || !v.verified || v.verify(ctx, p.Type(), nil, nil) != nil {
		return out
	}
	byVM := map[int]cloud.CloudResource{}
	for _, cr := range refs {
		if cr.ID == "" || cr.ProjectID != p.projectID || cr.ServiceID != p.svc.ServiceID || cr.Type != cloud.TypeServer || cr.Region != p.region {
			return out
		}
		id, err := strconv.Atoi(cr.ExternalID)
		if err != nil || byVM[id].ID != "" {
			return out
		}
		byVM[id] = cr
	}
	for _, r := range v.page {
		if r.Type != "qemu" || r.Pool != v.pool.PoolID || r.Template == 1 || isHolder(r.Tags) || strings.HasPrefix(r.Name, imageBuildPrefix) {
			continue
		}
		cr, ok := byVM[r.VMID]
		cfg := v.configs[r.VMID]
		if !ok || cfg == nil || vmRowUUID(&cr) == "" || vmRowUUID(&cr) != vmConfigUUID(cfg) {
			return out
		}
		delete(byVM, r.VMID)
		pr, err := p.svc.inventoryProtection(ctx, r, cfg)
		if err != nil {
			return out
		}
		fact := ObservedServer{ProjectID: cr.ProjectID, ServerID: cr.ID, InstanceID: vmConfigUUID(cfg), NodeID: r.Node}
		if pr != nil {
			fact.Enabled = true
			fact.HA, _ = pr["ha"].(bool)
			fact.Mode, _ = pr["mode"].(string)
			if fact.Mode == "" {
				fact.Mode = "replicated"
			}
			fact.LastSync = int64(intOf(pr["lastSync"]))
			fact.FailCount = intOf(pr["failCount"])
			if fact.Mode == ProtectionModeShared && !p.svc.observationSharedDisks(ctx, r.Node, cfg, v.nodes) {
				return ServerObservation{VMTrust: ObservationUntrusted, ProtectionTrust: ObservationUntrusted}
			}
		}
		out.Servers = append(out.Servers, fact)
	}
	if len(byVM) != 0 || v.verify(ctx, p.Type(), nil, nil) != nil {
		return ServerObservation{VMTrust: ObservationUntrusted, ProtectionTrust: ObservationUntrusted}
	}
	out.VMTrust, out.ProtectionTrust, out.LinksComplete = ObservationVerified, ObservationVerified, true
	return out
}
