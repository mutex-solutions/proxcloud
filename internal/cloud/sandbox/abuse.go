package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AbusePolicy is a heuristic, not a proof of mining. Alert is the default; only the
// provider operator can opt into pause/kill. Missing telemetry never authorizes an action.
type AbusePolicy struct {
	Disabled      bool     `json:"disabled"`
	CPUPercent    int      `json:"cpuPercent"`
	WindowSeconds int      `json:"windowSeconds"`
	MaxGapSeconds int      `json:"maxGapSeconds"`
	TrafficBytes  int64    `json:"trafficBytes"`
	Action        string   `json:"action"`
	NotifyEmails  []string `json:"notifyEmails,omitempty"`
}

func (p AbusePolicy) WithDefaults() AbusePolicy {
	if p.TrafficBytes == 0 {
		p.TrafficBytes = 65536
	}
	if p.CPUPercent == 0 {
		p.CPUPercent = 90
	}
	if p.WindowSeconds == 0 {
		p.WindowSeconds = 900
	}
	if p.MaxGapSeconds == 0 {
		p.MaxGapSeconds = 90
	}
	if p.Action == "" {
		p.Action = "alert"
	}
	return p
}
func (p AbusePolicy) Validate() error {
	p = p.WithDefaults()
	if p.CPUPercent < 50 || p.CPUPercent > 100 || p.WindowSeconds < 60 || p.WindowSeconds > 86400 || p.MaxGapSeconds < 30 || p.MaxGapSeconds > 180 || p.TrafficBytes < 0 {
		return fmt.Errorf("sandbox.abuse: invalid threshold/window/gap/traffic")
	}
	if p.Action != "alert" && p.Action != "pause" && p.Action != "kill" {
		return fmt.Errorf("sandbox.abuse.action must be alert, pause or kill")
	}
	return nil
}

