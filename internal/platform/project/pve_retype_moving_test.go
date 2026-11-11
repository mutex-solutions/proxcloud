package project

import (
	"fmt"
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

// h.cloud is deliberately nil: reaching MarkMoving (even if a follow cleared it
// later) would panic. An immediate snapshot refusal must never write that mark.
func TestRetypeSnapshotsRefuseBeforeMarkMoving(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"real snapshot and current parent", `{"data":[{"name":"before","snaptime":1794222000},{"name":"current","parent":"before","description":"You are here!"}]}`, 200},
		{"null is not empty", `{"data":null}`, 200},
		{"permission unknown", `{}`, 403},
		{"malformed", `{"data":[null]}`, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Errorf("mutation after snapshot refusal: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				switch r.URL.Path {
				case "/api2/json/cluster/resources":
					fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"pc-p1"}]}`)
				case "/api2/json/nodes/pve1/qemu/101/status/current":
					fmt.Fprint(w, `{"data":{"status":"running"}}`)
				case "/api2/json/nodes/pve1/qemu/101/snapshot":
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
				default:
					t.Errorf("unexpected read %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
			if err != nil {
				t.Fatal(err)
			}
			h := &Handler{pveFor: func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil }}
			es := &externalservice.ExternalService{ID: "svc", Config: map[string]any{"storageProfiles": map[string]any{
				"capacity": map[string]any{"storages": map[string]any{"*": "nas"}},
			}}}
			cr := &cloud.CloudResource{Type: cloud.TypeVolume, ExternalID: "local-zfs:vm-101-disk-1", Data: map[string]any{
				"volume": map[string]any{"vmid": 101, "slot": "scsi1"},
			}}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if !h.pveVolumeAction(w, r, nil, &Project{ID: "p1"}, es, cr, "RETYPE", map[string]any{"newType": "capacity"}) {
				t.Fatal("not handled")
			}
			if w.Code != 409 || cr.MovingSince != nil || writes != 0 {
				t.Fatalf("status=%d moving=%v writes=%d body=%s", w.Code, cr.MovingSince, writes, w.Body.String())
			}
		})
	}
}
