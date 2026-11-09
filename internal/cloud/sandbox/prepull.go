package sandbox

// Warming is intentionally best-effort. A finite digest/pool catalog is reconciled into bounded
// pull-only pods. Node image inventories, not old completed pods, determine current coverage.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const warmLabel = "proxcloud.io/sandbox-warm"
const warmIndex = "proxcloud-image-warming"

// This node label is applied by the operator ONLY after installing a runtime supporting the
// pull-only mode. Older sandbox-init binaries ignore -prepull and would initialise envd.
const LabelPrepullRuntime = "node-restriction.kubernetes.io/proxcloud-prepull-runtime"

type WarmConfig struct {
	Enabled      bool              `json:"enabled"`
	MaxImages    int               `json:"maxImages"`
	MaxImageGiB  int               `json:"maxImageGiB"`
	MaxTotalGiB  int               `json:"maxTotalGiB"`
	MaxNodes     int               `json:"maxNodes"`
	MaxParallel  int               `json:"maxParallel"`
	TTLSeconds   int               `json:"ttlSeconds"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

func (c WarmConfig) withDefaults() WarmConfig {
	for _, f := range []struct {
		p        *int
		def, cap int
	}{{&c.MaxImages, 3, 20}, {&c.MaxImageGiB, 15, 100}, {&c.MaxTotalGiB, 30, 500}, {&c.MaxNodes, 16, 128}, {&c.MaxParallel, 1, 4}, {&c.TTLSeconds, 86400, 604800}} {
		if *f.p == 0 {
			*f.p = f.def
		}
		*f.p = min(max(*f.p, 1), f.cap)
	}
	return c
}

type WarmCoverage struct {
	Enabled   bool      `json:"enabled"`
	Image     string    `json:"image"`
	Pool      string    `json:"pool"`
	Pulled    int       `json:"pulled"`
	Total     int       `json:"total"`
	Ready     bool      `json:"ready"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type warmEntry struct {
	ProjectID string
	Image     string
	GPU       string
	ExpiresAt time.Time
}

// Local serialization avoids a thundering herd; the resourceVersion lease below is the
// cluster-wide backstop across API replicas. The optional surface keeps non-warming fakes small.
var warmLocks sync.Map

type warmCASAPI interface {
	CreateCore(context.Context, string, map[string]any) error
	ReplaceCore(context.Context, string, map[string]any) error
}

func (s *Service) claimWarmLease(ctx context.Context) (func(), error) {
	api, ok := s.api.(warmCASAPI)
	if !ok {
		return func() {}, fmt.Errorf("warming requires Kubernetes conditional writes")
	}
	name := s.warmName() + "-lease"
	obj, err := s.api.GetCore(ctx, "configmaps", PrepullNamespace, name)
	if err != nil {
		return nil, err
	}
	token := newToken()
	if obj != nil {
		exp, _ := time.Parse(time.RFC3339Nano, digStr(obj, "data", "until"))
		if exp.After(time.Now()) {
			return nil, fmt.Errorf("image warming is busy; retry preparation shortly")
		}
	} else {
		obj = map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": PrepullNamespace}}
	}
	obj["data"] = map[string]any{"owner": token, "until": time.Now().Add(40 * time.Second).UTC().Format(time.RFC3339Nano)}
	if digStr(obj, "metadata", "resourceVersion") == "" {
		err = api.CreateCore(ctx, "configmaps", obj)
	} else {
		err = api.ReplaceCore(ctx, "configmaps", obj)
	}
	if err != nil {
		return nil, fmt.Errorf("image warming admission conflict; retry preparation: %w", err)
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cur, err := s.api.GetCore(cleanup, "configmaps", PrepullNamespace, name)
		if err != nil || digStr(cur, "data", "owner") != token {
			return
		}
		cur["data"] = map[string]any{"until": time.Now().UTC().Format(time.RFC3339Nano)}
		_ = api.ReplaceCore(cleanup, "configmaps", cur)
	}, nil
}
func (s *Service) warmLock() *sync.Mutex {
	m, _ := warmLocks.LoadOrStore(s.serviceID, &sync.Mutex{})
	return m.(*sync.Mutex)
}
func (s *Service) warmName() string { return warmIndex + "-" + warmKey(s.serviceID)[:12] }
func warmKey(s string) string       { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *Service) warmEntries(ctx context.Context) ([]warmEntry, error) {
	obj, err := s.api.GetCore(ctx, "configmaps", PrepullNamespace, s.warmName())
	if err != nil {
		return nil, err
	}
	var es []warmEntry
	if obj != nil {
		if digStr(obj, "metadata", "labels", LabelService) != s.serviceID || digStr(obj, "metadata", "labels", LabelManagedBy) != ManagedByValue {
			return nil, fmt.Errorf("warming catalog ownership mismatch")
		}
		if err = json.Unmarshal([]byte(digStr(obj, "data", "images")), &es); err != nil {
			return nil, err
		}
	}
	return es, nil
}
func (s *Service) saveWarmEntries(ctx context.Context, es []warmEntry) error {
	b, _ := json.Marshal(es)
	return s.api.ApplyCore(ctx, "configmaps", map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": s.warmName(), "namespace": PrepullNamespace, "labels": map[string]any{LabelManagedBy: ManagedByValue, LabelService: s.serviceID}}, "data": map[string]any{"images": string(b)}})
}

// PrepareImage admits only server-resolved, project-owned immutable template images. It never
// receives a credential, arbitrary registry or customer command. sizeBytes is verified compressed
// size when available; every entry still conservatively reserves MaxImageGiB (expansion unknown).
func (s *Service) PrepareImage(ctx context.Context, projectID string, t Template, sizeBytes int64) (*WarmCoverage, error) {
	out := &WarmCoverage{Enabled: s.cfg.Warming.Enabled, Image: t.Image, Pool: "cpu"}
	if t.GPU != nil {
		out.Pool = t.GPU.Model
	}
	if !out.Enabled {
		return out, nil
	}
	repo, d := splitRef(t.Image)
	if !s.importPolicy().ManagedRepository(repo, projectID) || len(d) != 71 || !strings.HasPrefix(d, "sha256:") {
		return nil, badBuild("warming requires a project-owned immutable template digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(d, "sha256:")); err != nil {
		return nil, badBuild("invalid warming digest")
	}
	c := s.cfg.Warming
	if sizeBytes < 0 || sizeBytes > int64(c.MaxImageGiB)<<30 {
		return nil, badBuild("image exceeds warming size budget")
	}
	mu := s.warmLock()
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.claimWarmLease(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	es, err := s.warmEntries(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	gpu := ""
	if t.GPU != nil {
		gpu = t.GPU.Model
	}
	kept := es[:0]
	for _, e := range es {
		if e.ExpiresAt.After(now) && !(e.Image == t.Image && e.GPU == gpu) {
			kept = append(kept, e)
		}
	}
	// Least-recently-prepared entries are evicted, never runtime images or containerd snapshots.
	capEntries := min(c.MaxImages, c.MaxTotalGiB/c.MaxImageGiB)
	if capEntries < 1 {
		return nil, badBuild("warming total budget cannot fit one image")
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].ExpiresAt.Before(kept[j].ExpiresAt) })
	for len(kept) >= capEntries {
		kept = kept[1:]
	}
	e := warmEntry{projectID, t.Image, gpu, now.Add(time.Duration(c.TTLSeconds) * time.Second)}
	kept = append(kept, e)
	if err = s.saveWarmEntries(ctx, kept); err != nil {
		return nil, err
	}
	if _, err = s.copyProjectPullSecret(ctx, projectID, PrepullNamespace, warmPullSecret(projectID)); err != nil {
		return nil, err
	}
	if err = s.reconcileWarming(ctx, kept); err != nil {
		return nil, err
	}
	return s.coverage(ctx, e)
}
func (s *Service) ImageWarmCoverage(ctx context.Context, projectID string, t Template) (*WarmCoverage, error) {
	if !s.cfg.Warming.Enabled {
		return &WarmCoverage{Enabled: false, Image: t.Image, Pool: "cpu"}, nil
	}
	es, err := s.warmEntries(ctx)
	if err != nil {
		return nil, err
	}
	gpu := ""
	if t.GPU != nil {
		gpu = t.GPU.Model
	}
	for _, e := range es {
		if e.ProjectID == projectID && e.Image == t.Image && e.GPU == gpu && e.ExpiresAt.After(s.now()) {
			return s.coverage(ctx, e)
		}
	}
	return &WarmCoverage{Enabled: s.cfg.Warming.Enabled, Image: t.Image, Pool: firstPool(gpu)}, nil
}
func firstPool(gpu string) string {
	if gpu == "" {
		return "cpu"
	}
	return gpu
}
func (s *Service) warmNode(n map[string]any, e warmEntry) bool {
	if digStr(n, "metadata", "labels", LabelPrepullRuntime) != "v1" {
		return false
	}
	if !conditionTrue(n, "Ready") || conditionTrue(n, "DiskPressure") || dig(n, "spec", "unschedulable") == true {
		return false
	}
	if e.GPU != "" {
		if digStr(n, "metadata", "labels", LabelGPU) != e.GPU || !nodeEligible(n) {
			return false
		}
	} else {
		taints, _ := dig(n, "spec", "taints").([]any)
		for _, v := range taints {
			m, _ := v.(map[string]any)
			if str(m["effect"]) != "PreferNoSchedule" {
				return false
			}
		}
	}
	for k, v := range s.cfg.Warming.NodeSelector {
		if digStr(n, "metadata", "labels", k) != v {
			return false
		}
	}
	return true
}
func cachedOn(n map[string]any, image string) bool {
	images, _ := dig(n, "status", "images").([]any)
	for _, im := range images {
		m, _ := im.(map[string]any)
		names, _ := m["names"].([]any)
		for _, name := range names {
			if str(name) == image {
				return true
			}
		}
	}
	return false
}
func (s *Service) coverage(ctx context.Context, e warmEntry) (*WarmCoverage, error) {
	ns, err := s.api.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	c := &WarmCoverage{Enabled: s.cfg.Warming.Enabled, Image: e.Image, Pool: firstPool(e.GPU), ExpiresAt: e.ExpiresAt}
	for _, n := range s.warmNodes(ns, e) {
		if s.warmNode(n, e) {
			c.Total++
			if cachedOn(n, e.Image) {
				c.Pulled++
			}
		}
	}
	c.Ready = c.Total > 0 && c.Pulled == c.Total
	return c, nil
}

// ReconcileWarming is called by the existing minutely template maintenance job. New nodes and
// images removed by kubelet GC are retried; expired catalog entries/pods are removed. Actual CRI
// cache GC remains kubelet-owned (no host socket, no dangerous image deletion by the API).
func (s *Service) ReconcileWarming(ctx context.Context) error {
	if !s.cfg.Warming.Enabled {
		return nil
	}
	mu := s.warmLock()
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := s.claimWarmLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	es, err := s.warmEntries(ctx)
	if err != nil {
		return err
	}
	kept := es[:0]
	for _, e := range es {
		if e.ExpiresAt.After(s.now()) {
			kept = append(kept, e)
		}
	}
	if err = s.saveWarmEntries(ctx, kept); err != nil {
		return err
	}
	return s.reconcileWarming(ctx, kept)
}
func (s *Service) reconcileWarming(ctx context.Context, es []warmEntry) error {
	ns, err := s.api.ListNodes(ctx)
	if err != nil {
		return err
	}
	pods, err := s.api.ListPods(ctx, PrepullNamespace, warmLabel+"=true,"+LabelService+"="+s.serviceID)
	if err != nil {
		return err
	}
	valid := map[string]bool{}
	for _, e := range es {
		valid[warmKey(e.Image+"/"+e.GPU)] = true
	}
	active := 0
	existing := map[string]bool{}
	for _, p := range pods {
		if digStr(p, "metadata", "labels", warmLabel) != "true" || digStr(p, "metadata", "labels", LabelManagedBy) != ManagedByValue || digStr(p, "metadata", "labels", LabelService) != s.serviceID {
			continue
		}
		name := digStr(p, "metadata", "name")
		key := digStr(p, "metadata", "annotations", "proxcloud.io/warm-key")
		phase := digStr(p, "status", "phase")
		if !valid[key] || phase == "Succeeded" || phase == "Failed" {
			if err = s.api.DeleteCore(ctx, "pods", PrepullNamespace, name); err != nil {
				return err
			}
			continue
		}
		existing[name] = true
		active++
	}
	for _, e := range es {
		for _, n := range s.warmNodes(ns, e) {
			if active >= s.cfg.Warming.MaxParallel {
				return nil
			}
			if !s.warmNode(n, e) || cachedOn(n, e.Image) {
				continue
			}
			node := digStr(n, "metadata", "name")
			name := "warm-" + warmKey(e.Image + "/" + e.GPU + "/" + node)[:40]
			if existing[name] {
				continue
			}
			if _, err = s.copyProjectPullSecret(ctx, e.ProjectID, PrepullNamespace, warmPullSecret(e.ProjectID)); err != nil {
				return err
			}
			if err = s.api.ApplyCore(ctx, "pods", s.warmPod(name, node, e)); err != nil {
				return err
			}
			active++
		}
	}
	return nil
}
func (s *Service) warmPod(name, node string, e warmEntry) map[string]any {
	res := map[string]any{"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "16Mi"}
	selector := map[string]any{}
	for k, v := range s.cfg.Warming.NodeSelector {
		selector[k] = v
	}
	selector[LabelPrepullRuntime] = "v1"
	if e.GPU != "" {
		selector[LabelGPU] = e.GPU
	}
	// Image pull bytes are CRI cache, not pod ephemeral storage. MaxTotalGiB is a conservative
	// admission reservation, NOT a kernel-enforced disk quota. Operators must retain GC headroom.
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name, "namespace": PrepullNamespace, "labels": map[string]any{warmLabel: "true", LabelManagedBy: ManagedByValue, LabelService: s.serviceID, LabelProject: e.ProjectID}, "annotations": map[string]any{"proxcloud.io/warm-key": warmKey(e.Image + "/" + e.GPU), "proxcloud.io/warm-image": e.Image}}, "spec": map[string]any{
		"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchFields": []any{map[string]any{"key": "metadata.name", "operator": "In", "values": []any{node}}}}}}}}, "tolerations": []any{map[string]any{"key": ResourceGPU, "operator": "Exists", "effect": "NoSchedule"}},
		"runtimeClassName": s.cfg.RuntimeClass, "restartPolicy": "Never", "activeDeadlineSeconds": int64(s.cfg.Startup.PullSeconds), "nodeSelector": selector,
		"automountServiceAccountToken": false, "enableServiceLinks": false, "terminationGracePeriodSeconds": int64(1),
		"imagePullSecrets": []any{map[string]any{"name": warmPullSecret(e.ProjectID)}},
		"containers":       []any{map[string]any{"name": "pull-only", "image": e.Image, "imagePullPolicy": "IfNotPresent", "command": []any{RuntimeMountPath + "/sandbox-init", "-prepull"}, "args": []any{}, "resources": map[string]any{"requests": res, "limits": res}, "securityContext": map[string]any{"runAsUser": int64(0), "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}, "volumeMounts": []any{map[string]any{"name": "runtime", "mountPath": RuntimeMountPath, "readOnly": true}}}},
		"volumes":          []any{map[string]any{"name": "runtime", "hostPath": map[string]any{"path": RuntimeHostDir, "type": "Directory"}}},
	}}
}

