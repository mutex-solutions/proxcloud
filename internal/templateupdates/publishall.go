package templateupdates

// publishall.go — "Publish on all nodes" (docs/template-updates.md): one artifact digest brought to
// published on every node of one provider, a step at a time. A site without shared storage needs a
// copy of a Kubernetes node image on every node, and each copy is imported, smoke-tested and
// published by itself.
//
// One advance publishes every node whose smoke test passed (through Publish, with all its gates) and
// then starts AT MOST ONE job, and only when no import and no smoke test of that provider is queued
// or running: several clones at once overload a small cluster. Nothing runs in the background: the
// caller advances again while the report says "advance".
//
// It never withdraws or removes tags, never imports where a copy of the digest exists, never repeats
// a failed import or smoke test, and never bypasses a publish gate.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Per-node states of a publish-all report.
const (
	NodePublished        = "published"
	NodeReadyToPublish   = "ready_to_publish" // smoke passed; the next advance publishes it
	NodePublishFailed    = "publish_failed"
	NodeSmokeRunning     = "smoke_running"
	NodeSmokeQueued      = "smoke_queued" // this advance queued it
	NodeStaged           = "staged"       // imported, waits its turn for a smoke test
	NodeSmokeNotStarted  = "smoke_not_started"
	NodeSmokeFailed      = "smoke_failed"
	NodeImporting        = "importing"
	NodeImportQueued     = "import_queued" // this advance queued it
	NodeImportFailed     = "import_failed"
	NodeMissing          = "missing" // no copy, waits its turn for an import
	NodePresentUnmanaged = "present_unmanaged"
	NodeSkipped          = "skipped"
)

// PublishAllInput is what the admin handler read from the provider.
type PublishAllInput struct {
	ServiceID     string
	Package       string
	Digest        string
	ConfirmDigest string
	// Nodes are the cluster's nodes; Options their active storages (PlacementOptions).
	Nodes   []pve.Node
	Options []PlacementOption
	// ImportStorage is the provider's importStorage; DiskStorage the default storage profile's
	// storage for a node. An import is started only where both are active (PlacementAllowed).
	ImportStorage string
	DiskStorage   func(node string) string
	// SmokeNet reads and checks the provider's smoke network for a node (AutoSmoke's netFor).
	SmokeNet func(ctx context.Context, serviceID, node string) (SmokeNetwork, error)
}

// PublishAllNode is one node of the report.
type PublishAllNode struct {
	Node    string `json:"node"`
	State   string `json:"state"`
	VMID    int    `json:"vmid,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`

	importID string
}

// PublishAllReport is the state of one digest on every node of one provider.
type PublishAllReport struct {
	Digest            string           `json:"digest"`
	KubernetesVersion string           `json:"kubernetesVersion,omitempty"`
	Nodes             []PublishAllNode `json:"nodes"`
	// VersionsLine (Kubernetes node image) is the config.cluster.versions entry of the published
	// copies, newest per node. It is display only: the operator copies it into the provider.
	VersionsLine string `json:"versionsLine,omitempty"`
	// Complete: every node is published or skipped for a reason other than being down, and NodesMissing is
	// empty. A node that is down may hold a published copy the versions line cannot show.
	Complete bool `json:"complete"`
	// NodesMissing hold a published node image of another Kubernetes version and none of this one:
	// a versions line without them moves a cluster's workers off those nodes.
	NodesMissing []string `json:"nodesMissing"`
	// Next: advance (calling again does something, also after an Error), wait (a job of the provider runs),
	// blocked (a step failed on a node and is never repeated here: that node's Message says what to do), done.
	Next string `json:"next"`
	// Actions is what this call changed (empty for a read).
	Actions []string `json:"actions,omitempty"`
	// Error is why an advance that had already changed something (Actions) could not start its next job.
	// The call still answers with the report, so what it did is shown and audited.
	Error *PublishAllError `json:"error,omitempty"`
}

