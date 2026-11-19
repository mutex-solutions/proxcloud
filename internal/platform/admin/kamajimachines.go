package admin

// kamajimachines.go: what the operator can do about a worker of a managed Kubernetes cluster that did not join. The
// machines route shows the guest's own account of the failed join (read live through the guest agent, masked, never
// stored), and one Machine can be replaced.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// The two files of a worker the operator's view reads. cloud-init writes both; kubeadm's output is in the second.
const (
	cloudInitResultPath = "/run/cloud-init/result.json"
	cloudInitOutputPath = "/var/log/cloud-init-output.log"
	// joinLogReadBytes bounds what of the log is looked at; joinLogTailBytes and joinLogTailLines what is answered.
	joinLogReadBytes = 256 << 10
	joinLogTailBytes = 6000
	joinLogTailLines = 40
	// joinLogLineBytes is the most of one line that is answered: its end.
	joinLogLineBytes = 1000
	// joinLogEndLines of the answer are always the log's last lines: cloud-init's own summary of the failure.
	joinLogEndLines = 8
	// joinNoiseBlockLines bounds one dropped block (a host key with its art is 17 lines): a block whose end is not
	// found does not take the rest of the log with it.
	joinNoiseBlockLines = 24
	// joinErrorMachines bounds the guest reads of one request: two calls to Proxmox per machine.
	// ponytail: the first five machines only; a page per machine if a cluster ever has more that did not join.
	joinErrorMachines = 5
	joinErrorTimeout  = 15 * time.Second
)

