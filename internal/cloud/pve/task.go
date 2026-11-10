package pve

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UPID is a Proxmox task id, e.g.
// "UPID:pve1:0003C4D1:1A2B3C4D:65F0A1B2:qmcreate:101:root@pam!proxcloud:".
// Every asynchronous mutation returns one; the task's node is embedded in it.
type UPID string

// Node extracts the node name a task runs on (2nd colon-separated field).
func (u UPID) Node() string {
	parts := strings.Split(string(u), ":")
	if len(parts) < 2 || parts[0] != "UPID" {
		return ""
	}
	return parts[1]
}

// TaskStatus is GET /nodes/{node}/tasks/{upid}/status.
type TaskStatus struct {
	Status     string `json:"status"`     // "running" | "stopped"
	ExitStatus string `json:"exitstatus"` // "OK" or an error string, set once stopped
	Type       string `json:"type"`
	ID         string `json:"id"`
}

// TaskStatus reads one task's status.
func (c *Client) TaskStatus(ctx context.Context, upid UPID) (*TaskStatus, error) {
	node := upid.Node()
	if node == "" {
		return nil, fmt.Errorf("pve: malformed UPID %q", upid)
	}
	var st TaskStatus
	if err := c.get(ctx, "/nodes/"+node+"/tasks/"+string(upid)+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// WaitTask polls a task until it stops, then returns nil iff exitstatus is OK.
// The poll interval starts fast (VM state flips are usually sub-second) and backs
// off to 2s; the caller bounds the total wait via ctx.
//
// ⚠ A stopped-OK task does NOT guarantee the object is visible in
// /cluster/resources yet (pmxcfs propagation) — read back through the node-scoped
// endpoint after waiting, never the cluster one.
func (c *Client) WaitTask(ctx context.Context, upid UPID) error {
	if upid == "" {
		return nil // some endpoints return no UPID for synchronous no-ops
	}
	delay := 200 * time.Millisecond
	for {
		st, err := c.TaskStatus(ctx, upid)
		if err != nil {
			return fmt.Errorf("pve: wait %s: %w", upid, err)
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("pve: task %s failed: %s", upid, st.ExitStatus)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pve: wait %s: %w", upid, ctx.Err())
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// TaskStopped reports whether a task has stopped, waiting up to wait for it and retrying failed reads, whatever ctx
// says. False means unknown, so a caller about to destroy what the task may write to must leave it alone.
func (c *Client) TaskStopped(ctx context.Context, upid UPID, wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	for {
		if st, err := c.TaskStatus(ctx, upid); err == nil && st.Status == "stopped" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// TaskLog tails a task's log (for surfacing failure detail to the admin UI).
func (c *Client) TaskLog(ctx context.Context, upid UPID, limit int) ([]string, error) {
	node := upid.Node()
	if node == "" {
		return nil, fmt.Errorf("pve: malformed UPID %q", upid)
	}
	var rows []struct {
		T string `json:"t"`
	}
	form := formVals{"limit": itoa(limit)}.values()
	if err := c.get(ctx, "/nodes/"+node+"/tasks/"+string(upid)+"/log", form, &rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.T)
	}
	return out, nil
}

// NodeTask is one row of GET /nodes/{node}/tasks. EndTime is 0 while the task runs.
type NodeTask struct {
	UPID      string `json:"upid"`
	Node      string `json:"node"`
	Type      string `json:"type"`
	ID        string `json:"id"` // the vmid for VM tasks
	User      string `json:"user"`
	Status    string `json:"status"`
	StartTime int64  `json:"starttime"`
	EndTime   int64  `json:"endtime"`
}

// NodeTasks lists a node's tasks started at or after since, running ones included. Seeing other
// users' tasks takes Sys.Audit on the node; without it only the token's own come back.
func (c *Client) NodeTasks(ctx context.Context, node string, since int64, limit int) ([]NodeTask, error) {
	form := formVals{"since": strconv.FormatInt(since, 10), "source": "all", "limit": itoa(limit)}.values()
	var out []NodeTask
	if err := c.get(ctx, "/nodes/"+url.PathEscape(node)+"/tasks", form, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RestoreRunning reports a restore task running on a VM. A restore is the token's own task, so it needs no Sys.Audit to see.
func (c *Client) RestoreRunning(ctx context.Context, node string, vmid int) (bool, error) {
	form := formVals{"source": "active", "vmid": itoa(vmid), "typefilter": "qmrestore"}.values()
	var out []NodeTask
	if err := c.get(ctx, "/nodes/"+url.PathEscape(node)+"/tasks", form, &out); err != nil {
		return false, err
	}
	if out == nil {
		return false, fmt.Errorf("pve: active restore task list is unavailable")
	}
	return slices.ContainsFunc(out, func(t NodeTask) bool { return t.EndTime == 0 }), nil
}
