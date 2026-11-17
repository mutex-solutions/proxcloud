package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Condition messages reach the operator without a credential: CAPMOX quotes the Authorization header it sent.
func TestMachineMessage(t *testing.T) {
	const secret = "3f1c9a52-0b77-4e0c-9d1e-aa00bb11cc22"
	for _, in := range []string{
		"401 authentication failure: PVEAPIToken=capmox@pve!pc-c-1=" + secret,
		"request failed: Authorization: PVEAPIToken=capmox@pve!pc-c-1=" + secret + " (retry)",
		"GET https://root:" + secret + "@pve1:8006/api2/json/nodes",
	} {
		if got := machineMessage(in); strings.Contains(got, secret) {
			t.Errorf("secret left in %q", got)
		}
	}
	if got := machineMessage("cannot reserve 8589934592B of memory on node pve3"); got != "cannot reserve 8589934592B of memory on node pve3" {
		t.Errorf("a plain message changed: %q", got)
	}
	if got := []rune(machineMessage(strings.Repeat("é", 900))); len(got) != 401 {
		t.Errorf("clipped to %d runes, want 400 and the ellipsis", len(got))
	}
}
