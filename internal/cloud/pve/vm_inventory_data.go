package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (v *vmInventory) disks(ctx context.Context, r ClusterResource) ([]VMDisk, error) {
	disks := ParseDisks(v.configs[r.VMID])
	for i, d := range disks {
		if isCSIDisk(d.VolID) {
			continue
		}
		if d.SizeGiB > 0 {
			continue
		}
		if !strings.HasPrefix(d.Slot, "unused") {
			return nil, errors.New("disk size is unknown")
		}
		var rows []struct {
			VolID string `json:"volid"`
			Size  *int64 `json:"size"`
		}
		path := "/nodes/" + r.Node + "/storage/" + url.PathEscape(d.Storage) + "/content"
		if err := v.svc.C.inventoryRead(ctx, path, url.Values{"content": {"images"}, "vmid": {strconv.Itoa(r.VMID)}}, &rows); err != nil || rows == nil {
			return nil, errors.Join(err, errors.New("detached disk inventory unknown"))
		}
		found := false
		for _, row := range rows {
			if row.VolID == d.VolID {
				if found || row.Size == nil || *row.Size <= 0 {
					return nil, errors.New("detached disk size malformed")
				}
				found = true
				disks[i].SizeGiB = int((*row.Size + (1 << 30) - 1) >> 30)
			}
		}
		if !found {
			return nil, errors.New("detached disk omitted by storage; retain known size")
		}
	}
	return disks, nil
}

// Use the already proved current config; a second best-effort config read must
// never silently replace a known address set with {}. Agent failure is recorded
// and can veto loss of a cached dynamic address in verify.
func (v *vmInventory) addresses(ctx context.Context, r ClusterResource) (map[string]any, bool) {
	cfg := v.configs[r.VMID]
	public := map[string]bool{}
	configured := []string{}
	for _, key := range nicKeys(cfg) {
		if addr := ipconfigAddr(cfg.Str("ipconfig" + key[3:])); addr != "" {
			configured = append(configured, addr)
			public[addr] = key != "net0"
		}
	}
	var entries []any
	agentOK := r.Status != "running"
	if r.Status == "running" {
		if ifaces, err := v.svc.C.AgentNetworkInterfaces(ctx, r.Node, r.VMID); err == nil {
			agentOK = true
			entries, _ = AgentAddresses(ifaces)["private"].([]any)
		}
	}
	if len(entries) == 0 {
		slices.Sort(configured)
		for _, addr := range configured {
			entries = append(entries, map[string]any{"addr": addr, "version": 4})
		}
	}
	out := map[string]any{}
	seen := map[string]bool{}
	for _, e := range entries {
		a := e.(map[string]any)
		addr := a["addr"].(string)
		seen[addr] = true
		group := "private"
		if public[addr] {
			group = "public"
		}
		list, _ := out[group].([]any)
		out[group] = append(list, e)
	}
	for _, addr := range configured {
		if public[addr] && !seen[addr] {
			list, _ := out["public"].([]any)
			out["public"] = append(list, map[string]any{"addr": addr, "version": 4})
		}
	}
	return out, agentOK
}

func (v *vmInventory) snapshots(ctx context.Context, r ClusterResource) (map[string]any, error) {
	var rows []Snapshot
	if err := v.svc.C.inventoryRead(ctx, qemuPath(r.Node, r.VMID)+"/snapshot", nil, &rows); err != nil || rows == nil {
		return nil, errors.Join(err, errors.New("snapshot inventory unknown"))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Name == "" || seen[row.Name] || !isPlatformSnapshot(row.Name) && row.SnapTime <= 0 {
			return nil, fmt.Errorf("snapshot identity/time malformed")
		}
		seen[row.Name] = true
	}
	rows = slices.DeleteFunc(rows, func(r Snapshot) bool { return isPlatformSnapshot(r.Name) })
	if len(rows) == 0 {
		return nil, nil
	}
	slices.SortFunc(rows, func(a, b Snapshot) int { return strings.Compare(a.Name, b.Name) })
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{"name": row.Name, "createdAt": time.Unix(row.SnapTime, 0).UTC().Format(time.RFC3339)})
	}
	return map[string]any{"items": items, "diskGb": diskGbOf(v.configs[r.VMID])}, nil
}

