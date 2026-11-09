package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// holderList requires positive list shape rather than treating a missing/null
// envelope as the typed slice's zero value. Only holder proof readers use it.
func holderList[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var raw json.RawMessage
	if err := c.get(ctx, path, nil, &raw); err != nil {
		return nil, err
	}
	var rows []T
	if len(raw) == 0 || json.Unmarshal(raw, &rows) != nil || rows == nil {
		return nil, fmt.Errorf("holder inventory: %s did not return an array", path)
	}
	return rows, nil
}

// holderPool is a proof reader, not the permissive GetPool convenience method.
// Missing/null/malformed data or member fields are UNKNOWN, never an empty pool.
func (s *Service) holderPool(ctx context.Context, id string) (*PoolInfo, error) {
	var raw json.RawMessage
	if err := s.C.get(ctx, "/pools/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, fmt.Errorf("holder pool: missing object")
	}
	var name string
	if json.Unmarshal(fields["poolid"], &name) != nil || name != id {
		return nil, fmt.Errorf("holder pool: wrong or missing identity")
	}
	members, present := fields["members"]
	if !present || len(members) == 0 || string(members) == "null" {
		return nil, fmt.Errorf("holder pool: unknown membership")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(members, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("holder pool: membership is not an array")
	}
	out := &PoolInfo{PoolID: name, Members: make([]PoolMember, 0, len(entries))}
	seen := map[string]bool{}
	for _, entry := range entries {
		var m PoolMember
		if json.Unmarshal(entry, &m) != nil || m.ID == "" || seen[m.ID] {
			return nil, fmt.Errorf("holder pool: invalid member identity")
		}
		seen[m.ID] = true
		switch m.Type {
		case "qemu", "lxc":
			if m.VMID <= 0 || m.Node == "" || m.ID != m.Type+"/"+strconv.Itoa(m.VMID) {
				return nil, fmt.Errorf("holder pool: invalid guest member")
			}
		case "storage":
			if m.Node == "" || m.Storage == "" || m.ID != "storage/"+m.Node+"/"+m.Storage {
				return nil, fmt.Errorf("holder pool: invalid storage member")
			}
		default:
			return nil, fmt.Errorf("holder pool: unsupported member type %q", m.Type)
		}
		if strings.ContainsAny(m.Node, "/\\\x00") {
			return nil, fmt.Errorf("holder pool: invalid node")
		}
		out.Members = append(out.Members, m)
	}
	return out, nil
}
