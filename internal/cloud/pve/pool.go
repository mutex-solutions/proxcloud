package pve

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// Pool membership is the platform's tenancy primitive on PVE (the ceph-s3 "uid
// prefix" analogue): every project gets one pool, every project VM lives in it,
// and every list/mutation filters/verifies through it.

// PoolMember is one member row of GET /pools/{poolid}.
type PoolMember struct {
	ID      string `json:"id"`   // "qemu/101" | "storage/pve1/local-zfs"
	Type    string `json:"type"` // "qemu" | "lxc" | "storage"
	VMID    int    `json:"vmid"`
	Node    string `json:"node"`
	Storage string `json:"storage"`
}

// PoolInfo is GET /pools/{poolid}.
type PoolInfo struct {
	PoolID  string       `json:"poolid"`
	Comment string       `json:"comment"`
	Members []PoolMember `json:"members"`
}

// ListPools returns all pool ids (comment included; members are not).
func (c *Client) ListPools(ctx context.Context) ([]PoolInfo, error) {
	var out []PoolInfo
	if err := c.get(ctx, "/pools", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPool reads one pool including members. Returns nil (no error) when absent.
func (c *Client) GetPool(ctx context.Context, poolID string) (*PoolInfo, error) {
	var out PoolInfo
	if err := c.get(ctx, "/pools/"+url.PathEscape(poolID), nil, &out); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if out.PoolID == "" {
		out.PoolID = poolID
	}
	return &out, nil
}

// CreatePool creates a pool (synchronous). Idempotent: "already exists" is not an error.
func (c *Client) CreatePool(ctx context.Context, poolID, comment string) error {
	form := formVals{"poolid": poolID, "comment": comment}.values()
	err := c.post(ctx, "/pools", form, nil)
	if err != nil {
		if ae, ok := err.(*APIError); ok && strings.Contains(strings.ToLower(ae.Error()), "already") {
			return nil
		}
	}
	return err
}

// DeletePool removes an (empty) pool. Absent pool is not an error.
func (c *Client) DeletePool(ctx context.Context, poolID string) error {
	err := c.delete(ctx, "/pools/"+url.PathEscape(poolID), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// AddVMsToPool adds VMs to a pool (PUT /pools/{poolid} vms=...).
func (c *Client) AddVMsToPool(ctx context.Context, poolID string, vmids ...int) error {
	return c.poolVMs(ctx, poolID, vmids, false)
}

// RemoveVMsFromPool removes VMs from a pool.
func (c *Client) RemoveVMsFromPool(ctx context.Context, poolID string, vmids ...int) error {
	return c.poolVMs(ctx, poolID, vmids, true)
}

func (c *Client) poolVMs(ctx context.Context, poolID string, vmids []int, del bool) error {
	if len(vmids) == 0 {
		return nil
	}
	ids := make([]string, len(vmids))
	for i, id := range vmids {
		ids[i] = strconv.Itoa(id)
	}
	form := formVals{"vms": strings.Join(ids, ","), "delete": pbool(del)}.values()
	return c.put(ctx, "/pools/"+url.PathEscape(poolID), form, nil)
}
