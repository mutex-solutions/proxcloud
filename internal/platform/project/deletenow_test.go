package project

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Without the deletion job (scheduler and debug triggers off) a delete-now is refused before anything is read or
// written: flagged, the project would stay DELETE_IN_PROGRESS for good (F42).
func TestDeleteNowRefusedWithoutDeletionJob(t *testing.T) {
	s := &Service{} // no repo: a refusal that read or wrote the project would panic
	s.SetNoDeletionJob(true)
	_, err := s.DeleteNow(context.Background(), "p1")
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || !strings.Contains(he.Msg, "scheduler is off") || !strings.Contains(he.Msg, "Nothing was changed") {
		t.Fatalf("err %v", err)
	}
}

// The same for a scheduled delete, on the same words: answered SCHEDULED_FOR_DELETION, the project would read so for
// good, since the sweep that deletes it after the grace is that job's (drill R4-o2 of v0.11.1). The project given is
// not marked.
func TestScheduleDeletionRefusedWithoutDeletionJob(t *testing.T) {
	s := &Service{} // no repo: a refusal that wrote the project would panic
	s.SetNoDeletionJob(true)
	p := &Project{ID: "p1", Status: StatusEnabled}
	_, err := s.ScheduleDeletion(context.Background(), p, false)
	_, now := s.DeleteNow(context.Background(), "p1")
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || !strings.Contains(he.Msg, "scheduler is off") || !errors.Is(err, now) {
		t.Fatalf("scheduled %v, delete-now %v", err, now)
	}
	if p.Status != StatusEnabled || p.ScheduledForDeletionAt != nil || p.StatusBeforeDeletion != "" {
		t.Fatalf("a refused delete marked the project: %+v", p)
	}
	if err := s.CanScheduleDeletion(context.Background(), "p1"); !errors.Is(err, now) {
		t.Fatalf("the operator route's check: %v", err)
	}
}
