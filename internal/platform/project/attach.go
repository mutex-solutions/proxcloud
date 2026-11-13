package project

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// AttachTimeout bounds one operator attach of one project (provider calls included), per-project POST and bulk alike.
const AttachTimeout = 2 * time.Minute

// Outcomes of an operator attach (AttachByOperator). They are fixed codes: the bulk attach stores them per project.
const (
	AttachAttached        = "attached"         // the project is now bound to the provider
	AttachAlreadyAttached = "already_attached" // it was already: no provider call, no second binding
	AttachNotFound        = "not_found"        // no such project
	AttachDeleting        = "deleting"         // scheduled for deletion, being torn down or deleted: never attached
)

// bindingAttachedBy / attachedByOperator mark a binding an operator attach (per project or bulk) added to a project whose
// first bootstrap has not run yet (never opened, or waiting on billing). Such a binding does not count as that bootstrap
// (Project.bootstrapped), so the project still gets every provider whose attach policy is on when it is first opened;
// that bootstrap then drops the marks. The mark is written in the same statement that appends the binding
// (Repo.AppendBinding), so no failure can leave an unmarked binding that would stand in for the first bootstrap.
const (
	bindingAttachedBy  = "attachedBy"
	attachedByOperator = "operator"
	// attachedByBulk is the value the first version wrote; still read as a mark.
	attachedByBulk = "bulk"
)

// attachLockKey serializes the writers of one project's bindings that decide on a read: the operator attaches (per
// project, bulk, two bulk jobs for different providers) and the provisioning part of enableAndBootstrap. The binding
// itself is appended atomically (Repo.AppendBinding), so a writer outside the lock (a member change, a rename) cannot
// drop it either.
func attachLockKey(projectID string) string { return "project-attach:" + projectID }

// attachLock is a held attach lock: the session that holds it and its key. The binding writes made under it run ON that
// session, with the ownership check in the same statement (Repo.AppendBinding / ClearBulkMarks): there is no gap
// between "the lock is held" and the write, and a session that is gone fails the statement.
type attachLock struct {
	session pgdoc.LockSession
	key     string
}

// bindIntent rides on the context of a provisioning made under the attach lock: what the binding append must carry
// and where it runs.
type bindIntent struct {
	// mark: the binding is appended with attachedBy (an operator attach before the first bootstrap).
	mark bool
	// lock is the held attach lock; nil without a database (unit tests).
	lock *attachLock
}

type bindIntentKey struct{}

func withBindIntent(ctx context.Context, bi bindIntent) context.Context {
	return context.WithValue(ctx, bindIntentKey{}, bi)
}

func bindIntentOf(ctx context.Context) bindIntent {
	bi, _ := ctx.Value(bindIntentKey{}).(bindIntent)
	return bi
}

// withAttachLock runs fn under the project's attach lock (a session advisory lock on the lock pool, so a provider call
// made while holding it does not tie up a primary pool connection). fn gets the held lock: binding writes run on its
// session and are refused (ErrAttachLost) when the lock is no longer held. Without a database (unit tests) fn runs
// unlocked with a nil lock. The session is used by one writer at a time (the attach work under one lock is
// sequential).
func (h *Handler) withAttachLock(ctx context.Context, projectID string, fn func(lock *attachLock) error) error {
	if !h.hasRepo() {
		return fn(nil)
	}
	key := attachLockKey(projectID)
	_, err := h.svc.repo.col.DB().WithAdvisoryLockSession(ctx, key, false, func(s pgdoc.LockSession) error {
		return fn(&attachLock{session: s, key: key})
	})
	return err
}

