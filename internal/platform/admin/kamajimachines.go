package admin

// kamajimachines.go: what the operator can do about a worker of a managed Kubernetes cluster that did not join. The
// machines route shows the guest's own account of the failed join (read live through the guest agent, masked, never
// stored), and one Machine can be replaced.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// The two files of a worker the operator's view reads. cloud-init writes both; kubeadm's output is in the second.
const (
	cloudInitResultPath = "/run/cloud-init/result.json"
	cloudInitOutputPath = "/var/log/cloud-init-output.log"
	// joinLogTailBytes and joinLogTailLines bound what of the log is answered: the end, where kubeadm's error is.
	joinLogTailBytes = 6000
	joinLogTailLines = 40
	// joinErrorMachines bounds the guest reads of one request: two calls to Proxmox per machine.
	// ponytail: the first five machines only; a page per machine if a cluster ever has more that did not join.
	joinErrorMachines = 5
	joinErrorTimeout  = 15 * time.Second
)

// joinSecrets are the credentials a failed join can print: the bootstrap token (it lets a machine join the cluster
// for as long as it is valid), the hash of the cluster CA that goes with it, a certificate key, a service account or
// bootstrap JWT. Order matters: the flag and key forms take the whole value before the bare forms see its parts.
var joinSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(--(?:token|tls-bootstrap-token|discovery-token|discovery-token-ca-cert-hash|certificate-key)[= ]+)\S+`),
	regexp.MustCompile(`(?i)((?:token|tlsBootstrapToken|caCertHashes|certificateKey|certificate-key|password|passwd|secret)"?\s*[:=]\s*(?:-\s*)?"?)[^\s",]+`),
	regexp.MustCompile(`()\b[a-z0-9]{6}\.[a-z0-9]{16}\b`),
	regexp.MustCompile(`()\bsha256:[A-Fa-f0-9]{64}\b`),
	regexp.MustCompile(`()\b[A-Fa-f0-9]{32,}\b`),
	regexp.MustCompile(`()\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
}

// maskJoinText takes the credentials out of a text read from a worker: the kubeadm forms above, then what every
// upstream message loses (machineMessage's rules, without its length cut).
func maskJoinText(text string) string {
	for _, re := range joinSecrets {
		text = re.ReplaceAllString(text, "${1}[MASKED]")
	}
	return machineText(text)
}

// joinError reads cloud-init's result and the end of its output log with read, and masks both. A file that cannot be
// read is left out; when neither can, ReadError says why in words that name no internal address.
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
		if len(log) > joinLogTailBytes {
			log = log[len(log)-joinLogTailBytes:]
			if i := strings.IndexByte(log, '\n'); i >= 0 {
				log = log[i+1:] // the first line is cut in two: a half token would not match the masks
			}
		}
		lines := strings.Split(strings.TrimRight(maskJoinText(log), "\n"), "\n")
		if len(lines) > joinLogTailLines {
			lines = lines[len(lines)-joinLogTailLines:]
		}
		out.LogTail = strings.Join(lines, "\n")
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
	if writeClusterErr(w, kamaji.ReplaceMachineError(err)) {
		return
	}
	httpx.OK(w, map[string]any{"result": "REPLACING"})
}
