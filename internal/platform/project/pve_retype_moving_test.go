package project

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// A retype clicked again while the first move copies must not reach PVE: its follow would find the old volid and
// clear the running move's mark. A mark past MoveKeepFor is a dead portal's and does not block.
func TestRetypeRefusedWhileMoving(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("reached PVE: %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
	retype := func(since time.Time) int {
		cr := &cloud.CloudResource{Type: cloud.TypeVolume, ExternalID: "local-zfs:vm-101-disk-1", MovingSince: &since,
			Data: map[string]any{"volume": map[string]any{"vmid": 101, "slot": "scsi1"}}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if !h.pveVolumeAction(w, r, nil, &Project{ID: "p1"}, &externalservice.ExternalService{ID: "svc"}, cr, "RETYPE", map[string]any{"newType": "capacity"}) {
			t.Fatal("RETYPE not handled")
		}
		return w.Code
	}
	if code := retype(time.Now().UTC().Add(-time.Minute)); code != http.StatusConflict {
		t.Fatalf("retype during a move: %d, want 409", code)
	}
	if code := retype(time.Now().UTC().Add(-cloud.MoveKeepFor - time.Minute)); code == http.StatusConflict {
		t.Fatal("a stale mark refused the retype")
	}
}
