package audit

import (
	"context"
	"log/slog"
	"time"
)

// Service writes audit events asynchronously (each write runs in its own
// goroutine). Handlers build an event via the ClientUserEvent/UserEvent/AdminEvent
// helpers, fill it, and call LogAsync.
type Service struct {
	repo *Repo
	log  *slog.Logger
}

func NewService(repo *Repo, log *slog.Logger) *Service { return &Service{repo: repo, log: log} }

// LogAsync persists the event in the background (fire-and-forget), never blocking
// the request. Failures are logged, not surfaced.
func (s *Service) LogAsync(ev AuditEvent) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.repo.Insert(ctx, ev); err != nil {
			s.log.Warn("audit write failed", "action", ev.Action, "resourceType", ev.ResourceType, "err", err)
		}
	}()
}

// Query exposes the cursor-paginated read for the audit handlers.
func (s *Service) Query(ctx context.Context, f Filter, after, before string, limit int) ([]AuditEvent, *string, *string, error) {
	return s.repo.Query(ctx, f, after, before, limit)
}

// QueryAll exposes the capped non-paginated read (audit export).
func (s *Service) QueryAll(ctx context.Context, f Filter, limit int) ([]AuditEvent, error) {
	return s.repo.QueryAll(ctx, f, limit)
}

// LogEvent persists an already-built event asynchronously (used by the admin audit middleware).
func (s *Service) LogEvent(ev AuditEvent) { s.LogAsync(ev) }
