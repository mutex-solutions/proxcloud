package sandbox

// capacity.go — what the GPU pool can give a sandbox right now. A pod cannot span nodes, so the
// most GPUs one sandbox can get is the largest number of free GPUs on a single node that is
// Ready, schedulable and labelled for the model. Only nodes that exist count: a model with no
// such node offers 0 (the console greys it out; creates are refused with 409).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoGPUCapacity: the pool cannot fit the requested GPUs on one node right now.
var ErrNoGPUCapacity = errors.New("not enough free GPUs")

// GPUPool is one model's capacity. MaxPerSandbox is what a single sandbox can get now.
type GPUPool struct {
	Model         string `json:"model"`
	MaxPerSandbox int    `json:"maxPerSandbox"`
	Free          int    `json:"free"`
	Total         int    `json:"total"`
}

// gpuTolerated are the taints a sandbox GPU pod tolerates (spec.go).
var gpuTolerated = map[string]bool{ResourceGPU: true}

func nodeEligible(n map[string]any) bool {
	if dig(n, "spec", "unschedulable") == true || !conditionTrue(n, "Ready") {
		return false
	}
	taints, _ := dig(n, "spec", "taints").([]any)
	for _, t := range taints {
		m, _ := t.(map[string]any)
		if str(m["effect"]) != "PreferNoSchedule" && !gpuTolerated[str(m["key"])] {
			return false
		}
	}
	return true
}

