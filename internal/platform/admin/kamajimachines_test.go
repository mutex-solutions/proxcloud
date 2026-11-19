package admin

import (
	"context"
	"errors"
	"net/http"
	"regexp"
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
client-key-data: LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVktLS0tLQpNSUlFcEFJQkFBS0NBUUVB
-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun
shortlastline0gun==
-----END RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAxyzTAILofAKeyWhoseBEGINlineWasCutOffAbove0123456
[preflight] Running pre-flight checks
error execution phase preflight: couldn't validate the identity of the API Server: Get "https://192.168.140.101:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s": dial tcp 192.168.140.101:6443: i/o timeout
To see the stack trace of this error execute with --v=5 or higher
`

func TestMaskJoinText(t *testing.T) {
	got := maskJoinText(joinLog)
	for _, secret := range []string{"abcdef.0123456789abcdef", "ghijkl.0123456789abcdef", "1f2e3d4c5b6a7988", "9f8e7d6c5b4a3928",
		"eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ", "c2lnbmF0dXJl", "11111111-2222-3333-4444-555555555555",
		"LS0tLS1CRUdJTiBSU0Eg", "MIIEpAIBAAKCAQEAu1SU", "shortlastline0gun", "PRIVATE KEY", "MIIEpAIBAAKCAQEAxyzT"} {
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
	// A log of one long line: the cut does not leave half a word (it could be half a token) or half a rune.
	oneLine := "token abcdef." + strings.Repeat("é", joinLogTailBytes) + " then the error"
	if je := joinError(ctx, files(map[string]string{cloudInitOutputPath: oneLine}, gone)); je.LogTail != "then the error" {
		t.Errorf("one long line: %q", je.LogTail)
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

// hostKeyArt is what ssh-keygen prints for one host key, as cloud-init's output log holds it.
func hostKeyArt(kind, bits string) string {
	return "Generating public/private " + kind + " key pair.\n" +
		"Your identification has been saved in /etc/ssh/ssh_host_" + kind + "_key\n" +
		"Your public key has been saved in /etc/ssh/ssh_host_" + kind + "_key.pub\n" +
		"The key fingerprint is:\n" +
		"SHA256:Qm9vdHN0cmFwRmluZ2VycHJpbnRPZlRoZUhvc3RLZXkwMTIz root@pc-c-df10cdf0-workers-pve3-ggb9k-b5rr7\n" +
		"The key's randomart image is:\n" +
		"+---[" + bits + "]---+\n" + strings.Repeat("|     . o+*=o.    |\n", 9) + "+----[SHA256]-----+\n"
}

// workerLog has the shape of /var/log/cloud-init-output.log on a worker of the BOM's node image whose join failed
// (lab round of 2026-11-19, finding Q3; the join lines are those of host105/acceptance-audit.md, "Second run"):
// kubeadm's error in the middle, then cloud-init's failure lines, then the SSH host keys, which that image writes
// after the user scripts. Not a recorded log: the lab's answer held only the 40 lines of key output. middle goes
// between kubeadm's first line and its error.
func workerLog(middle string) string {
	return `Cloud-init v. 25.3-0ubuntu1 running 'init' at Wed, 18 Nov 2026 23:08:21 +0000. Up 9.41 seconds.
ci-info: ++++++++++++++++++++++++++++++++++++++Net device info+++++++++++++++++++++++++++++++++++++++
ci-info: | Device |  Up  |           Address           |      Mask     | Scope  |     Hw-Address    |
ci-info: | ens18  | True |         10.121.1.239        | 255.255.255.0 | global | bc:24:11:a2:aa:0e |
Cloud-init v. 25.3-0ubuntu1 running 'modules:config' at Wed, 18 Nov 2026 23:08:30 +0000. Up 18.02 seconds.
Cloud-init v. 25.3-0ubuntu1 running 'modules:final' at Wed, 18 Nov 2026 23:08:36 +0000. Up 24.77 seconds.
+ kubeadm join --config /run/kubeadm/kubeadm-join-config.yaml
[preflight] Running pre-flight checks
` + middle + `error execution phase preflight: couldn't validate the identity of the API Server: failed to request the cluster-info ConfigMap: Get "https://10.90.0.100:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)
To see the stack trace of this error execute with --v=5 or higher
2026-11-18 23:13:41,512 - cc_scripts_user.py[WARNING]: Failed to run module scripts_user (scripts in /var/lib/cloud/instance/scripts)
2026-11-18 23:13:41,514 - util.py[WARNING]: Running module scripts_user (<module 'cloudinit.config.cc_scripts_user' from '/usr/lib/python3/dist-packages/cloudinit/config/cc_scripts_user.py'>) failed
` + hostKeyArt("rsa", "RSA 3072") + hostKeyArt("ecdsa", "ECDSA 256") + hostKeyArt("ed25519", "ED25519 256") +
		`-----BEGIN SSH HOST KEY FINGERPRINTS-----
256 SHA256:Qm9vdHN0cmFwRmluZ2VycHJpbnRPZlRoZUhvc3RLZXkwMTIz root@pc-c-df10cdf0-workers-pve3-ggb9k-b5rr7 (ED25519)
-----END SSH HOST KEY FINGERPRINTS-----
-----BEGIN SSH HOST KEY KEYS-----
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostKeyOfTheWorkerThatDidNotJoin0123456789 root@pc-c-df10cdf0-workers-pve3-ggb9k-b5rr7
-----END SSH HOST KEY KEYS-----
`
}

// Finding G2F-4: on a real worker every line of the log begins with "[date time] " (the shape of the lab's answer of
// 2026-11-19), and the host key output and cloud-init's tables must go all the same.
func TestJoinLogExcerptSkipsHostKeysBehindLineStamps(t *testing.T) {
	var log strings.Builder
	for _, line := range strings.Split(strings.TrimRight(workerLog(""), "\n"), "\n") {
		log.WriteString("[2026-11-19 04:44:29] " + line + "\n")
	}
	got := joinLogExcerpt(log.String())
	for _, want := range []string{"[2026-11-19 04:44:29] [preflight] Running pre-flight checks", "couldn't validate the identity of the API Server",
		"Failed to run module scripts_user"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is not in the excerpt:\n%s", want, got)
		}
	}
	for _, noise := range []string{"randomart", "Generating public/private", "SHA256", "o+*=o", "SSH HOST KEY", "ssh-ed25519", "ci-info", "| "} {
		if strings.Contains(got, noise) {
			t.Errorf("%q is still in the excerpt:\n%s", noise, got)
		}
	}
}

// Finding Q3: the end of the log is key art, and the operator must still read kubeadm's error. The old tail of 40
// lines answered the key art alone.
func TestJoinLogExcerptSkipsHostKeys(t *testing.T) {
	log := workerLog("")
	if tail := strings.Split(strings.TrimRight(log, "\n"), "\n"); strings.Contains(strings.Join(tail[len(tail)-40:], "\n"), "kubeadm") {
		t.Fatal("the fixture must end with 40 lines that do not name kubeadm, as the lab's log did")
	}
	got := joinLogExcerpt(log)
	for _, want := range []string{"+ kubeadm join --config", "[preflight] Running pre-flight checks",
		"couldn't validate the identity of the API Server", "request canceled while waiting for connection", "10.90.0.100:6443",
		"Failed to run module scripts_user"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is not in the excerpt:\n%s", want, got)
		}
	}
	for _, noise := range []string{"randomart", "Generating public/private", "SHA256", "o+*=o", "SSH HOST KEY", "ssh-ed25519", "ci-info", "modules:config"} {
		if strings.Contains(got, noise) {
			t.Errorf("%q is in the excerpt:\n%s", noise, got)
		}
	}
	if !strings.HasPrefix(got, "+ kubeadm join") {
		t.Errorf("the excerpt starts before the join:\n%s", got)
	}
}

// A join that printed much (kubeadm at a high verbosity, with the credentials of the join in the lines): the
// excerpt keeps its bounds, the start of the join, the error and cloud-init's summary, and no credential.
func TestJoinLogExcerptBoundsAndMasks(t *testing.T) {
	const token, hash = "abcdef.0123456789abcdef", "1f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a79881f2e3d4c5b6a7988"
	middle := "I1118 23:08:37.101 join.go:412] [preflight] found NodeName empty; using OS hostname as NodeName\n" +
		"I1118 23:08:37.102 joinconfiguration.go:83] loading configuration, token: " + token + ", caCertHashes: [sha256:" + hash + "]\n" +
		strings.Repeat("I1118 23:08:42.214 token.go:228] [discovery] Waiting for the cluster-info ConfigMap to receive a JWS signature for token ID \"abcdef\"\n", 300) +
		strings.Repeat("I1118 23:09:12.377 token.go:223] [discovery] Retrying due to error: failed to request the cluster-info ConfigMap with --discovery-token-ca-cert-hash sha256:"+hash+" --token "+token+"\n", 60)
	got := joinLogExcerpt(workerLog(middle))
	lines := strings.Split(got, "\n")
	gaps := strings.Count(got, " lines left out ...]")
	if len(lines)-gaps > joinLogTailLines || gaps == 0 || len(got) > joinLogTailBytes {
		t.Errorf("%d lines with %d gap lines, %d bytes", len(lines), gaps, len(got))
	}
	for _, want := range []string{"couldn't validate the identity of the API Server", "Failed to run module scripts_user", "[MASKED]", "Retrying due to error"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is not in the excerpt:\n%s", want, got)
		}
	}
	for _, secret := range []string{token, hash, "0123456789abcdef", "randomart"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q is in the excerpt:\n%s", secret, got)
		}
	}
	// The lab's own search of an answer (m0-run.md, G1.B3 "Masks") finds nothing.
	for _, re := range []string{`[a-z0-9]{6}\.[a-z0-9]{16}`, `sha256:[0-9a-f]{64}`, `[0-9a-f]{32}`} {
		if m := regexp.MustCompile(re).FindString(got); m != "" {
			t.Errorf("pattern %s matches %q", re, m)
		}
	}
	// A log with no join in it (cloud-init failed before the user scripts): its end, without the key output.
	noJoin := "Cloud-init v. 25.3 running 'init'\n2026-11-18 23:08:25,001 - util.py[WARNING]: failed stage init\n" + hostKeyArt("rsa", "RSA 3072")
	if got := joinLogExcerpt(noJoin); !strings.HasSuffix(got, "failed stage init") || strings.Contains(got, "randomart") {
		t.Errorf("no join: %q", got)
	}
	// A key block that never ends (the log was read while it was written) does not swallow what follows it.
	open := "[preflight] Running pre-flight checks\nGenerating public/private rsa key pair.\n" + strings.Repeat("filler\n", joinNoiseBlockLines) + "error: the join failed\n"
	if got := joinLogExcerpt(open); !strings.HasSuffix(got, "error: the join failed") {
		t.Errorf("open block: %q", got)
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
