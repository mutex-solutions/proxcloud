package kamaji

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// progress.go tells a tenant why a cluster is not there yet. The status sync reads what Cluster API (Machine) and
// CAPMOX (ProxmoxMachine, ProxmoxCluster) report and stores, per unsettled node group, a reason CODE and the time of
// the group's last progress; the consoles own the texts. No upstream message is stored: those name PVE nodes, VMIDs
// and paths. "Needs attention" is a group or control plane that made no progress for a bounded time. It is computed
// again at every sync, so it clears by itself when the cluster recovers, and it never changes cluster.status.

// Reason codes of node_groups[i].progress.code and cluster.attention.code (docs/managed-k8s.md has the texts).
const (
	CodeProviderAccessRefused = "provider_access_refused"
	CodeNoCapacity            = "no_capacity"
	CodeVMCreateFailed        = "vm_create_failed"
	CodeVMCreateRetrying      = "vm_create_retrying"
	CodeWaitingControlPlane   = "waiting_control_plane"
	CodeCreatingVM            = "creating_vm"
	CodeStartingVM            = "starting_vm"
	CodeNotJoined             = "not_joined"
	CodeNodeNotReady          = "node_not_ready"
	CodeProvisioning          = "provisioning"
	CodeControlPlaneStalled   = "control_plane_stalled"
)

// How long each state may last without progress. Lab creates turned Healthy 9.5 to 16 min after the create, and the
// first node waits for the CNI, hence 20 min for a node that is not Ready. CAPMOX clones every machine of a cluster at
// once, so on slow disks each further machine in Provisioning adds to the bound.
// ponytail: fixed values; a provider setting if a slower site needs one.
const (
	boundDefault         = 15 * time.Minute
	boundNodeNotReady    = 20 * time.Minute
	boundProvisioning    = 30 * time.Minute
	boundPerProvisioning = 5 * time.Minute
	boundProvisioningMax = 90 * time.Minute
	boundProviderSilent  = 10 * time.Minute
)

const (
	capiGroup    = "cluster.x-k8s.io"
	capiVersion  = "v1beta2"
	infraGroup   = "infrastructure.cluster.x-k8s.io"
	infraVersion = "v1alpha2"

	labelClusterName   = "cluster.x-k8s.io/cluster-name"
	condVMProvisioned  = "VirtualMachineProvisioned"
	condProxmoxAvail   = "ProxmoxAvailable"
	phaseProvisioning  = "Provisioning"
	reasonNodeNotExist = "NodeDoesNotExist"
)

// How far a machine is, least advanced first.
const (
	rankMissing = iota
	rankPending
	rankProvisioning
	rankProvisioned
	rankNotReady
	rankReady
)

// MachineState is one machine's place in the build.
type MachineState struct {
	Code      string
	Rank      int
	Immediate bool      // needs attention now: CAPMOX gave up, no capacity, a refused token
	Waiting   bool      // waits for the control plane: never counts towards a stall
	Since     time.Time // the machine's last progress
}

func (m MachineState) ready() bool { return m.Rank == rankReady }

// pveAuthFailure tells a message that carries Proxmox's refusal of a token: "401 authentication failure" from the
// API, "not authorized to access endpoint" from the client CAPMOX uses. A bare 401 is not enough: VMIDs are numbers.
func pveAuthFailure(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "authentication failure") || strings.Contains(low, "not authorized") || strings.Contains(low, "401 unauthorized")
}

// noCapacity tells CAPMOX's refusal to place a machine: scheduler.InsufficientMemoryError at v0.9.2 reads "cannot
// reserve <n>B of memory on node <node>: <n>B available memory left" and ends the machine (VMProvisionFailed).
func noCapacity(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "cannot reserve") || strings.Contains(low, "insufficient memory")
}

