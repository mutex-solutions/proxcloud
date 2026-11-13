package project

import (
	"context"
	"errors"
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Outcomes of an operator attach (AttachByOperator). They are fixed codes: the bulk attach stores them per project.
const (
	AttachAttached        = "attached"         // the project is now bound to the provider
	AttachAlreadyAttached = "already_attached" // it was already: no provider call, no second binding
	AttachNotFound        = "not_found"        // no such project
	AttachDeleting        = "deleting"         // scheduled for deletion, being torn down or deleted: never attached
)

// bindingAttachedBy / attachedByBulk mark a binding the bulk attach added to a project whose first bootstrap has not run
// yet (never opened, or waiting on billing). Such a binding does not count as that bootstrap (Project.bootstrapped), so
// the project still gets every provider whose attach policy is on when it is first opened; that bootstrap then drops the
// mark. Without it, the bulk binding would make the first bootstrap skip every other provider.
const (
	bindingAttachedBy = "attachedBy"
	attachedByBulk    = "bulk"
)

// attachLockKey serializes the writers of one project's bindings: the operator attaches (per project, bulk, two bulk jobs
// for different providers) and the provisioning part of enableAndBootstrap. Each writes the whole services list it read,
// so two at once would drop a binding.
func attachLockKey(projectID string) string { return "project-attach:" + projectID }

// withAttachLock runs fn under the project's attach lock (a session advisory lock on the lock pool, so a provider call
// made while holding it does not tie up a primary pool connection). Without a database (unit tests) fn runs unlocked.
func (h *Handler) withAttachLock(ctx context.Context, projectID string, fn func() error) error {
	if !h.hasRepo() {
		return fn()
	}
	_, err := h.svc.repo.col.DB().WithAdvisoryLock(ctx, attachLockKey(projectID), false, fn)
	return err
}

// AttachByOperator is the operator's attach of one provider to one project: the per-project admin POST
// (/admin/project/{id}/external-service/{esid}), the admin create-with-provider and the bulk attach all run it. Under the
// project's attach lock it reads the project afresh, then:
//
//   - a project that does not exist is AttachNotFound, one scheduled for deletion, being torn down or deleted is
//     AttachDeleting: neither is touched;
//   - a project already bound to the provider is AttachAlreadyAttached, with no provider call and no write;
//   - otherwise BootstrapOnto provisions it (pool, RGW user, namespace…) and appends the binding: AttachAttached, or the
//     bootstrap's error.
//
// bulk marks the new binding on a project whose first bootstrap has not run (see attachedByBulk). The project's status
// is not changed and is not a condition: a DISABLED project (billing hold or operator) is attached like an ENABLED one,
// as the per-project attach always did. Attaching only adds the binding; which service a request uses is decided as
// before (#215: an explicit service id must be attached, no header keeps the default).
func (h *Handler) AttachByOperator(ctx context.Context, projectID string, es *externalservice.ExternalService, bulk bool) (string, error) {
	if es == nil {
		return "", errors.New("attach: no provider")
	}
	outcome := ""
	err := h.withAttachLock(ctx, projectID, func() error {
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
		firstPending := !p.bootstrapped()
		if err := h.BootstrapOnto(ctx, p, es); err != nil {
			return err
		}
		if bulk && firstPending && markBulkBinding(p, es.ID) {
			if err := h.svc.Save(ctx, p); err != nil {
				return err
			}
		}
		outcome = AttachAttached
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// markBulkBinding marks the project's binding to serviceID as added by the bulk attach.
func markBulkBinding(p *Project, serviceID string) bool {
	for _, svc := range p.Services {
		if m, ok := svc.(map[string]any); ok && svcField(m, "serviceId") == serviceID {
			m[bindingAttachedBy] = attachedByBulk
			return true
		}
	}
	return false
}

// clearBulkMarks drops the bulk marks once the first bootstrap ran; reports a change.
func clearBulkMarks(p *Project) bool {
	changed := false
	for _, svc := range p.Services {
		if m, ok := svc.(map[string]any); ok && m[bindingAttachedBy] != nil {
			delete(m, bindingAttachedBy)
			changed = true
		}
	}
	return changed
}

// bootstrapped reports whether the project's first bootstrap ran: it carries a binding the bulk attach did not add.
func (p *Project) bootstrapped() bool {
	for _, svc := range p.Services {
		if svcField(svc, "serviceId") != "" && svcField(svc, bindingAttachedBy) != attachedByBulk {
			return true
		}
	}
	return false
}

// ErrAttachDeleting answers the per-project attach of a project on its way out.
var ErrAttachDeleting = httpx.NewError(http.StatusBadRequest, http.StatusBadRequest, "Project is being deleted. Cannot attach a provider")