// joinSecrets are the credentials a failed join can print: the bootstrap token (it lets a machine join the cluster
// for as long as it is valid), the hash of the cluster CA that goes with it, a certificate key, a service account or
// bootstrap JWT. Order matters: the flag and key forms take the whole value before the bare forms see its parts.
// A join log should hold no key material, but the text comes from a guest: a PEM block (to its end, or to the end of
// the text), the *-data keys of a kubeconfig, and any run of base64 as long as one PEM line go too.
var joinSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?s)()-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|\z)`),
	regexp.MustCompile(`(?i)(--(?:token|tls-bootstrap-token|discovery-token|discovery-token-ca-cert-hash|certificate-key)[= ]+)\S+`),
	regexp.MustCompile(`(?i)((?:token|tlsBootstrapToken|caCertHashes|certificateKey|certificate-key|password|passwd|secret|[a-z-]+-data)"?\s*[:=]\s*(?:-\s*)?"?)[^\s",]+`),
	regexp.MustCompile(`()\b[a-z0-9]{6}\.[a-z0-9]{16}\b`),
	regexp.MustCompile(`()\bsha256:[A-Fa-f0-9]{64}\b`),
	regexp.MustCompile(`()\b[A-Fa-f0-9]{32,}\b`),
	regexp.MustCompile(`()\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`()[A-Za-z0-9+/]{64,}={0,2}`),
}

// maskJoinText takes the credentials out of a text read from a worker: the kubeadm forms above, then what every
// upstream message loses (machineMessage's rules, without its length cut).
func maskJoinText(text string) string {
	for _, re := range joinSecrets {
		text = re.ReplaceAllString(text, "${1}[MASKED]")
	}
	return machineText(text)
}

// What cloud-init prints around the join that says nothing about it. On the node image of the platform BOM (Ubuntu
// 26.04) the SSH host keys are generated and printed AFTER the user scripts, so the end of the log is 40 lines of
// key art and kubeadm's error is above it (lab round of 2026-11-19, finding Q3).
var (
	joinNoiseStart = regexp.MustCompile(`^(Generating public/private \S+ key pair|-----BEGIN SSH HOST KEY (FINGERPRINTS|KEYS)-----)`)
	joinNoiseEnd   = regexp.MustCompile(`^(\+-+\[SHA256\]-+\+|-----END SSH HOST KEY (FINGERPRINTS|KEYS)-----)`)
	// joinStart is where the last join begins: kubeadm's first line, or the command as a shell trace prints it.
	joinStart = regexp.MustCompile(`\[preflight\] Running pre-flight checks|kubeadm join`)
	// joinTelling marks the lines that say what went wrong, kubeadm's and cloud-init's.
	joinTelling = regexp.MustCompile(`(?i)error|fail|fatal|couldn't|could not|cannot|unable|timed? ?out|refused|denied|unreachable|no route|not found|invalid|expired|x509|warning`)
)

// joinLogExcerpt is the part of a worker's cloud-init output log that says why the join failed, masked: from the
// start of the last kubeadm join to the end of the log, without the SSH host key output and cloud-init's tables.
// When that is more than joinLogTailLines or joinLogTailBytes, the first line of the join, the last joinLogEndLines
// and, between them, the lines that name an error (the latest first, while they fit) are kept, with a line that
// counts each gap. A log without a join is answered the same way from its start. Never more than joinLogTailLines
// plus the gap lines, nor joinLogTailBytes.
func joinLogExcerpt(log string) string {
	if len(log) > joinLogReadBytes {
		// The first line (or, in a log of one line, the first word) is cut in two: a half token would not match the
		// masks, and half a rune is not text.
		log = cutToBytes(log, joinLogReadBytes)
	}
	var kept []string
	noise := 0 // lines of a noise block still to drop
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case noise > 0:
			if noise--; joinNoiseEnd.MatchString(line) {
				noise = 0
			}
		case joinNoiseStart.MatchString(line):
			noise = joinNoiseBlockLines
		case !strings.HasPrefix(line, "ci-info: "):
			kept = append(kept, line)
		}
	}
	// Masked as one text (a PEM block spans lines), then cut into lines again.
	lines := strings.Split(strings.Trim(maskJoinText(strings.Join(kept, "\n")), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if joinStart.MatchString(lines[i]) {
			// The shell trace of the command and kubeadm's first line belong together.
			for i > 0 && joinStart.MatchString(lines[i-1]) {
				i--
			}
			lines = lines[i:]
			break
		}
	}
	size := 0
	for i, line := range lines {
		if len(line) > joinLogLineBytes { // the cause is at the end of a kubeadm error line
			lines[i] = cutToBytes(line, joinLogLineBytes)
		}
		size += len(lines[i]) + 1
	}
	if len(lines) > joinLogTailLines || size > joinLogTailBytes {
		const gapLine = "[... %d lines left out ...]"
		keep := map[int]bool{0: true}
		size = len(lines[0]) + 1
		for i := max(len(lines)-joinLogEndLines, 1); i < len(lines); i++ {
			keep[i] = true
			size += len(lines[i]) + 1
		}
		for i := len(lines) - joinLogEndLines - 1; i > 0 && len(keep) < joinLogTailLines; i-- {
			if n := len(lines[i]) + 1 + len(gapLine) + 8; joinTelling.MatchString(lines[i]) && size+n <= joinLogTailBytes {
				keep[i] = true
				size += n
			}
		}
		var out []string
		last := -1
		for i, line := range lines {
			if !keep[i] {
				continue
			}
			if gap := i - last - 1; gap > 0 {
				out = append(out, fmt.Sprintf(gapLine, gap))
			}
			out, last = append(out, line), i
		}
		lines = out
	}
	text := strings.Join(lines, "\n")
	if len(text) > joinLogTailBytes { // only when the first and the last lines alone are too long
		text = cutToBytes(text, joinLogTailBytes)
	}
	return text
}

// cutToBytes keeps the last n bytes of text, from the next line start on (or, in one long line, the next word): the
// text is already masked or about to be, and half a token would not match the masks; half a rune is not text.
func cutToBytes(text string, n int) string {
	text = text[len(text)-n:]
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	} else {
		text = text[strings.IndexAny(text, " \t")+1:]
	}
	return strings.ToValidUTF8(text, "")
}

