package admin

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestAuditCaptureSeesUntypedError500: an untyped error used to write nothing, so the capture kept
// its default 200 and the failed mutation was audited as a success.
func TestAuditCaptureSeesUntypedError500(t *testing.T) {
	cap := &auditCapture{ResponseWriter: httptest.NewRecorder(), status: 200}
	httpx.WriteError(cap, errors.New("db down"))
	if cap.status != 500 {
		t.Fatalf("capture status %d, want 500", cap.status)
	}
}
