package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// HolderBudgets bound observation, not the lifetime of a PVE worker. Zero uses
// the conservative defaults; operators may increase them without changing WaitTask.
type HolderBudgets struct{ Config, Migration, Settlement, Lease, Renew, DB, Acquire time.Duration }

func (b HolderBudgets) defaults() HolderBudgets {
	for _, p := range []struct {
		v *time.Duration
		d time.Duration
	}{
		{&b.Config, 5 * time.Minute}, {&b.Migration, 15 * time.Minute}, {&b.Settlement, time.Minute},
		{&b.Lease, 90 * time.Second}, {&b.Renew, 20 * time.Second}, {&b.DB, 5 * time.Second}, {&b.Acquire, time.Minute},
	} {
		if *p.v <= 0 {
			*p.v = p.d
		}
	}
	return b
}

type holderVMIdentity struct {
	Node  string            `json:"node"`
	VMID  int               `json:"vmid"`
	UUID  string            `json:"uuid"`
	Disks map[string]string `json:"disks,omitempty"`
}
type holderProof struct {
	Kind       string            `json:"kind"`
	Pool       string            `json:"pool"`
	Source     holderVMIdentity  `json:"source"`
	Target     *holderVMIdentity `json:"target,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Delete     []string          `json:"delete,omitempty"`
	Slot       string            `json:"slot,omitempty"`
	TargetSlot string            `json:"targetSlot,omitempty"`
	Disk       string            `json:"disk,omitempty"`
}

type holderSessionKey struct{}
type holderSession struct {
	lease   lock.Lease
	scope   string
	store   HolderOperationStore
	budgets HolderBudgets
	cancel  context.CancelFunc
	done    chan struct{}
	work    context.Context // retained even when a cleanup caller uses WithoutCancel
	close   sync.Once
	lost    atomic.Bool
}

func (s *Service) holderScope(projectID string) string {
	return "pve-holders/" + s.ServiceID + "/" + s.PoolFor(projectID)
}
func holderFrom(ctx context.Context) *holderSession {
	v, _ := ctx.Value(holderSessionKey{}).(*holderSession)
	return v
}

// beginHolders joins nested helpers. No connection/session lock spans PVE I/O.
func (s *Service) beginHolders(ctx context.Context, projectID string) (context.Context, func(), error) {
	return s.beginHoldersExpected(ctx, projectID, "")
}
func (s *Service) beginHoldersExpected(ctx context.Context, projectID, expected string) (context.Context, func(), error) {
	if h := holderFrom(ctx); h != nil {
		if h.scope != s.holderScope(projectID) {
			return ctx, nil, errors.New("holder scope mismatch")
		}
		return ctx, func() {}, nil
	}
	if s.HolderOperations == nil {
		rel, err := s.lockHolders(ctx, projectID)
		return ctx, rel, err
	}
	b := s.HolderBudgets.defaults()
	wait, cancel := context.WithTimeout(ctx, b.Acquire)
	defer cancel()
	var x *lock.Lease
	for {
		q, c := context.WithTimeout(wait, b.DB)
		var err error
		x, err = s.HolderOperations.Acquire(q, s.holderScope(projectID), b.Lease)
		c()
		if err != nil {
			return ctx, nil, err
		}
		if x != nil {
			break
		}
		t := time.NewTimer(250 * time.Millisecond)
		select {
		case <-wait.Done():
			t.Stop()
			return ctx, nil, conflictf("Another volume operation owns this project; try again later")
		case <-t.C:
		}
	}
	work, stop := context.WithCancel(ctx)
	h := &holderSession{lease: *x, scope: x.Name, store: s.HolderOperations, budgets: b, cancel: stop, done: make(chan struct{})}
	work = context.WithValue(work, holderSessionKey{}, h)
	h.work = work
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(b.Renew)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				q, c := context.WithTimeout(work, b.DB)
				err := h.store.Renew(q, h.lease, b.Lease)
				c()
				if err != nil {
					h.lost.Store(true)
					stop()
					return
				}
			}
		}
	}()
	release := func() {
		h.close.Do(func() {
			stop()
			<-h.done
			q, c := context.WithTimeout(context.Background(), b.DB)
			defer c()
			// Pending/unknown means no release, even though the lease will expire.
			// The nonexpiring operation, not that expiry, is the safety boundary.
			if !h.lost.Load() && h.store.Check(q, h.lease) == nil {
				_ = h.store.Release(q, h.lease)
			}
		})
	}
	q, c := context.WithTimeout(work, b.DB)
	p, err := h.store.Pending(q, h.scope)
	c()
	if err == nil && p != nil && expected != "" && p.ID != expected {
		err = conflictf("Operation changed; read status first")
	}
	if err == nil && p != nil {
		err = s.observeHolder(work, p, true)
		if err == nil {
			err = conflictf("Previous volume operation %s settled. Refresh before requesting a new change; this retry dispatched nothing", p.ID)
		}
	}
	if err == nil {
		err = s.holderCheck(work)
	}
	if err != nil {
		release()
		return ctx, nil, err
	}
	return work, release, nil
}
func (s *Service) holderCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h := holderFrom(ctx)
	if h == nil {
		return nil
	}
	if err := h.work.Err(); err != nil {
		return err
	}
	q, c := context.WithTimeout(ctx, h.budgets.DB)
	defer c()
	err := h.store.Check(q, h.lease)
	if err != nil {
		h.lost.Store(true)
		h.cancel()
	}
	return err
}
func (s *Service) holderIdentity(ctx context.Context, pool string, vm holderVMIdentity) (VMConfig, error) {
	info, err := s.holderPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	owned := false
	if info != nil {
		for _, m := range info.Members {
			if m.Type == "qemu" && m.VMID == vm.VMID && m.Node == vm.Node {
				owned = true
			}
		}
	}
	if !owned {
		return nil, errors.New("holder VM is not positively owned on its expected node")
	}
	cfg, err := s.holderCurrentConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, err
	}
	if vm.UUID == "" || cfg.Str("smbios1") != vm.UUID {
		return nil, errors.New("holder VM identity changed or missing")
	}
	return cfg, nil
}

// holderCurrentConfig does not change global GetVMConfig semantics. PVE's
// default config merges pending state; that cannot prove a disk effect landed.
func (s *Service) holderCurrentConfig(ctx context.Context, node string, id int) (VMConfig, error) {
	path := qemuPath(node, id)
	var current, merged VMConfig
	if err := s.C.get(ctx, path+"/config", url.Values{"current": {"1"}}, &current); err != nil {
		return nil, err
	}
	if err := s.C.get(ctx, path+"/config", url.Values{"current": {"0"}}, &merged); err != nil {
		return nil, err
	}
	if current.Str("lock") != "" || merged.Str("lock") != "" {
		return nil, errors.New("VM config still locked")
	}
	var pending []struct {
		Key     string          `json:"key"`
		Pending json.RawMessage `json:"pending"`
		Delete  int             `json:"delete"`
	}
	if err := s.C.get(ctx, path+"/pending", nil, &pending); err != nil {
		return nil, err
	}
	if pending == nil {
		return nil, errors.New("VM pending configuration is unknown")
	}
	for _, p := range pending {
		if p.Delete != 0 || len(p.Pending) > 0 && string(p.Pending) != "null" {
			return nil, errors.New("VM config still pending")
		}
	}
	delete(current, "digest")
	delete(merged, "digest")
	if !reflect.DeepEqual(current, merged) {
		return nil, errors.New("VM current and pending config differ")
	}
	return current, nil
}
func diskIdentity(cfg VMConfig) map[string]string {
	out := map[string]string{}
	for _, d := range ParseDisks(cfg) {
		out[d.Slot] = cfg.Str(d.Slot)
	}
	return out
}
func (s *Service) holderProofFor(ctx context.Context, project, kind string, vm *ClusterResource) (holderProof, error) {
	p := holderProof{Kind: kind, Pool: s.PoolFor(project), Source: holderVMIdentity{Node: vm.Node, VMID: vm.VMID}}
	if holderFrom(ctx) == nil {
		return p, nil
	}
	cfg, err := s.holderCurrentConfig(ctx, vm.Node, vm.VMID)
	if err != nil {
		return p, err
	}
	p.Source.UUID = cfg.Str("smbios1")
	p.Source.Disks = diskIdentity(cfg)
	_, err = s.holderIdentity(ctx, p.Pool, p.Source)
	return p, err
}

// Deliberately do not reuse legacy taskRefused's all-4xx classification. A
// proxy408/499 may be a lost response AFTER dispatch, not a PVE refusal.
func holderRefused(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403 || ae.Status == 400 && len(ae.Errors) > 0 || ae.Status == 500 && strings.Contains(ae.Error(), "VM is locked ("))
}
func (s *Service) holderTask(ctx context.Context, p holderProof, budget time.Duration, dispatch func(context.Context) (UPID, error)) error {
	h := holderFrom(ctx)
	if h == nil {
		up, err := dispatch(ctx)
		if err != nil {
			return err
		}
		return s.C.WaitTask(ctx, up)
	}
	// A cleanup caller may have stripped cancellation. Never strip the lease
	// owner's cancellation/deadline when dispatching or observing a mutation.
	ctx = h.work
	if err := s.holderCheck(ctx); err != nil {
		return err
	}
	q, c := context.WithTimeout(ctx, h.budgets.DB)
	op, err := h.store.Begin(q, h.lease, p)
	c()
	if err != nil {
		return err
	}
	// Once Begin commits, ALL exits leave a pending row unless proved settled.
	if ctx.Err() != nil || h.work.Err() != nil {
		return holderPending(op)
	}
	d := ctx
	c = func() {}
	if budget > 0 {
		d, c = context.WithTimeout(ctx, budget)
	}
	up, err := dispatch(d)
	c()
	if err != nil {
		if holderRefused(err) {
			q, c := context.WithTimeout(ctx, h.budgets.DB)
			e := h.store.Settle(q, h.lease, op, "REFUSED")
			c()
			if e != nil {
				return holderPending(op)
			}
			return err
		}
		return fmt.Errorf("%w: %v", holderPending(op), err)
	}
	if !validHolderUPID(up, p.Source) {
		return fmt.Errorf("%w: mutation returned no valid task identity", holderPending(op))
	}
	op.UPID = up
	q, c = context.WithTimeout(ctx, h.budgets.DB)
	err = h.store.RecordUPID(q, h.lease, op.ID, up)
	c()
	if err != nil {
		return fmt.Errorf("%w: task reply could not be saved", holderPending(op))
	}
	return s.observeHolder(ctx, op, false)
}
func (s *Service) observeHolder(ctx context.Context, op *HolderOperation, recovery bool) error {
	h := holderFrom(ctx)
	if h == nil {
		return errors.New("holder recovery requires a lease")
	}
	if recovery && op.UPID == "" {
		return holderPending(op)
	} // no TTL/blank-config guess ever
	budget := h.budgets.Config
	if op.Proof.Kind == "migrate" {
		budget = h.budgets.Migration
	}
	if recovery {
		budget = h.budgets.Settlement
	}
	r := BoundedTaskResult{Outcome: TaskSucceeded}
	if op.UPID != "" {
		if op.Proof.Kind == "retype" && !recovery {
			// Existing heavy-copy observation budget is the caller's context. No
			// new five-minute (or arbitrary 24-hour) ceiling in this phase.
			err := s.C.WaitTask(ctx, op.UPID)
			if err == nil {
				r.Outcome = TaskSucceeded
			} else {
				r = s.C.WaitTaskBounded(ctx, op.UPID, h.budgets.Settlement)
				if r.Outcome == TaskUncertain {
					r.Err = err
				}
			}
		} else {
			r = s.C.WaitTaskBounded(ctx, op.UPID, budget)
		}
	}
	if r.Outcome == TaskUncertain {
		return fmt.Errorf("%w: %v", holderPending(op), r.Err)
	}
	settle, c := context.WithTimeout(ctx, h.budgets.Settlement)
	defer c()
	for {
		ok, err := s.holderEffect(settle, op.Proof, r.Outcome == TaskFailed)
		if !ok && r.Outcome == TaskSucceeded && op.Proof.Kind == "delete" {
			ok, err = s.holderDeleteEffect(settle, op)
		}
		if err == nil && ok {
			state := "SUCCEEDED"
			if r.Outcome == TaskFailed {
				state = "FAILED"
			}
			q, c := context.WithTimeout(settle, h.budgets.DB)
			err = h.store.Settle(q, h.lease, op, state)
			c()
			if err != nil {
				return holderPending(op)
			}
			return r.Err
		}
		t := time.NewTimer(200 * time.Millisecond)
		select {
		case <-settle.Done():
			t.Stop()
			return fmt.Errorf("%w: node identity/effect not proved", holderPending(op))
		case <-t.C:
		}
	}
}
func configHasVolume(cfg VMConfig, vol string) bool {
	for _, d := range ParseDisks(cfg) {
		if d.VolID == vol {
			return true
		}
	}
	return false
}
func diskVol(v string) string { v, _, _ = strings.Cut(v, ","); return v }

// holderEffect never treats a cluster census, task404 or empty config as proof.
func (s *Service) holderEffect(ctx context.Context, p holderProof, failed bool) (bool, error) {
	if p.Kind == "delete" {
		// The pre-dispatch UUID was positively read; the exact node config must now
		// report its canonical missing-config error AND retain VM.Audit authority.
		cfg, err := s.holderCurrentConfig(ctx, p.Source.Node, p.Source.VMID)
		if err == nil {
			return failed && cfg.Str("smbios1") == p.Source.UUID && maps.Equal(diskIdentity(cfg), p.Source.Disks), nil
		}
		var ae *APIError
		exact := fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", p.Source.Node, p.Source.VMID)
		if !errors.As(err, &ae) || ae.Status != 500 {
			return false, err
		}
		match := strings.TrimSpace(ae.Message) == exact
		for _, v := range ae.Errors {
			if strings.TrimSpace(v) == exact {
				match = true
			}
		}
		if !match {
			return false, err
		}
		missing, e := s.C.MissingPrivileges(ctx, []PrivCheck{{Path: "/vms/" + strconv.Itoa(p.Source.VMID), Privs: []string{"VM.Audit"}}})
		return e == nil && len(missing) == 0, e
	}
	if p.Kind == "migrate" {
		if p.Target == nil {
			return false, nil
		}
		p.Source.Node = p.Target.Node
	}
	cfg, err := s.holderIdentity(ctx, p.Pool, p.Source)
	if err != nil {
		return false, err
	}
	if failed && maps.Equal(diskIdentity(cfg), p.Source.Disks) {
		if p.Target != nil && p.Kind == "move" {
			target, e := s.holderIdentity(ctx, p.Pool, *p.Target)
			return e == nil && maps.Equal(diskIdentity(target), p.Target.Disks), e
		}
		return true, nil
	}
	switch p.Kind {
	case "ha-rule-delete", "ha-delete", "replication-delete":
		return s.holderProtectionEffect(ctx, p)
	case "create":
		return true, nil // exact preselected smbios UUID, node and pool
	case "stop":
		st, e := s.C.GetVMStatus(ctx, p.Source.Node, p.Source.VMID)
		return e == nil && st.Status == "stopped", e
	case "migrate":
		return maps.Equal(diskIdentity(cfg), p.Source.Disks), nil
	case "move":
		if configHasVolume(cfg, p.Disk) || p.Target == nil {
			return false, nil
		}
		target, e := s.holderIdentity(ctx, p.Pool, *p.Target)
		if e != nil {
			return false, e
		}
		v := diskVol(target.Str(p.TargetSlot))
		storage, _, _ := strings.Cut(p.Disk, ":")
		// PVE reassign renames the disk. The task is stopped, the exact old disk
		// left the source and a NEW same-storage disk occupies the reserved slot.
		return p.Target.Disks[p.TargetSlot] == "" && strings.HasPrefix(v, storage+":") && strings.Contains(v, "vm-"+strconv.Itoa(p.Target.VMID)+"-"), nil
	case "retype":
		return !configHasVolume(cfg, p.Disk) && strings.HasPrefix(cfg.Str(p.Slot), p.Params["storage"]+":"), nil
	case "resize":
		for _, d := range ParseDisks(cfg) {
			if d.Slot == p.Slot && d.VolID == p.Disk {
				n, _ := strconv.Atoi(p.Params["size"])
				return d.SizeGiB >= n, nil
			}
		}
		return false, nil
	case "config":
		for k, v := range p.Params {
			if strings.HasPrefix(k, "scsi") && strings.Contains(v, ":") && !strings.Contains(v, "vm-") {
				store, size, _ := strings.Cut(v, ":")
				n, _ := strconv.Atoi(size)
				found := false
				for _, d := range ParseDisks(cfg) {
					if d.Slot == k && d.Storage == store && d.SizeGiB >= n {
						found = true
					}
				}
				if !found {
					return false, nil
				}
			} else if strings.HasPrefix(k, "scsi") && strings.Contains(v, ":") {
				if diskVol(cfg.Str(k)) != diskVol(v) {
					return false, nil
				}
			} else if cfg.Str(k) != v {
				return false, nil
			}
		}
		for _, k := range p.Delete {
			if cfg.Str(k) != "" {
				return false, nil
			}
			v := diskVol(p.Source.Disks[k])
			if strings.HasPrefix(k, "unused") {
				if configHasVolume(cfg, v) {
					return false, nil
				}
			} else if v != "" && !configHasVolume(cfg, v) {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}