// podGPUs is the GPUs a pod holds: its containers' requests, or its largest init container's
// if that is more (init containers run one at a time).
func podGPUs(p map[string]any) int {
	req := func(c map[string]any) int {
		if n := intOf(dig(c, "resources", "requests", ResourceGPU)); n > 0 {
			return n
		}
		return intOf(dig(c, "resources", "limits", ResourceGPU))
	}
	sum, initMax := 0, 0
	cs, _ := dig(p, "spec", "containers").([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		sum += req(m)
	}
	ics, _ := dig(p, "spec", "initContainers").([]any)
	for _, c := range ics {
		m, _ := c.(map[string]any)
		initMax = max(initMax, req(m))
	}
	return max(sum, initMax)
}

// unschedulable: the scheduler has given up on placing the pod (e.g. it asks for more CPU than a
// GPU node has). Such a pod holds no GPU and must not block the model for everyone else.
func unschedulable(p map[string]any) bool {
	conds, _ := dig(p, "status", "conditions").([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if str(m["type"]) == "PodScheduled" && str(m["status"]) == "False" && str(m["reason"]) == "Unschedulable" {
			return true
		}
	}
	return false
}

// poolGPUCapacity computes per-model capacity from the cluster's nodes and pods.
func poolGPUCapacity(models []string, nodes, pods []map[string]any) []GPUPool {
	type nodeCap struct {
		model     string
		free, all int
	}
	byNode := map[string]*nodeCap{}
	for _, n := range nodes {
		model := digStr(n, "metadata", "labels", LabelGPU)
		all := intOf(dig(n, "status", "allocatable", ResourceGPU))
		if model == "" || all <= 0 || !nodeEligible(n) {
			continue
		}
		byNode[digStr(n, "metadata", "name")] = &nodeCap{model: model, free: all, all: all}
	}
	unplaced := map[string]int{}
	for _, p := range pods {
		switch digStr(p, "status", "phase") {
		case "Succeeded", "Failed":
			continue
		}
		if dig(p, "metadata", "deletionTimestamp") != nil {
			continue // going away (a pause or delete): its GPU is about to be free
		}
		g := podGPUs(p)
		if g == 0 {
			continue
		}
		if nc := byNode[digStr(p, "spec", "nodeName")]; nc != nil {
			nc.free -= g
		} else if digStr(p, "spec", "nodeName") == "" && !unschedulable(p) {
			// ponytail: a pending GPU pod is charged to its model's total, not a node; exact while
			// nodes hold 1 GPU, needs bin-packing once nodes have more.
			unplaced[digStr(p, "spec", "nodeSelector", LabelGPU)] += g
		}
	}
	out := make([]GPUPool, 0, len(models))
	for _, m := range models {
		pool := GPUPool{Model: m}
		for _, nc := range byNode {
			if nc.model != m {
				continue
			}
			free := max(nc.free, 0)
			pool.Total += nc.all
			pool.Free += free
			pool.MaxPerSandbox = max(pool.MaxPerSandbox, free)
		}
		pool.Free = max(pool.Free-unplaced[m], 0)
		pool.MaxPerSandbox = min(pool.MaxPerSandbox, pool.Free)
		out = append(out, pool)
	}
	return out
}

// PoolGPUCapacity reads the cluster (uncached) for the configured models.
func (s *Service) PoolGPUCapacity(ctx context.Context) ([]GPUPool, error) {
	return s.poolFor(ctx, s.cfg.GPUModels)
}

func (s *Service) poolFor(ctx context.Context, models []string) ([]GPUPool, error) {
	nodes, err := s.api.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := s.api.ListPodsAllNamespaces(ctx, "")
	if err != nil {
		return nil, err
	}
	return poolGPUCapacity(models, nodes, pods), nil
}

const gpuCapacityTTL = 5 * time.Second

type cachedPools struct {
	at    time.Time
	pools []GPUPool
}

var gpuPoolCache sync.Map // serviceID → cachedPools

// GPUCapacity is what a sandbox of this project can get per model now: the pool's per-sandbox
// maximum, clamped by the provider's per-sandbox limit and the project's remaining GPU quota.
// The pool read is cached for a few seconds (the console polls while its dialog is open).
func (s *Service) GPUCapacity(ctx context.Context, projectID string) (pools []GPUPool, projectLimitReached bool, err error) {
	var all []GPUPool
	if c, ok := gpuPoolCache.Load(s.serviceID); ok && time.Since(c.(cachedPools).at) < gpuCapacityTTL {
		all = c.(cachedPools).pools
	} else {
		if all, err = s.PoolGPUCapacity(ctx); err != nil {
			return nil, false, err
		}
		gpuPoolCache.Store(s.serviceID, cachedPools{at: time.Now(), pools: all})
	}
	live, err := s.live(ctx, projectID)
	if err != nil {
		return nil, false, err
	}
	held := 0
	for _, it := range live {
		held += gpuCount(it)
	}
	l := s.cfg.WithDefaults().Limits
	left := max(l.MaxConcurrentGPUs-held, 0)
	out := make([]GPUPool, len(all))
	for i, p := range all {
		p.MaxPerSandbox = min(p.MaxPerSandbox, l.MaxGPUs, left)
		out[i] = p
	}
	return out, left == 0, nil
}

// checkGPUCapacity refuses a request the pool cannot place now (uncached). Concurrent requests
// (several forks at once, two users at the same moment) may all pass; the extra pods then wait
// Pending as before. A cluster read error
// lets the create go on: the pod then waits as Pending, as before this check existed.
func (s *Service) checkGPUCapacity(ctx context.Context, g *GPURequest) error {
	if g == nil || g.Count == 0 {
		return nil
	}
	// The requested model, configured or not: a paused sandbox may use a model since dropped
	// from the offer.
	pools, err := s.poolFor(ctx, []string{g.Model})
	if err != nil {
		return nil
	}
	for _, p := range pools {
		if p.Model == g.Model && p.MaxPerSandbox >= g.Count {
			return nil
		}
		if p.Model == g.Model {
			return fmt.Errorf("%w: %d× %s requested, %d available on one node right now", ErrNoGPUCapacity, g.Count, g.Model, p.MaxPerSandbox)
		}
	}
	return fmt.Errorf("%w: no %s nodes are available", ErrNoGPUCapacity, g.Model)
}
