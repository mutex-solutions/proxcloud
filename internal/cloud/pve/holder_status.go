package pve

import (
	"context"
	"errors"
)

func (s *Service) HolderOperationStatus(ctx context.Context, project string) (*HolderOperation, error) {
	if s.HolderOperations == nil {
		return nil, errors.New("holder journal unavailable")
	}
	q, c := context.WithTimeout(ctx, s.HolderBudgets.defaults().DB)
	defer c()
	return s.HolderOperations.Pending(q, s.holderScope(project))
}

// PollHolderOperation is deliberately incapable of submitting a mutation. A
// previous status read pins the operation ID so an administrator cannot unknowingly
// observe/clear a different operation. No-UPID cases always remain held.
func (s *Service) PollHolderOperation(ctx context.Context, project, id string) error {
	op, err := s.HolderOperationStatus(ctx, project)
	if err != nil {
		return err
	}
	if op == nil {
		return nil
	}
	if op.ID != id {
		return conflictf("Operation changed; read status first")
	}
	if op.UPID == "" {
		return holderPending(op)
	}
	// beginHolders only observes existing pending work; even successful recovery
	// returns a refresh-required409 so the triggering request can never replay it.
	_, release, err := s.beginHoldersExpected(ctx, project, id)
	if release != nil {
		defer release()
	}
	if err != nil {
		current, e := s.HolderOperationStatus(ctx, project)
		if e == nil && current == nil {
			return nil
		}
	}
	return err
}
