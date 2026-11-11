package externalservice

import (
	"context"
	"errors"
)

// ValidateTrueNASProjectOwner also guards orphan cluster cleanup, which has no
// project Handler but still must honor the project's attached NAS owners.
func (s *Service) ValidateTrueNASProjectOwner(ctx context.Context, projectID string, target *ExternalService) error {
	var p struct {
		Services []struct {
			ServiceID string `json:"serviceId"`
		} `json:"services"`
	}
	found, err := s.repo.col.DB().C("project").Get(ctx, projectID, &p)
	if err != nil || !found {
		return err // deleted project: no attached competing provider
	}
	var attached []*ExternalService
	for _, binding := range p.Services {
		es, err := s.Get(ctx, binding.ServiceID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if es.IsTrueNAS() {
			attached = append(attached, es)
		}
	}
	return ValidateTrueNASOwner(target, attached)
}