func condOf(obj map[string]any, typ string) map[string]any {
	conds, _ := dig(obj, "status", "conditions").([]any)
	for _, raw := range conds {
		if c, _ := raw.(map[string]any); c["type"] == typ {
			return c
		}
	}
	return nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// machineFailure is the failure message Cluster API keeps on a Machine (v1beta2 moved it under deprecated).
func machineFailure(m map[string]any) string {
	if msg := digStr(m, "status", "failureMessage"); msg != "" {
		return msg
	}
	return digStr(m, "status", "deprecated", "v1beta1", "failureMessage")
}

// classifyMachine maps one Machine and its ProxmoxMachine (nil when CAPMOX has made none) to a reason code. First
// match wins; a message only ever selects a code.
func classifyMachine(m, pm map[string]any, cpReady bool) MachineState {
	phase := digStr(m, "status", "phase")
	st := MachineState{Rank: rankPending, Since: latest(parseTime(digStr(m, "metadata", "creationTimestamp")), parseTime(digStr(m, "status", "lastUpdated")))}
	switch phase {
	case phaseProvisioning:
		st.Rank = rankProvisioning
	case "Provisioned":
		st.Rank = rankProvisioned
	case "Running":
		st.Rank = rankNotReady
	}
	vm := condOf(pm, condVMProvisioned)
	vmFalse := vm["status"] == "False"
	reason, _ := vm["reason"].(string)
	vmMsg, _ := vm["message"].(string)
	failure := machineFailure(m)

	conds, _ := dig(pm, "status", "conditions").([]any)
	for _, raw := range conds {
		if c, _ := raw.(map[string]any); c["status"] != "True" {
			if msg, _ := c["message"].(string); pveAuthFailure(msg) {
				st.Code, st.Immediate = CodeProviderAccessRefused, true
				return st
			}
		}
	}
	switch {
	// Only the terminal refusal: a clone CAPMOX retries (CloningFailed) may also say "insufficient".
	case noCapacity(failure) || (vmFalse && reason == "VMProvisionFailed" && noCapacity(vmMsg)):
		st.Code, st.Immediate = CodeNoCapacity, true
	case (vmFalse && (reason == "VMProvisionFailed" || reason == "TaskFailed")) || phase == "Failed" || failure != "":
		st.Code, st.Immediate = CodeVMCreateFailed, true
	case vmFalse && (reason == "CloningFailed" || reason == "PoweringOnFailed"):
		st.Code = CodeVMCreateRetrying
	case phase == "Running":
		ready := condOf(m, "NodeReady")
		if ready["status"] == "True" {
			st.Rank = rankReady
			return st
		}
		st.Code = CodeNodeNotReady
		st.Since = latest(st.Since, parseTime(digStr(ready, "lastTransitionTime")))
	case st.Rank == rankPending && !cpReady:
		st.Code, st.Waiting = CodeWaitingControlPlane, true
	case vm == nil || (vmFalse && (reason == "Cloning" || reason == "WaitingForDiskReconciliation" ||
		reason == "WaitingForStaticIPAllocation" || reason == "WaitingForBootstrapDataReconciliation")):
		st.Code = CodeCreatingVM
	case vmFalse && (reason == "WaitingForVMPowerUp" || reason == "WaitingForCloudInit" ||
		reason == "WaitingForBootstrapReady" || reason == "WaitingForClusterAPIMachineAddresses"):
		st.Code = CodeStartingVM
	case phase == "Provisioned" || (condOf(m, "NodeHealthy")["reason"] == reasonNodeNotExist && condOf(m, "NodeHealthy")["status"] != "True"):
		st.Code = CodeNotJoined
	default:
		st.Code = CodeProvisioning
	}
	return st
}

// stallBound is how long a machine at rank may make no progress; provisioning counts the cluster's machines in
// Provisioning, which CAPMOX clones in parallel.
func stallBound(rank, provisioning int) time.Duration {
	switch rank {
	case rankProvisioning:
		return min(boundProvisioning+time.Duration(max(provisioning-1, 0))*boundPerProvisioning, boundProvisioningMax)
	case rankNotReady:
		return boundNodeNotReady
	}
	return boundDefault
}

func intAt(obj map[string]any, keys ...string) (int, bool) {
	switch n := dig(obj, keys...).(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// mdUnsettled: the MachineDeployment's counts differ from what it asks for. upToDateReplicas catches a rolling
// upgrade whose surge machine hangs while every old replica is still ready; absent, it counts as equal.
func mdUnsettled(md map[string]any) bool {
	want, _ := intAt(md, "spec", "replicas")
	have, _ := intAt(md, "status", "replicas")
	ready, _ := intAt(md, "status", "readyReplicas")
	if have != want || ready != want {
		return true
	}
	upToDate, ok := intAt(md, "status", "upToDateReplicas")
	return ok && upToDate != want
}

// groupProgress is one node group's progress entry, nil when the group is settled. states are its non-deleting
// machines, mds its MachineDeployments, desired its count in the values, appSyncedAt when ArgoCD last finished a sync
// (the clock of machines not made yet), provisioning the cluster's machines in Provisioning.
func groupProgress(states []MachineState, mds []map[string]any, desired int, appSyncedAt time.Time, provisioning int, now time.Time) map[string]any {
	unsettled := len(states) != desired
	var since time.Time
	for _, md := range mds {
		unsettled = unsettled || mdUnsettled(md)
	}
	for _, st := range states {
		unsettled = unsettled || !st.ready()
		since = latest(since, st.Since)
	}
	if !unsettled {
		return nil
	}
	if missing := desired - len(states); missing > 0 {
		since = latest(since, appSyncedAt)
		for _, md := range mds {
			since = latest(since, parseTime(digStr(md, "metadata", "creationTimestamp")))
		}
		for ; missing > 0; missing-- {
			states = append(states, MachineState{Code: CodeProvisioning, Rank: rankMissing, Since: since})
		}
	}
	// The worst machine speaks for the group: one that needs attention now, then the least advanced, then the oldest.
	var worst *MachineState
	for i := range states {
		st := &states[i]
		if st.ready() {
			continue
		}
		if worst == nil || (st.Immediate && !worst.Immediate) ||
			(st.Immediate == worst.Immediate && (st.Rank < worst.Rank || (st.Rank == worst.Rank && st.Since.Before(worst.Since)))) {
			worst = st
		}
	}
	if worst == nil {
		// Every machine is ready and the MachineDeployment has not caught up: nothing to tell.
		return nil
	}
	n := 0
	for _, st := range states {
		if !st.ready() && st.Code == worst.Code {
			n++
		}
	}
	attention := worst.Immediate || (!worst.Waiting && !since.IsZero() && now.Sub(since) > stallBound(worst.Rank, provisioning))
	out := map[string]any{"code": worst.Code, "machines": n, "attention": attention}
	if !since.IsZero() {
		out["since"] = since.UTC().Format(time.RFC3339)
	}
	return out
}

// clusterAttention is the cluster-level attention entry (nil when none): CAPMOX cannot use the Proxmox API, or the
// control plane never came up. c is the cached cluster payload, pc the cluster's ProxmoxCluster (nil when unread).
// A control plane stuck in a later upgrade is left out: nothing gives it a clock.
func clusterAttention(c, pc map[string]any, now time.Time) map[string]any {
	entry := func(code string, since time.Time) map[string]any {
		return map[string]any{"code": code, "since": since.UTC().Format(time.RFC3339)}
	}
	if pc != nil {
		made := parseTime(digStr(pc, "metadata", "creationTimestamp"))
		conds, _ := dig(pc, "status", "conditions").([]any)
		for _, raw := range conds {
			cond, _ := raw.(map[string]any)
			if msg, _ := cond["message"].(string); cond["status"] != "True" && pveAuthFailure(msg) {
				since := made
				if at, _ := cond["lastTransitionTime"].(string); at != "" {
					since = parseTime(at)
				}
				return entry(CodeProviderAccessRefused, since)
			}
		}
		// CAPMOX sets its first condition only once it opened the Proxmox API: a refused token can show as nothing.
		if condOf(pc, condProxmoxAvail) == nil && !made.IsZero() && now.Sub(made) > boundProviderSilent {
			return entry(CodeProviderAccessRefused, made)
		}
	}
	cp, _ := c["cp_status"].(string)
	created := parseTime(digStr(c, "created_at"))
	if (c["endpoint"] == nil || cp == "" || cp == phaseProvisioning) && !created.IsZero() && now.Sub(created) > boundDefault {
		return entry(CodeControlPlaneStalled, created)
	}
	return nil
}

// progressObjs is one namespace's Machines, ProxmoxMachines and ProxmoxClusters, read once per List.
type progressObjs struct {
	machines []map[string]any
	infra    map[string]map[string]any // ProxmoxMachine name → object
	clusters map[string]map[string]any // cluster name → ProxmoxCluster
	ok       bool                      // false: the service account may not read them (RBAC older than this release)
}

// readProgress lists the three kinds namespace-wide. A 403 or 404 (the RBAC of an older install, a missing CRD)
// leaves ok false and the fields out; any other error fails the sync, so the cache keeps the last row instead of
// dropping an attention flag on a transient API error.
func (s *Service) readProgress(ctx context.Context, ns string) (progressObjs, error) {
	out := progressObjs{infra: map[string]map[string]any{}, clusters: map[string]map[string]any{}}
	read := func(group, version, plural string) ([]map[string]any, bool, error) {
		items, err := s.api.ListCRs(ctx, group, version, plural, ns, "")
		var ae *kamajik8s.APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusNotFound) {
			return nil, false, nil
		}
		return items, err == nil, err
	}
	machines, ok, err := read(capiGroup, capiVersion, "machines")
	if !ok {
		return out, err
	}
	infra, ok, err := read(infraGroup, infraVersion, "proxmoxmachines")
	if !ok {
		return out, err
	}
	clusters, ok, err := read(infraGroup, infraVersion, "proxmoxclusters")
	if !ok {
		return out, err
	}
	out.machines, out.ok = machines, true
	for _, pm := range infra {
		out.infra[digStr(pm, "metadata", "name")] = pm
	}
	for _, pc := range clusters {
		name := digStr(pc, "metadata", "labels", labelClusterName)
		if name == "" {
			name = digStr(pc, "metadata", "name") // the chart names it after the cluster
		}
		out.clusters[name] = pc
	}
	return out, nil
}

// machinesOf is the cluster's Machines that are not being deleted: a machine on its way out (a scale-down, the old
// half of a rollout) is neither progress nor a stall.
func (o progressObjs) machinesOf(clusterID string) []map[string]any {
	var out []map[string]any
	for _, m := range o.machines {
		if digStr(m, "metadata", "labels", labelClusterName) == clusterID && dig(m, "metadata", "deletionTimestamp") == nil {
			out = append(out, m)
		}
	}
	return out
}

// needsProgress: is there anything to explain? Only then are the Machines read at all.
func needsProgress(c map[string]any, mds []map[string]any) bool {
	if c["endpoint"] == nil || c["cp_status"] != "Ready" {
		return true
	}
	have := map[string]int{}
	for _, md := range mds {
		if mdUnsettled(md) {
			return true
		}
		n, _ := intAt(md, "spec", "replicas")
		have[digStr(md, "metadata", "labels", labelNodeGroup)] += n
	}
	groups, _ := c["node_groups"].([]any)
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		name, _ := g["name"].(string)
		if have[name] != int(count(g["count"])) {
			return true
		}
	}
	return false
}

// addProgress writes node_groups[i].progress and cluster.attention into the cached payload c.
func addProgress(c, app map[string]any, mds []map[string]any, objs progressObjs, now time.Time) {
	id, _ := c["id"].(string)
	cpReady := c["endpoint"] != nil && c["cp_status"] == "Ready"
	states := map[string][]MachineState{}
	provisioning := 0
	for _, m := range objs.machinesOf(id) {
		if digStr(m, "status", "phase") == phaseProvisioning {
			provisioning++
		}
		group := digStr(m, "metadata", "labels", labelNodeGroup)
		states[group] = append(states[group], classifyMachine(m, objs.infra[digStr(m, "spec", "infrastructureRef", "name")], cpReady))
	}
	// The clock of a machine that is not made yet. Right after an edit ArgoCD's last sync is still the one before it
	// (or absent while the new one runs), so the time of the edit counts too: a scale-up is not stalled at once.
	synced := parseTime(digStr(app, "status", "operationState", "finishedAt"))
	if synced.IsZero() {
		synced = parseTime(digStr(app, "metadata", "creationTimestamp"))
	}
	synced = latest(synced, parseTime(digStr(app, "metadata", "annotations", AnnotationChangedAt)))
	byGroup := map[string][]map[string]any{}
	for _, md := range mds {
		name := digStr(md, "metadata", "labels", labelNodeGroup)
		byGroup[name] = append(byGroup[name], md)
	}
	groups, _ := c["node_groups"].([]any)
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		name, _ := g["name"].(string)
		if p := groupProgress(states[name], byGroup[name], int(count(g["count"])), synced, provisioning, now); p != nil {
			g["progress"] = p
		}
	}
	if a := clusterAttention(c, objs.clusters[id], now); a != nil {
		c["attention"] = a
	}
}