func addressesMissing(old, fresh any) bool {
	collect := func(v any) map[string]bool {
		out := map[string]bool{}
		if groups, ok := v.(map[string]any); ok {
			for _, items := range groups {
				if rows, ok := items.([]any); ok {
					for _, item := range rows {
						if row, ok := item.(map[string]any); ok {
							if a, ok := row["addr"].(string); ok {
								out[a] = true
							}
						}
					}
				}
			}
		}
		return out
	}
	a, b := collect(old), collect(fresh)
	for ip := range a {
		if !b[ip] {
			return true
		}
	}
	return false
}

// inventoryCurrentConfig is stricter than legacy convenience readers. Pending
// [{}], null members or a malformed NIC cannot mean an empty pending set.
func (s *Service) inventoryCurrentConfig(ctx context.Context, node string, id int) (VMConfig, error) {
	path := qemuPath(node, id)
	var current, merged VMConfig
	if err := s.C.inventoryRead(ctx, path+"/config", url.Values{"current": {"1"}}, &current); err != nil {
		return nil, err
	}
	if err := s.C.inventoryRead(ctx, path+"/config", url.Values{"current": {"0"}}, &merged); err != nil {
		return nil, err
	}
	if current == nil || merged == nil || current.Str("lock") != "" || merged.Str("lock") != "" {
		return nil, errors.New("VM configuration unknown or locked")
	}
	var pending []map[string]json.RawMessage
	if err := s.C.inventoryRead(ctx, path+"/pending", nil, &pending); err != nil || pending == nil {
		return nil, errors.Join(err, errors.New("pending configuration unknown"))
	}
	seen := map[string]bool{}
	for _, row := range pending {
		var key string
		if json.Unmarshal(row["key"], &key) != nil || key == "" || seen[key] {
			return nil, errors.New("pending identity malformed")
		}
		seen[key] = true
		if raw, ok := row["pending"]; ok && string(raw) != "null" {
			return nil, errors.New("pending mutation exists")
		}
		if raw, ok := row["delete"]; ok && string(raw) != "0" {
			return nil, errors.New("pending deletion or malformed flag")
		}
	}
	delete(current, "digest")
	delete(merged, "digest")
	if !reflect.DeepEqual(current, merged) {
		return nil, errors.New("current and merged config differ")
	}
	if err := validVMInventoryConfig(current); err != nil {
		return nil, err
	}
	for key, value := range current {
		if !numberedConfigKey(key, "net") && !numberedConfigKey(key, "ipconfig") {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" {
			return nil, errors.New("network configuration malformed")
		}
		if numberedConfigKey(key, "net") {
			kv := splitCommaKV(text)
			if len(kv) == 0 {
				return nil, errors.New("NIC identity unknown")
			}
			if _, err := net.ParseMAC(kv[0][1]); err != nil {
				return nil, errors.New("NIC MAC malformed")
			}
		} else {
			for _, kv := range splitCommaKV(text) {
				if kv[0] != "ip" && kv[0] != "ip6" {
					continue
				}
				if kv[1] == "dhcp" || kv[1] == "auto" || kv[1] == "manual" {
					continue
				}
				if _, _, err := net.ParseCIDR(kv[1]); err != nil {
					return nil, errors.New("NIC address malformed")
				}
			}
		}
	}
	return current, nil
}
func numberedConfigKey(key, prefix string) bool {
	suffix, ok := strings.CutPrefix(key, prefix)
	if !ok || suffix == "" {
		return false
	}
	_, err := strconv.ParseUint(suffix, 10, 16)
	return err == nil
}
