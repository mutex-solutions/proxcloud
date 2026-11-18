package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
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

// A CSI migration that stops on a backend answers its report and the error, and logs why: the answer does not go
// through httpx.WriteError, and nothing else records the cause.
func TestWriteReportErrLogsTheCause(t *testing.T) {
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	defer slog.SetDefault(old)

	cause := errors.New("tenant cluster: Delete https://10.0.0.7:6443/api/v1/namespaces/csi-proxmox/secrets/x: i/o timeout")
	err := httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, "The tenant cluster is unavailable. Try again later.").WithCause(cause)
	rec := httptest.NewRecorder()
	rec.Header().Set(httpx.RequestIDHeader, "req-9")
	if !writeReportErr(rec, map[string]any{"switched": true}, err) {
		t.Fatal("an error with a report was not answered")
	}
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"switched":true`) || strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("answered %d %s", rec.Code, rec.Body.String())
	}
	var line map[string]any
	if err := json.Unmarshal(logged.Bytes(), &line); err != nil {
		t.Fatalf("the log is not one JSON line: %q", logged.String())
	}
	if line["msg"] != "backend request failed" || line["err"] != cause.Error() || line["reqid"] != "req-9" {
		t.Errorf("the log line: %v", line)
	}
	// Without a report, or with a plain error, the usual answer is written by the caller.
	if writeReportErr(httptest.NewRecorder(), nil, err) || writeReportErr(httptest.NewRecorder(), 1, errors.New("x")) || writeReportErr(httptest.NewRecorder(), 1, nil) {
		t.Error("answered what writeClusterErr answers")
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