// joinError reads cloud-init's result and its output log with read, and masks both; of the log it answers
// joinLogExcerpt. A file that cannot be read is left out; when neither can, ReadError says why in words that name no
// internal address.
func joinError(ctx context.Context, read func(ctx context.Context, path string) (string, error)) *kamaji.JoinError {
	out := &kamaji.JoinError{Errors: []string{}}
	result, resultErr := read(ctx, cloudInitResultPath)
	if resultErr == nil {
		var doc struct {
			V1 struct {
				Errors []string `json:"errors"`
			} `json:"v1"`
		}
		if json.Unmarshal([]byte(result), &doc) == nil {
			for _, e := range doc.V1.Errors {
				out.Errors = append(out.Errors, machineMessage(maskJoinText(e)))
			}
		}
	}
	log, logErr := read(ctx, cloudInitOutputPath)
	if logErr == nil {
		out.LogTail = joinLogExcerpt(log)
	}
	if resultErr != nil && logErr != nil {
		var ae *pve.APIError
		switch {
		case errors.As(logErr, &ae) && ae.Status == http.StatusForbidden:
			out.ReadError = "The provider token may not read files through the guest agent (VM.GuestAgent.Unrestricted on the worker's pool)."
		case errors.Is(logErr, context.DeadlineExceeded):
			out.ReadError = "Proxmox did not answer in time."
		default:
			out.ReadError = "The guest agent of the machine did not answer or the files do not exist: the machine may be stopped, or cloud-init did not run."
		}
	}
	return out
}

// addJoinErrors fills JoinError for the machines that did not join within their bound. Any failure is that machine's
// ReadError: the route's answer does not depend on the guest.
func (h *Handler) addJoinErrors(ctx context.Context, ks *kamaji.Service, out *kamaji.ClusterMachines) {
	var stuck []*kamaji.MachineDetail
	for i := range out.Machines {
		if m := &out.Machines[i]; m.Code == kamaji.CodeNotJoined && m.Attention && len(stuck) < joinErrorMachines {
			stuck = append(stuck, m)
		}
	}
	if len(stuck) == 0 {
		return
	}
	fail := func(msg string) {
		for _, m := range stuck {
			m.JoinError = &kamaji.JoinError{Errors: []string{}, ReadError: msg}
		}
	}
	compute := out.ComputeServiceID
	if compute == "" {
		compute = ks.Config().Defaults.ProxmoxServiceID
	}
	if compute == "" || h.pveFor == nil || h.esSvc == nil {
		fail("The Proxmox provider of this cluster's workers is not recorded, so the machine cannot be read.")
		return
	}
	es, err := h.esSvc.Get(ctx, compute)
	if err != nil || es == nil || !es.IsProxmox() {
		fail("The Proxmox provider of this cluster's workers could not be loaded.")
		return
	}
	ps, err := h.pveFor(es)
	if err != nil {
		fail("The Proxmox provider of this cluster's workers is not reachable.")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, joinErrorTimeout)
	defer cancel()
	for _, m := range stuck {
		if m.VMID == 0 || m.ProxmoxNode == "" {
			m.JoinError = &kamaji.JoinError{Errors: []string{}, ReadError: "Cluster API reports no VM for this machine."}
			continue
		}
		m.JoinError = joinError(ctx, func(ctx context.Context, path string) (string, error) {
			content, _, err := ps.C.GuestReadFile(ctx, m.ProxmoxNode, m.VMID, path)
			return content, err
		})
	}
}

// kamajiClusterMachineReplace deletes ONE worker Machine of a cluster so that Cluster API makes a new one: a new
// clone on the same Proxmox node and a new join. Body {"confirm": true} when the machine is the last Ready node of
// its group. The node group's count, placement and the project's quota are not touched. ADMIN_SERVICE_MANAGE.
func (h *Handler) kamajiClusterMachineReplace(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	_, ks, ok := h.kamajiService(w, r)
	if !ok {
		return
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, httpx.BadRequest("the body must be JSON: {\"confirm\": true} or empty"))
		return
	}
	err := ks.ReplaceMachine(r.Context(), chi.URLParam(r, "clusterId"), chi.URLParam(r, "machine"), body.Confirm)
	var ae *kamajik8s.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
		// An install older than this action. The caller is the operator: say what to apply (the tenant's route says
		// "contact the operator").
		err = httpx.NewError(http.StatusBadGateway, http.StatusBadGateway,
			"The ProxCloud service account cannot delete Machines: re-apply deploy/mgmt-cluster/rbac.yaml, or update the managed-services platform to the proxcloud-platform chart 0.1.3 or later")
	}
	if writeClusterErr(w, kamaji.ReplaceMachineError(err)) {
		return
	}
	httpx.OK(w, map[string]any{"result": "REPLACING"})
}
