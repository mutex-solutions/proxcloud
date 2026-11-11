package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// A plain error from the management cluster must answer, not fall through to a 200 with no clusters.
func TestWriteClusterErr(t *testing.T) {
	for _, tc := range []struct {
		err    error
		wrote  bool
		status int
	}{
		{nil, false, http.StatusOK},
		{errors.New("dial tcp 192.0.2.1:6443: i/o timeout"), true, http.StatusBadGateway},
		{httpx.BadRequest("not a Managed Kubernetes provider"), true, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		if got := writeClusterErr(rec, tc.err); got != tc.wrote || rec.Code != tc.status {
			t.Errorf("%v: wrote=%v status=%d, want %v %d", tc.err, got, rec.Code, tc.wrote, tc.status)
		}
	}
}
