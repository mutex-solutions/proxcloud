package syncjob

import (
	"context"
	"sync"
)

// One sync invocation only: visibility failure must suppress its dependent
// destructive passes, without leaving sticky process-wide failure state.
type inventoryFailures struct {
	mu                  sync.Mutex
	providers, projects map[string]bool
}
type inventoryFailuresKey struct{}

func withInventoryFailures(ctx context.Context) context.Context {
	if _, ok := ctx.Value(inventoryFailuresKey{}).(*inventoryFailures); ok {
		return ctx
	}
	return context.WithValue(ctx, inventoryFailuresKey{}, &inventoryFailures{providers: map[string]bool{}, projects: map[string]bool{}})
}
func markInventoryFailure(ctx context.Context, serviceID, projectID string) {
	if s, ok := ctx.Value(inventoryFailuresKey{}).(*inventoryFailures); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.providers[serviceID], s.projects[projectID] = true, true
	}
}
func inventoryFailed(ctx context.Context, serviceID, projectID string) bool {
	if s, ok := ctx.Value(inventoryFailuresKey{}).(*inventoryFailures); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		return serviceID != "" && s.providers[serviceID] || projectID != "" && s.projects[projectID]
	}
	return false
}