// PublishAllError is the step of an advance that failed after an earlier one of the same call acted.
type PublishAllError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// smokeFailedHint ends the message of a smoke_failed node: the one step that helps is on another tab.
const smokeFailedHint = ". Not repeated here: run this copy's smoke test again from its row under Imported templates; when it passes, the next advance publishes it"

// NodeDownCode marks a node that was skipped because it is not online.
const NodeDownCode = "node_down"

// stop records err on the report of an advance that already acted. With nothing done yet the error is
// the call's answer, as before.
func (r *PublishAllReport) stop(err error) error {
	if len(r.Actions) == 0 {
		return err
	}
	r.Error = &PublishAllError{Code: "next_step_failed", Message: err.Error()}
	var le *LifecycleError
	var rl *RateLimitError
	switch {
	case errors.As(err, &le):
		r.Error.Code = le.Code
	case errors.As(err, &rl):
		r.Error.Code = "rate_limited"
	}
	return nil
}

// PublishAll reports the digest's state per node and, with act, advances it one step.
func (s *Service) PublishAll(ctx context.Context, pv LifecyclePVE, in PublishAllInput, actor string, act bool) (*PublishAllReport, error) {
	if !s.VMEnabled {
		return nil, errVMDisabled
	}
	if !digestRE.MatchString(in.Digest) || act && in.ConfirmDigest != in.Digest {
		return nil, lcErr(http.StatusBadRequest, "confirm_digest", "Confirm the exact digest to publish")
	}
	if b, err := s.Receipts.Blocked(ctx, in.Digest); err != nil {
		return nil, err
	} else if b != nil {
		return nil, ErrBlocklisted
	}
	if act {
		release, ok, err := s.DB.TryAdvisoryLock(ctx, "templateupdate-publish-all:"+in.ServiceID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, lcErr(http.StatusConflict, "busy", "Another publish on all nodes of this provider is running; try again")
		}
		defer release()
	}
	rows, err := pv.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, lcErr(http.StatusBadGateway, "pve_unreadable", "The provider's cluster could not be read; nothing was changed")
	}
	out := &PublishAllReport{Digest: in.Digest, Nodes: []PublishAllNode{}, NodesMissing: []string{}}
	var payload *VMPayload
	nodes := append([]pve.Node(nil), in.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })
	prov := map[string]ProviderRows{in.ServiceID: {Rows: rows}}
	for _, n := range nodes {
		recs, err := s.Receipts.ByKey(ctx, in.ServiceID, n.Node, in.Digest)
		if err != nil {
			return nil, err
		}
		for i := range recs {
			payload = &recs[i].Payload
		}
		out.Nodes = append(out.Nodes, s.publishAllNode(ctx, n, recs, prov, in))
	}
	if act {
		// 1. Publish what passed. One node's refusal is reported; the others go on.
		published := false
		for i := range out.Nodes {
			n := &out.Nodes[i]
			if n.State != NodeReadyToPublish {
				continue
			}
			if _, err := s.Publish(ctx, pv, n.importID, in.Digest, actor); err != nil {
				n.State, n.Code, n.Message = NodePublishFailed, "publish_failed", err.Error()
				var le *LifecycleError
				if errors.As(err, &le) {
					n.Code = le.Code
				}
				continue
			}
			n.State, published = NodePublished, true
			out.Actions = append(out.Actions, fmt.Sprintf("published %s/%d", n.Node, n.VMID))
		}
		if published {
			if rows, err = pv.ClusterResources(ctx, "vm"); err != nil {
				rows = nil // the line is left out of this answer; the next read has it
			}
		}
	}
	busy, err := s.providerBusy(ctx, in.ServiceID)
	if err != nil {
		// What step 1 published is reported and audited; no job is started on an unknown queue.
		if err = out.stop(err); err != nil {
			return nil, err
		}
	}
	if act && !busy && out.Error == nil {
		// 2. One mover: a smoke test of an import that has none, else the next import.
		for i := range out.Nodes {
			n := &out.Nodes[i]
			if n.State != NodeStaged {
				continue
			}
			if err := s.AutoSmoke(ctx, n.importID, in.SmokeNet); err != nil {
				n.State, n.Code, n.Message = NodeSmokeNotStarted, "smoke_not_started", err.Error()
				var le *LifecycleError
				if errors.As(err, &le) {
					n.Code = le.Code
				}
				continue
			}
			n.State, busy = NodeSmokeQueued, true
			out.Actions = append(out.Actions, fmt.Sprintf("smoke test queued %s/%d", n.Node, n.VMID))
			break
		}
		for i := range out.Nodes {
			n := &out.Nodes[i]
			if busy || n.State != NodeMissing {
				continue
			}
			rec, err := s.EnqueueVM(ctx, VMRequest{Package: in.Package, Digest: in.Digest, ConfirmDigest: in.Digest, ServiceID: in.ServiceID,
				Node: n.Node, ImportStorage: in.ImportStorage, DiskStorage: in.DiskStorage(n.Node)}, actor)
			if errors.Is(err, ErrDuplicate) {
				n.State, n.Message = NodeSkipped, "An earlier import of this artifact on this node blocks a new one"
				continue
			}
			if err != nil {
				// The artifact or the registry, not this node: no other node is tried.
				if err = out.stop(err); err != nil {
					return nil, err
				}
				break
			}
			payload = &rec.Payload
			n.State, n.importID, busy = NodeImportQueued, rec.ID, true
			out.Actions = append(out.Actions, "import queued "+n.Node)
		}
	}
	out.Complete = rows != nil
	for _, n := range out.Nodes {
		waits := n.State == NodeStaged || n.State == NodeMissing
		switch {
		case n.State == NodeReadyToPublish || waits && !busy:
			out.Next = "advance"
		case out.Next == "" && (waits || n.State == NodeSmokeRunning || n.State == NodeSmokeQueued || n.State == NodeImporting || n.State == NodeImportQueued):
			out.Next = "wait"
		}
		out.Complete = out.Complete && (n.State == NodePublished || n.State == NodeSkipped && n.Code != NodeDownCode)
	}
	if out.Next == "" {
		out.Next = "done"
		for _, n := range out.Nodes {
			// "done" with complete false read as finished. These four are never repeated by an advance.
			if n.State == NodeSmokeFailed || n.State == NodeSmokeNotStarted || n.State == NodeImportFailed || n.State == NodePublishFailed {
				out.Next = "blocked"
			}
		}
	}
	if payload != nil && payload.Variant == "kubernetes" && rows != nil {
		out.KubernetesVersion = payload.KubernetesVersion
		if v := versionsMapValue(rows, *payload); v != "" {
			out.VersionsLine = fmt.Sprintf("%q: %q", payload.KubernetesVersion, v)
		}
		out.NodesMissing = nodesMissingLine(rows, *payload)
		out.Complete = out.Complete && len(out.NodesMissing) == 0
	}
	return out, nil
}