// WarmRegistryRefs keeps admitted digests alive through registry pruning until cache intent expires.
func (s *Service) WarmRegistryRefs(ctx context.Context) ([]string, error) {
	if !s.cfg.Warming.Enabled {
		return nil, nil
	}
	es, err := s.warmEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("warming registry roots: %w", err)
	}
	var out []string
	for _, e := range es {
		if e.ExpiresAt.After(s.now()) {
			out = append(out, e.Image)
		}
	}
	return out, nil
}

func warmPullSecret(project string) string { return "registry-warm-pull-" + project }

// warmNodes bounds fanout deterministically; coverage and reconciliation use the same set.
func (s *Service) warmNodes(nodes []map[string]any, e warmEntry) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if s.warmNode(n, e) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return digStr(out[i], "metadata", "name") < digStr(out[j], "metadata", "name") })
	if len(out) > s.cfg.Warming.MaxNodes {
		out = out[:s.cfg.Warming.MaxNodes]
	}
	return out
}

// WarmingAvailable is a capability check, not a promise that an image is cached.
func (s *Service) WarmingAvailable(ctx context.Context) (bool, error) {
	if !s.cfg.Warming.Enabled {
		return false, nil
	}
	if _, ok := s.api.(warmCASAPI); !ok {
		return false, nil
	}
	nodes, err := s.api.ListNodes(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if digStr(n, "metadata", "labels", LabelPrepullRuntime) == "v1" && conditionTrue(n, "Ready") {
			return true, nil
		}
	}
	return false, nil
}