// Sample contains host-owned counters only. Identity joins a live, provider-owned
// Sandbox CR and its live pod UID. Counter rollback/restart starts a new window.
type Sample struct {
	ProjectID string    `json:"projectId"`
	SandboxID string    `json:"sandboxId"`
	CRUID     string    `json:"crUid"`
	PodUID    string    `json:"uid"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Node      string    `json:"node"`
	At        time.Time `json:"at"`
	CPUAt     time.Time `json:"cpuAt"`
	NetworkAt time.Time `json:"networkAt"`
	CPU       *uint64   `json:"cpuNanoseconds"`
	RX        *uint64   `json:"rxBytes"`
	TX        *uint64   `json:"txBytes"`
	Cores     int       `json:"cores"`
	LastExec  time.Time `json:"lastExec"`
}

// Observation is persisted, not process-local: restarts and multiple replicas do
// not shorten the window. Each new incident requires a fresh consecutive window.
type Observation struct {
	Previous Sample    `json:"previous"`
	Since    time.Time `json:"since"`
	Flagged  bool      `json:"flagged"`
	Policy   string    `json:"policy"`
}

func (o Observation) Next(s Sample, p AbusePolicy, now time.Time) (Observation, bool) {
	p = p.WithDefaults()
	policy, _ := json.Marshal(p)
	n := Observation{Previous: s, Policy: string(policy)}
	prev := o.Previous
	gap := s.At.Sub(prev.At)
	fresh := func(t time.Time) bool {
		return !t.IsZero() && !t.After(now.Add(5*time.Second)) && now.Sub(t) <= time.Duration(p.MaxGapSeconds)*time.Second
	}
	valid := s.CRUID != "" && s.PodUID != "" && s.CPU != nil && s.RX != nil && s.TX != nil && s.Cores > 0 && fresh(s.At) && fresh(s.CPUAt) && fresh(s.NetworkAt)
	if !valid || p.Disabled {
		return Observation{}, false
	}
	// Same collector sample delivered twice (manual triggers/replicas) adds no evidence.
	if s.At.Equal(prev.At) && s.PodUID == prev.PodUID && s.CRUID == prev.CRUID && o.Policy == n.Policy {
		return o, false
	}
	if o.Policy != n.Policy || s.CRUID != prev.CRUID || s.PodUID != prev.PodUID || s.ProjectID != prev.ProjectID || s.SandboxID != prev.SandboxID || s.Node != prev.Node || gap < 20*time.Second || gap > time.Duration(p.MaxGapSeconds)*time.Second || prev.CPU == nil || prev.RX == nil || prev.TX == nil || *s.CPU < *prev.CPU || *s.RX < *prev.RX || *s.TX < *prev.TX || !s.CPUAt.After(prev.CPUAt) || !s.NetworkAt.After(prev.NetworkAt) {
		return n, false
	}
	elapsed := s.CPUAt.Sub(prev.CPUAt).Seconds()
	if elapsed > float64(p.MaxGapSeconds) || s.NetworkAt.Sub(prev.NetworkAt) > time.Duration(p.MaxGapSeconds)*time.Second {
		return n, false
	}
	high := elapsed > 0 && float64(*s.CPU-*prev.CPU)/1e9/elapsed/float64(s.Cores)*100 >= float64(p.CPUPercent)
	active := s.LastExec.After(prev.At) || s.LastExec.After(now.Add(-time.Duration(p.WindowSeconds)*time.Second)) || (*s.RX-*prev.RX)+(*s.TX-*prev.TX) > uint64(p.TrafficBytes)
	if !high || active {
		return n, false
	}
	n.Since = o.Since
	if n.Since.IsZero() {
		n.Since = prev.At
	}
	n.Flagged = o.Flagged
	if !n.Flagged && s.At.Sub(n.Since) >= time.Duration(p.WindowSeconds)*time.Second {
		n.Flagged = true
		return n, true
	}
	return n, false
}

// StillInactive is the final destructive-action check. New traffic/native exec or
// missing/stale/recreated counters invalidate previously collected evidence.
func StillInactive(old, cur Sample, p AbusePolicy, now time.Time) bool {
	p = p.WithDefaults()
	if cur.PodUID != old.PodUID || cur.CRUID != old.CRUID || cur.Node != old.Node || cur.CPU == nil || cur.RX == nil || cur.TX == nil || old.RX == nil || old.TX == nil || now.Sub(cur.At) > time.Duration(p.MaxGapSeconds)*time.Second || cur.At.After(now.Add(5*time.Second)) {
		return false
	}
	if cur.LastExec.After(old.LastExec) || cur.LastExec.After(now.Add(-time.Duration(p.WindowSeconds)*time.Second)) {
		return false
	}
	if *cur.RX < *old.RX || *cur.TX < *old.TX {
		return false
	}
	return (*cur.RX-*old.RX)+(*cur.TX-*old.TX) <= uint64(p.TrafficBytes)
}

type telemetryAPI interface {
	ListConfigMaps(context.Context, string, string) ([]map[string]any, error)
}

const TelemetryNamespace = "sandbox-telemetry"
const AnnotationLastExec = "proxcloud.io/last-exec"

// AbuseSamples returns every owned running pod, including missing counters (which
// reset its persisted window). No /nodes/proxy or tenant-reported /proc data.
func (s *Service) AbuseSamples(ctx context.Context) ([]Sample, error) {
	api, ok := s.api.(telemetryAPI)
	if !ok {
		return nil, fmt.Errorf("sandbox telemetry reader unavailable")
	}
	cms, err := api.ListConfigMaps(ctx, TelemetryNamespace, "app.kubernetes.io/name=sandbox-telemetry")
	if err != nil {
		return nil, err
	}
	stats := map[string]Sample{}
	for _, cm := range cms {
		var xs []Sample
		if json.Unmarshal([]byte(digStr(cm, "data", "samples")), &xs) != nil {
			continue
		}
		for _, x := range xs {
			if digStr(cm, "metadata", "name") != "node-"+x.Node {
				continue
			}
			stats[x.PodUID] = x
		}
	}
	crs, err := s.api.ListCRsAllNamespaces(ctx, CRGroup, CRVersion, CRPlural, LabelManagedBy+"="+ManagedByValue+","+LabelService+"="+s.serviceID)
	if err != nil {
		return nil, err
	}
	pods, err := s.api.ListPodsAllNamespaces(ctx, LabelManagedBy+"="+ManagedByValue+","+LabelService+"="+s.serviceID)
	if err != nil {
		return nil, err
	}
	byID := map[string]map[string]any{}
	for _, cr := range crs {
		project := digStr(cr, "metadata", "labels", LabelProject)
		if s.owns(cr, project) && digStr(cr, "metadata", "namespace") == NamespaceFor(project) && dig(cr, "metadata", "deletionTimestamp") == nil {
			byID[project+"/"+digStr(cr, "metadata", "name")] = cr
		}
	}
	out := []Sample{}
	for _, pod := range pods {
		project := digStr(pod, "metadata", "labels", LabelProject)
		id := digStr(pod, "metadata", "labels", LabelSandboxID)
		cr := byID[project+"/"+id]
		if cr == nil || !ValidID(id) || digStr(pod, "metadata", "namespace") != NamespaceFor(project) || digStr(pod, "metadata", "name") != id || digStr(pod, "status", "phase") != "Running" || dig(pod, "metadata", "deletionTimestamp") != nil {
			continue
		}
		uid := digStr(pod, "metadata", "uid")
		x := stats[uid]
		if x.Namespace != NamespaceFor(project) || x.Name != id || x.Node != digStr(pod, "spec", "nodeName") || uid == "" || digStr(cr, "metadata", "uid") == "" {
			x = Sample{}
		}
		x.ProjectID = project
		x.SandboxID = id
		x.PodUID = uid
		x.CRUID = digStr(cr, "metadata", "uid")
		x.Cores = intOf(dig(firstContainer(cr), "resources", "limits", "cpu"))
		x.LastExec, _ = time.Parse(time.RFC3339Nano, digStr(cr, "metadata", "annotations", AnnotationLastExec))
		out = append(out, x)
	}
	return out, nil
}

// CheckIdentity revalidates live ownership immediately before an automatic action.
func (s *Service) CheckIdentity(ctx context.Context, x Sample) error {
	cr, err := s.get(ctx, x.ProjectID, x.SandboxID)
	if err != nil {
		return err
	}
	if x.CRUID == "" || x.PodUID == "" || digStr(cr, "metadata", "uid") != x.CRUID || dig(cr, "metadata", "deletionTimestamp") != nil || digStr(cr, "metadata", "annotations", AnnotationLastExec) != "" && digStr(cr, "metadata", "annotations", AnnotationLastExec) != x.LastExec.Format(time.RFC3339Nano) {
		return ErrNotFound
	}
	pod, err := s.api.GetCore(ctx, "pods", NamespaceFor(x.ProjectID), x.SandboxID)
	if err != nil {
		return err
	}
	if digStr(pod, "metadata", "uid") != x.PodUID || !s.owns(pod, x.ProjectID) || digStr(pod, "metadata", "labels", LabelSandboxID) != x.SandboxID || digStr(pod, "spec", "nodeName") != x.Node || digStr(pod, "status", "phase") != "Running" || dig(pod, "metadata", "deletionTimestamp") != nil {
		return ErrNotFound
	}
	return nil
}
