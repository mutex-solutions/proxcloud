package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// What a failed kubeadm join leaves in cloud-init's output: the bootstrap token and the CA hash in every form
// kubeadm and the bootstrap script print them. None may reach the operator's screen; the error itself must.
const joinLog = `Cloud-init v. 25.1 running 'modules:final'
+ kubeadm join --config /run/kubeadm/kubeadm-join-config.yaml
+ kubeadm join 192.168.140.101:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988
    token: abcdef.0123456789abcdef
    caCertHashes:
    - sha256:1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988
  tlsBootstrapToken: "ghijkl.0123456789abcdef"
certificateKey: 9f8e7d6c5b4a39281f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988
Authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ.eyJpc3MiOiJrdWJlcm5ldGVzIn0.c2lnbmF0dXJl
PVEAPIToken=capmox@pve!pc-c-1=11111111-2222-3333-4444-555555555555
[preflight] Running pre-flight checks
error execution phase preflight: couldn't validate the identity of the API Server: Get "https://192.168.140.101:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s": dial tcp 192.168.140.101:6443: i/o timeout
To see the stack trace of this error execute with --v=5 or higher
`

func TestMaskJoinText(t *testing.T) {
	got := maskJoinText(joinLog)
	for _, secret := range []string{"abcdef.0123456789abcdef", "ghijkl.0123456789abcdef", "1f2e3d4c5b6a7988", "9f8e7d6c5b4a3928",
		"eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ", "c2lnbmF0dXJl", "11111111-2222-3333-4444-555555555555"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q survived the mask:\n%s", secret, got)
		}
	}
	for _, kept := range []string{"couldn't validate the identity of the API Server", "dial tcp 192.168.140.101:6443: i/o timeout", "[preflight] Running pre-flight checks", "cluster-info?timeout=10s"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was lost:\n%s", kept, got)
		}
	}
}

func TestJoinError(t *testing.T) {
	ctx := context.Background()
	files := func(m map[string]string, missing error) func(context.Context, string) (string, error) {
		return func(_ context.Context, path string) (string, error) {
			if c, ok := m[path]; ok {
				return c, nil
			}
			return "", missing
		}
	}
	gone := &pve.APIError{Status: 500, Message: "Agent error: failed to open file"}
	// Both files: the errors of the result and the end of the log, masked.
	long := strings.Repeat("an early line of the boot with token abcdef.0123456789abcdef\n", 400) + joinLog
	je := joinError(ctx, files(map[string]string{
		cloudInitResultPath: `{"v1":{"datasource":"DataSourceNoCloud [seed=/dev/sr0]","errors":["('scripts_user', RuntimeError('Runparts: 1 failures (runcmd) in 1 attempted, token abcdef.0123456789abcdef'))"]}}`,
		cloudInitOutputPath: long,
	}, gone))
	if je.ReadError != "" || len(je.Errors) != 1 || !strings.Contains(je.Errors[0], "Runparts: 1 failures") || strings.Contains(je.Errors[0], "abcdef.0123456789abcdef") {
		t.Errorf("result: %+v", je)
	}
	if n := strings.Count(je.LogTail, "\n") + 1; n > joinLogTailLines || len(je.LogTail) > joinLogTailBytes {
		t.Errorf("tail is %d lines, %d bytes", n, len(je.LogTail))
	}
	if !strings.Contains(je.LogTail, "i/o timeout") || strings.Contains(je.LogTail, "abcdef.0123456789abcdef") || strings.Contains(je.LogTail, "1f2e3d4c") {
		t.Errorf("tail:\n%s", je.LogTail)
	}
	// The log alone is an answer.
	if je := joinError(ctx, files(map[string]string{cloudInitOutputPath: "kubeadm: i/o timeout\n"}, gone)); je.ReadError != "" || je.LogTail != "kubeadm: i/o timeout" || len(je.Errors) != 0 {
		t.Errorf("log only: %+v", je)
	}
	// Nothing readable: a reason, and never Proxmox's own message (it names the node and the path).
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"forbidden": {&pve.APIError{Status: http.StatusForbidden, Message: "Permission check failed (/vms/112, VM.GuestAgent.FileRead)"}, "VM.GuestAgent.Unrestricted"},
		"stopped":   {&pve.APIError{Status: 500, Message: "VM 112 is not running on node pve"}, "may be stopped"},
		"timeout":   {context.DeadlineExceeded, "in time"},
		"transport": {errors.New("dial tcp 192.168.32.254:8006: connect: connection refused"), "did not answer"},
	} {
		je := joinError(ctx, files(nil, c.err))
		if !strings.Contains(je.ReadError, c.want) || strings.Contains(je.ReadError, "192.168") || strings.Contains(je.ReadError, "/vms/") || je.Errors == nil {
			t.Errorf("%s: %+v", name, je)
		}
	}
}

func TestKamajiClusterMachineReplaceRoute(t *testing.T) {
	router := chi.NewRouter()
	(&Handler{}).Routes(router)
	if !router.Match(chi.NewRouteContext(), http.MethodPost, "/admin/service/svc-1/k8s-clusters/pc-c-1/machines/pc-c-1-workers-pve-abcde-fghij/replace") {
		t.Fatal("POST /admin/service/{id}/k8s-clusters/{clusterId}/machines/{machine}/replace route is not registered")
	}
	if router.Match(chi.NewRouteContext(), http.MethodGet, "/admin/service/svc-1/k8s-clusters/pc-c-1/machines/m/replace") {
		t.Fatal("replace must not answer GET")
	}
}