// publishAllNode is one node's state before anything is changed.
func (s *Service) publishAllNode(ctx context.Context, node pve.Node, recs []Receipt, rows map[string]ProviderRows, in PublishAllInput) PublishAllNode {
	n := PublishAllNode{Node: node.Node}
	// The import that counts: one that made its template, else one in flight, else the newest failure.
	var best *TemplateState
	rank := func(st string) int {
		switch st {
		case StateImportFailed:
			return 1
		case StateImporting:
			return 2
		}
		return 3
	}
	for _, rec := range recs {
		if rec.TemplateGone {
			continue
		}
		st := s.stateOf(ctx, rec, rows, nil)
		if best == nil || rank(st.State) > rank(best.State) || rank(st.State) == rank(best.State) && st.ImportedAt.After(best.ImportedAt) {
			best = &st
		}
	}
	if best != nil && best.State != StateImportFailed {
		n.VMID, n.importID = best.VMID, best.ID
		switch best.State {
		case StatePublishedDefault, StatePublishedOlder:
			n.State = NodePublished
		case StateReady:
			n.State = NodeReadyToPublish
		case StateSmokeRunning:
			n.State = NodeSmokeRunning
		case StateSmokeFailed:
			n.State, n.Code, n.Message = NodeSmokeFailed, best.Smoke.Code, best.Smoke.Message+smokeFailedHint
		case StateImporting:
			n.State = NodeImporting
		case StateStaged:
			n.State = NodeStaged
			// A smoke test that could not be queued (AutoSmoke recorded why) is not tried again here.
			if best.Smoke != nil && best.Smoke.JobID == "" && best.Smoke.Code != "" {
				n.State, n.Code, n.Message = NodeSmokeNotStarted, best.Smoke.Code, best.Smoke.Message
			}
		case StateTemplateMissing:
			n.State, n.Message = NodeSkipped, "The template an earlier import made here is gone, and that import still blocks a new one"
		case StateProviderUnreadable:
			n.State, n.Code, n.Message = NodeSkipped, NodeDownCode, "The template's node is down; its copy cannot be read"
		default: // withdrawn copies
			n.State, n.Message = NodeSkipped, "This copy is tagged withdrawn"
		}
		return n
	}
	// No import of the portal holds a template here. A copy someone else made is never imported over.
	for _, r := range rows[in.ServiceID].Rows {
		if r.Node == node.Node && r.Type == "qemu" && r.Template == 1 && hasTagValue(r.Tags, TagSource+digestPrefix(in.Digest)) {
			n.State, n.VMID, n.Message = NodePresentUnmanaged, r.VMID, "A copy of this artifact that the portal did not import is on this node"
			return n
		}
	}
	if best != nil {
		n.State, n.Message = NodeImportFailed, "The import failed; start it again from Template updates"
		for _, r := range recs {
			if r.ID == best.ID && r.Code != "" {
				n.Code, n.Message = r.Code, r.Message
			}
		}
		return n
	}
	disk := ""
	if in.DiskStorage != nil {
		disk = in.DiskStorage(node.Node)
	}
	switch {
	case node.Status != "online":
		n.State, n.Code, n.Message = NodeSkipped, NodeDownCode, "The node is not online"
	case in.ImportStorage == "":
		n.State, n.Message = NodeSkipped, "The provider has no import storage (importStorage)"
	case disk == "":
		n.State, n.Message = NodeSkipped, "The default storage profile names no storage for this node"
	case !PlacementAllowed(in.Options, node.Node, in.ImportStorage, disk):
		n.State, n.Message = NodeSkipped, fmt.Sprintf("Import storage %s and disk storage %s are not both active on this node", in.ImportStorage, disk)
	default:
		n.State = NodeMissing
	}
	return n
}

