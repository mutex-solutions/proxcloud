package pve

import (
	"context"
	"net/url"
)

// PCIMapping is one cluster PCI resource mapping (GET /cluster/mapping/pci) — the
// admin-side unit of GPU passthrough: one mapping per GPU model, one map entry per
// physical device per node. The scheduler counts free slots per node as
// (map entries on node) − (cached VMs using the mapping on node).
type PCIMapping struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Map         []string `json:"map"` // "id=10de:2204,iommugroup=27,node=pve1,path=0000:41:00,subsystem-id=..."
}

// ListPCIMappings lists cluster PCI mappings. Non-empty is also the host-prereq
// preflight signal (IOMMU + vfio configured ⇒ the admin could create mappings).
func (c *Client) ListPCIMappings(ctx context.Context) ([]PCIMapping, error) {
	var out []PCIMapping
	if err := c.get(ctx, "/cluster/mapping/pci", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MapEntryNode extracts the node= field of one PCIMapping.Map entry.
func MapEntryNode(entry string) string {
	for _, kv := range splitCommaKV(entry) {
		if kv[0] == "node" {
			return kv[1]
		}
	}
	return ""
}

// splitCommaKV parses "k=v,k2=v2" into pairs (PVE property-string syntax).
func splitCommaKV(s string) [][2]string {
	var out [][2]string
	for _, part := range splitEsc(s, ',') {
		for i := 0; i < len(part); i++ {
			if part[i] == '=' {
				out = append(out, [2]string{part[:i], part[i+1:]})
				break
			}
		}
	}
	return out
}

// splitEsc splits on sep. PVE property strings don't escape sep inside values we
// care about (ids, nodes, paths), so a plain split is faithful.
func splitEsc(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start <= len(s) {
		out = append(out, s[start:])
	}
	return out
}

// NodeHardwarePCI lists a node's raw PCI devices (admin preflight display).
func (c *Client) NodeHardwarePCI(ctx context.Context, node string) ([]map[string]any, error) {
	var out []map[string]any
	if err := c.get(ctx, "/nodes/"+url.PathEscape(node)+"/hardware/pci", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
