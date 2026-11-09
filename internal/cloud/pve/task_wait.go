package pve

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type TaskOutcome string

const (
	TaskSucceeded TaskOutcome = "stopped-ok"
	TaskFailed    TaskOutcome = "stopped-failed"
	TaskUncertain TaskOutcome = "uncertain"
)

type BoundedTaskResult struct {
	UPID       UPID
	Outcome    TaskOutcome
	ExitStatus string
	Err        error
}

func validHolderUPID(up UPID, vm holderVMIdentity) bool {
	parts := strings.Split(string(up), ":")
	if len(parts) != 9 || parts[0] != "UPID" || parts[1] != vm.Node || parts[5] == "" || parts[6] == "" || parts[7] == "" || parts[8] != "" {
		return false
	}
	for _, field := range parts[2:5] {
		if field == "" {
			return false
		}
		if _, err := strconv.ParseUint(field, 16, 64); err != nil {
			return false
		}
	}
	// PVE qmmove uses a composite source-slot>target-slot ID, unlike power,
	// config/create/migrate/destroy whose ID is the source VMID.
	return parts[6] == strconv.Itoa(vm.VMID) || strings.HasPrefix(parts[6], strconv.Itoa(vm.VMID)+"-")
}

// WaitTaskBounded is observation ONLY. A cancelled HTTP request does not cancel
// a PVE worker. In particular 403/404 are not stopped/absent-task evidence.
// This additive API does not change clone/import/backup/seed WaitTask semantics.
func (c *Client) WaitTaskBounded(ctx context.Context, up UPID, budget time.Duration) BoundedTaskResult {
	r := BoundedTaskResult{UPID: up, Outcome: TaskUncertain}
	if up == "" || up.Node() == "" {
		r.Err = fmt.Errorf("pve: no valid task identity")
		return r
	}
	if budget <= 0 {
		r.Err = fmt.Errorf("pve: positive task budget required")
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	delay := 200 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			r.Err = err
			return r
		}
		st, err := c.TaskStatus(ctx, up)
		if err != nil {
			r.Err = err
			return r
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "" {
				r.Err = fmt.Errorf("pve: stopped task has no exit status")
				return r
			}
			r.ExitStatus = st.ExitStatus
			if st.ExitStatus == "OK" {
				r.Outcome = TaskSucceeded
			} else {
				r.Outcome = TaskFailed
				r.Err = fmt.Errorf("pve: task %s failed: %s", up, st.ExitStatus)
			}
			return r
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			r.Err = ctx.Err()
			return r
		case <-timer.C:
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}