// AttachByOperator is the operator's attach of one provider to one project: the per-project admin POST
// (/admin/project/{id}/external-service/{esid}) and the bulk attach (internal/providerattach) both run it. Under the
// project's attach lock it reads the project afresh, then:
//
//   - a project that does not exist is AttachNotFound, one scheduled for deletion, being torn down or deleted is
//     AttachDeleting: neither is touched;
//   - a project already bound to the provider is AttachAlreadyAttached, with no provider call and no write;
//   - otherwise BootstrapOnto provisions it (pool, RGW user, namespace…) and appends the binding: AttachAttached, or the
//     bootstrap's error.
//
// A binding added to a project whose first bootstrap has not run is marked (attachedBy), in the same write. The
// project's status is not changed and is not a condition: a DISABLED project (billing hold or operator) is attached like
// an ENABLED one, as the per-project attach always did. Attaching only adds the binding; which service a request uses is
// decided as before (#215: an explicit service id must be attached, no header keeps the default).
func (h *Handler) AttachByOperator(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error) {
	if es == nil {
		return "", errors.New("attach: no provider")
	}
	outcome := ""
	err := h.withAttachLock(ctx, projectID, func(lock *attachLock) error {
		p, err := h.svc.repo.FindByID(ctx, projectID)
		if err != nil {
			return err
		}
		switch {
		case p == nil:
			outcome = AttachNotFound
			return nil
		case p.Deleting():
			outcome = AttachDeleting
			return nil
		case p.HasService(es.ID):
			outcome = AttachAlreadyAttached
			return nil
		}
		bctx := withBindIntent(ctx, bindIntent{mark: !p.bootstrapped(), lock: lock})
		if err := h.BootstrapOnto(bctx, p, es); err != nil {
			return err
		}
		outcome = AttachAttached
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// appendBinding is the one write of every Bootstrap*Onto: it appends binding to the stored services atomically (only
// when none of the same service is there) and, for an operator attach before the first bootstrap, marks it in the same
// statement. Under the attach lock the write is refused once the lock is lost. p's bindings are updated to what is
// stored. A binding already present is no error (idempotent).
func (h *Handler) appendBinding(ctx context.Context, p *Project, binding map[string]any) error {
	bi := bindIntentOf(ctx)
	if bi.mark {
		binding[bindingAttachedBy] = attachedByOperator
	}
	if !h.hasRepo() {
		// No database (unit tests): the in-memory append is all there is.
		if !p.HasService(svcField(binding, "serviceId")) {
			p.Services = append(p.Services, binding)
		}
		return nil
	}
	if _, err := h.svc.repo.AppendBinding(ctx, p.ID, binding, bi.lock.lockedExec()); err != nil {
		return fmt.Errorf("record the binding: %w", err)
	}
	if h.afterBind != nil {
		if err := h.afterBind(ctx, p.ID); err != nil {
			return err
		}
	}
	fresh, err := h.svc.repo.FindByID(ctx, p.ID)
	if err != nil {
		return err
	}
	if fresh != nil {
		p.Services = fresh.Services
	}
	return nil
}

// SetAfterBindForTest installs a hook run right after a binding is stored, before anything else the attach does:
// tests inject a failure (a timeout, a crash) at that exact point. Never set outside tests.
func (h *Handler) SetAfterBindForTest(f func(ctx context.Context, projectID string) error) {
	h.afterBind = f
}

// isMark reports a binding an operator attach marked (any value written so far).
func isMark(svc any) bool {
	v := svcField(svc, bindingAttachedBy)
	return v == attachedByOperator || v == attachedByBulk
}

// bootstrapped reports whether the project's first bootstrap ran: it carries a binding no operator attach marked.
func (p *Project) bootstrapped() bool {
	for _, svc := range p.Services {
		if svcField(svc, "serviceId") != "" && !isMark(svc) {
			return true
		}
	}
	return false
}

// hasMarks reports bindings still marked.
func (p *Project) hasMarks() bool {
	for _, svc := range p.Services {
		if isMark(svc) {
			return true
		}
	}
	return false
}

// clearMarks drops the marks once the first bootstrap ran (one atomic statement on the stored bindings).
func (h *Handler) clearMarks(ctx context.Context, p *Project) error {
	if !p.hasMarks() {
		return nil
	}
	if h.hasRepo() {
		if _, err := h.svc.repo.ClearBulkMarks(ctx, p.ID, bindIntentOf(ctx).lock.lockedExec()); err != nil {
			return err
		}
	}
	for _, svc := range p.Services {
		if m, ok := svc.(map[string]any); ok {
			delete(m, bindingAttachedBy)
		}
	}
	return nil
}

// lockedExec is how a binding write runs under this lock: on the lock's session, only while it holds the lock (nil lock:
// unlocked, on the pool — unit tests and callers outside the lock).
func (l *attachLock) lockedExec() *LockedExec {
	if l == nil {
		return nil
	}
	return &LockedExec{Session: l.session, Key: l.key}
}

// ErrAttachDeleting answers the per-project attach of a project on its way out.
var ErrAttachDeleting = httpx.NewError(http.StatusBadRequest, http.StatusBadRequest, "Project is being deleted. Cannot attach a provider")

// CanAttach reports whether this build can attach a project to the provider: its kind is one the bootstrap provisions
// onto, and the client it needs is wired (proxmox, kamaji, dbaas, sandbox, ceph credentials).
func (h *Handler) CanAttach(es *externalservice.ExternalService) error {
	switch {
	case es.IsObjectStore():
		if h.cephCreds == nil {
			return errors.New("object stores are not wired in this API")
		}
	case es.IsKamaji():
		if h.kamajiFor == nil {
			return errors.New("managed Kubernetes is not wired in this API")
		}
	case es.IsDbaas():
		if h.dbaasFor == nil {
			return errors.New("managed databases are not wired in this API")
		}
	case es.IsProxmox():
		if h.pveFor == nil {
			return errors.New("Proxmox is not wired in this API")
		}
	case es.IsSandbox():
		if h.sandboxFor == nil {
			return errors.New("sandboxes are not wired in this API")
		}
	case es.IsTrueNAS(), es.IsPowerDNS():
	default:
		return fmt.Errorf("projects cannot be attached to a %q provider", es.Provider())
	}
	return nil
}