// Condition is one status condition as the operator sees it.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// MachineDetail is one worker Machine with what Cluster API and CAPMOX say about it: the operator's view of the code
// a tenant gets.
type MachineDetail struct {
	Name           string      `json:"name"`
	NodeGroup      string      `json:"nodeGroup"`
	Phase          string      `json:"phase"`
	LastUpdated    string      `json:"lastUpdated,omitempty"`
	NodeRef        string      `json:"nodeRef,omitempty"`
	VMID           int         `json:"vmid,omitempty"`
	ProxmoxNode    string      `json:"proxmoxNode,omitempty"`
	Addresses      []string    `json:"addresses"`
	Code           string      `json:"code,omitempty"`
	Attention      bool        `json:"attention"`
	Deleting       bool        `json:"deleting,omitempty"`
	FailureMessage string      `json:"failureMessage,omitempty"`
	Conditions     []Condition `json:"conditions"`
}

// ClusterMachines is the operator detail of one cluster.
type ClusterMachines struct {
	ProxmoxCluster struct {
		Conditions []Condition `json:"conditions"`
	} `json:"proxmoxCluster"`
	Machines []MachineDetail `json:"machines"`
}

func conditionsOf(objs ...map[string]any) []Condition {
	out := []Condition{}
	for _, obj := range objs {
		conds, _ := dig(obj, "status", "conditions").([]any)
		for _, raw := range conds {
			c, _ := raw.(map[string]any)
			out = append(out, Condition{Type: digStr(c, "type"), Status: digStr(c, "status"), Reason: digStr(c, "reason"),
				Message: digStr(c, "message"), LastTransitionTime: digStr(c, "lastTransitionTime")})
		}
	}
	return out
}

