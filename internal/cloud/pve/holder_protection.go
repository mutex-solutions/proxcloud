package pve

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// holderSync journals only the documented synchronous HA/replication DELETEs
// used by server retirement. A nil error is an acknowledged completed request,
// NOT the async API's missing UPID. A lost response leaves a no-UPID operator hold.
func (s *Service) holderSync(ctx context.Context, p holderProof, dispatch func(context.Context) error) error {
	h := holderFrom(ctx)
	if h == nil {
		return dispatch(ctx)
	}
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
	if ctx.Err() != nil {
		return holderPending(op)
	}
	call, c := context.WithTimeout(ctx, h.budgets.Config)
	err = dispatch(call)
	c()
	if err != nil {
		if holderRefused(err) {
			q, c := context.WithTimeout(ctx, h.budgets.DB)
			e := h.store.Settle(q, h.lease, op, "REFUSED")
			c()
			if e == nil {
				return err
			}
		}
		return fmt.Errorf("%w: %v", holderPending(op), err)
	}
	return s.observeHolder(ctx, op, false)
}
func (s *Service) holderProject(ctx context.Context) (string, error) {
	h := holderFrom(ctx)
	if h == nil {
		return "", fmt.Errorf("holder session required")
	}
	return s.holderProjectForPool(strings.TrimPrefix(h.scope, "pve-holders/"+s.ServiceID+"/"))
}
func (s *Service) holderProtectionDelete(ctx context.Context, vm *ClusterResource, kind, id string, dispatch func(context.Context) error) error {
	project, err := s.holderProject(ctx)
	if err != nil {
		return err
	}
	p, err := s.holderProofFor(ctx, project, kind, vm)
	if err != nil {
		return err
	}
	p.Params = map[string]string{"id": id}
	// Preserve idempotent removal when a prior successful stage already removed
	// the HA object. A positive authorized list is required; errors do not skip.
	if absent, e := s.holderProtectionEffect(ctx, p); e != nil {
		return e
	} else if absent {
		return nil
	}
	return s.holderSync(ctx, p, dispatch)
}

// holderDeleteEffect binds a successful qmdestroy to its original owned identity.
// Purge removes pool-derived VM.Audit, so a post-delete403 cannot be the proof.
// A successful, positively authorized pool read omitting the task's exact VMID
// plus the stopped qmdestroy proves that OLD identity was removed. A VMID reused
// elsewhere is never touched or adopted.
func (s *Service) holderDeleteEffect(ctx context.Context, op *HolderOperation) (bool, error) {
	p := op.Proof
	parts := strings.Split(string(op.UPID), ":")
	if !validHolderUPID(op.UPID, p.Source) || parts[5] != "qmdestroy" || parts[6] != strconv.Itoa(p.Source.VMID) || p.Source.UUID == "" {
		return false, nil
	}
	pool, err := s.holderPool(ctx, p.Pool)
	if err != nil {
		return false, err
	}
	if pool == nil {
		return false, nil
	}
	for _, m := range pool.Members {
		if m.Type == "qemu" && m.VMID == p.Source.VMID {
			return false, nil
		}
	}
	missing, err := s.C.MissingPrivileges(ctx, []PrivCheck{{Path: "/pool/" + p.Pool, Privs: []string{"Pool.Audit"}}})
	return err == nil && len(missing) == 0, err
}

func (s *Service) holderProtectionEffect(ctx context.Context, p holderProof) (bool, error) {
	// A permission-filtered empty HA list is not evidence of successful deletion.
	missing, err := s.C.MissingPrivileges(ctx, []PrivCheck{{Path: "/", Privs: []string{"Sys.Audit"}}})
	if err != nil {
		return false, err
	}
	if len(missing) != 0 {
		return false, fmt.Errorf("protection inventory is not positively authorized")
	}
	switch p.Kind {
	case "replication-delete":
		jobs, e := holderList[ReplicationJob](ctx, s.C, "/cluster/replication")
		if e != nil {
			return false, e
		}
		for _, j := range jobs {
			if j.ID == "" || j.Guest <= 0 {
				return false, fmt.Errorf("holder inventory: malformed replication job")
			}
			if j.ID == p.Params["id"] && j.RemoveJob == "" {
				return false, nil
			}
		}
		return true, nil
	case "ha-rule-delete":
		rules, e := holderList[HARule](ctx, s.C, "/cluster/ha/rules")
		if e != nil {
			return false, e
		}
		for _, r := range rules {
			if r.Rule == "" {
				return false, fmt.Errorf("holder inventory: malformed HA rule")
			}
			if r.Rule == p.Params["id"] {
				return false, nil
			}
		}
		return true, nil
	case "ha-delete":
		rows, e := holderList[HAResource](ctx, s.C, "/cluster/ha/resources")
		if e != nil {
			return false, e
		}
		for _, r := range rows {
			if r.SID == "" {
				return false, fmt.Errorf("holder inventory: malformed HA resource")
			}
			if r.SID == "vm:"+strconv.Itoa(p.Source.VMID) {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}
