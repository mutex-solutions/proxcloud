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