// ClusterMachines reads one cluster's Machines live, with full upstream messages. They name PVE nodes and VMIDs and
// may quote a credential: the caller is an operator route and redacts before it answers. A kamajik8s.APIError 403
// comes back as it is, so the route can say which RBAC to apply.
func (s *Service) ClusterMachines(ctx context.Context, clusterID string, now time.Time) (*ClusterMachines, error) {
	app, err := s.api.GetApplication(ctx, s.cfg.ArgoNamespace, clusterID)
	if err != nil {
		return nil, err
	}
	if app == nil || !managedBy(app) || digStr(app, "metadata", "labels", LabelService) != s.serviceID {
		return nil, ErrClusterNotFound
	}
	ns := NamespaceFor(digStr(app, "metadata", "labels", LabelProject))
	out := &ClusterMachines{Machines: []MachineDetail{}}
	machines, err := s.api.ListCRs(ctx, capiGroup, capiVersion, "machines", ns, "")
	if err != nil {
		return nil, err
	}
	infra, err := s.api.ListCRs(ctx, infraGroup, infraVersion, "proxmoxmachines", ns, "")
	if err != nil {
		return nil, err
	}
	clusters, err := s.api.ListCRs(ctx, infraGroup, infraVersion, "proxmoxclusters", ns, "")
	if err != nil {
		return nil, err
	}
	tcp, err := s.findTCP(ctx, ns, clusterID)
	if err != nil {
		return nil, err
	}
	cpReady := digStr(tcp, "status", "controlPlaneEndpoint") != "" && digStr(tcp, "status", "kubernetesResources", "version", "status") == "Ready"
	byName := map[string]map[string]any{}
	for _, pm := range infra {
		byName[digStr(pm, "metadata", "name")] = pm
	}
	for _, pc := range clusters {
		if digStr(pc, "metadata", "labels", labelClusterName) == clusterID || digStr(pc, "metadata", "name") == clusterID {
			out.ProxmoxCluster.Conditions = conditionsOf(pc)
		}
	}
	if out.ProxmoxCluster.Conditions == nil {
		out.ProxmoxCluster.Conditions = []Condition{}
	}
	provisioning := 0
	for _, m := range machines {
		if digStr(m, "metadata", "labels", labelClusterName) == clusterID && digStr(m, "status", "phase") == phaseProvisioning {
			provisioning++
		}
	}
	for _, m := range machines {
		if digStr(m, "metadata", "labels", labelClusterName) != clusterID {
			continue
		}
		pm := byName[digStr(m, "spec", "infrastructureRef", "name")]
		st := classifyMachine(m, pm, cpReady)
		d := MachineDetail{
			Name: digStr(m, "metadata", "name"), NodeGroup: digStr(m, "metadata", "labels", labelNodeGroup),
			Phase: digStr(m, "status", "phase"), LastUpdated: digStr(m, "status", "lastUpdated"),
			NodeRef: digStr(m, "status", "nodeRef", "name"), ProxmoxNode: digStr(pm, "status", "proxmoxNode"),
			Addresses: []string{}, FailureMessage: machineFailure(m), Conditions: conditionsOf(m, pm),
			Deleting: dig(m, "metadata", "deletionTimestamp") != nil,
		}
		d.VMID, _ = intAt(pm, "spec", "virtualMachineID")
		addrs, _ := dig(m, "status", "addresses").([]any)
		for _, raw := range addrs {
			if a, _ := raw.(map[string]any); digStr(a, "address") != "" {
				d.Addresses = append(d.Addresses, digStr(a, "address"))
			}
		}
		if !d.Deleting {
			d.Code = st.Code
			d.Attention = st.Immediate || (!st.ready() && !st.Waiting && now.Sub(st.Since) > stallBound(st.Rank, provisioning))
		}
		out.Machines = append(out.Machines, d)
	}
	return out, nil
}

// ErrClusterNotFound: no cluster of this provider has that id.
var ErrClusterNotFound = errors.New("kamaji: cluster not found")