// providerBusy reports whether an import or a smoke test of the provider is queued or running
// (whatever artifact it is of).
func (s *Service) providerBusy(ctx context.Context, serviceID string) (bool, error) {
	var imports []Receipt
	if err := s.DB.C(ReceiptCollection).Find(ctx, pgdoc.M{"payload.serviceId": serviceID}, &imports); err != nil {
		return false, err
	}
	for _, r := range imports {
		if r.Step == StepFailed || r.Step == StepSucceeded || r.Step == StepTemplated || r.Cleaned {
			continue
		}
		if st, _ := s.JobState(ctx, r.ID); st != "dead" {
			return true, nil
		}
	}
	var smokes []SmokeReceipt
	if err := s.DB.C(SmokeCollection).Find(ctx, pgdoc.M{"payload.serviceId": serviceID}, &smokes); err != nil {
		return false, err
	}
	for _, r := range smokes {
		if r.Step == SmokeFinished || r.Cleaned {
			continue
		}
		if st, _ := s.JobState(ctx, r.ID); st != "dead" {
			return true, nil
		}
	}
	return false, nil
}

// nodesMissingLine lists the nodes that publish a node image of another Kubernetes version of p's OS
// and none of p's version.
func nodesMissingLine(rows []pve.ClusterResource, p VMPayload) []string {
	has, other := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if servesLine(r, p) {
			has[r.Node] = true
		} else if servesAnyK8s(r, p.OS) {
			other[r.Node] = true
		}
	}
	out := []string{}
	for n := range other {
		if !has[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
