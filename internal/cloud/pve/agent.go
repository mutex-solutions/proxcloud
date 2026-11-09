package pve

// agent.go — running things inside a guest through the qemu guest agent. This is root in the guest:
// PVE 9 asks VM.GuestAgent.Unrestricted for exec and file-write, so the platform only does it on
// VMs it owns (a load balancer's appliance), never on a tenant's server.

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// agentExecResult is GET .../agent/exec-status. PVE sends exited as 0/1 or true/false by version.
type agentExecResult struct {
	Exited   any    `json:"exited"`
	ExitCode int    `json:"exitcode"`
	OutData  string `json:"out-data"`
	ErrData  string `json:"err-data"`
}

func (r agentExecResult) done() bool {
	switch v := r.Exited.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	}
	return false
}

// GuestStart starts one command in the guest and leaves it running.
func (c *Client) GuestStart(ctx context.Context, node string, vmid int, args ...string) (pid int, err error) {
	var started struct {
		PID int `json:"pid"`
	}
	err = c.post(ctx, qemuPath(node, vmid)+"/agent/exec", url.Values{"command": args}, &started)
	return started.PID, err
}

// GuestRun runs one command in the guest and waits for it.
func (c *Client) GuestRun(ctx context.Context, node string, vmid int, timeout time.Duration, args ...string) (exit int, stdout, stderr string, err error) {
	pid, err := c.GuestStart(ctx, node, vmid, args...)
	if err != nil {
		return 0, "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		var st agentExecResult
		form := url.Values{"pid": {strconv.Itoa(pid)}}
		if err := c.get(ctx, qemuPath(node, vmid)+"/agent/exec-status", form, &st); err != nil {
			return 0, "", "", err
		}
		if st.done() {
			return st.ExitCode, st.OutData, st.ErrData, nil
		}
		select {
		case <-ctx.Done():
			return 0, "", "", fmt.Errorf("pve: guest command on vm %d did not finish: %w", vmid, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// GuestWriteFile writes a file in the guest (PVE base64-encodes it and caps it near 45 KiB).
func (c *Client) GuestWriteFile(ctx context.Context, node string, vmid int, path, content string) error {
	return c.post(ctx, qemuPath(node, vmid)+"/agent/file-write", url.Values{"file": {path}, "content": {content}}, nil)
}
