package sandboxproxy

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// Pod is what the proxy needs to know about a sandbox pod.
type Pod struct {
	IP           string
	TrafficToken string // empty = no token required
}

// ErrNotFound means no running pod carries the sandbox id.
var ErrNotFound = errors.New("sandbox not found")

// ErrNotReady means the sandbox's pod exists but is still starting (or already stopping). It is
// not cached: the next request looks again.
var ErrNotReady = errors.New("sandbox not ready")

// Lookup resolves a sandbox id to its pod.
type Lookup func(ctx context.Context, id string) (Pod, error)

// PodLister is the slice of kamajik8s.Client the proxy uses.
type PodLister interface {
	ListPodsAllNamespaces(ctx context.Context, labelSelector string) ([]map[string]any, error)
}

// K8sLookup finds the ready pod with a podIP for a sandbox id: Running, Ready (its startup probe
// passed: envd answers and the template's ready command succeeded) and not being deleted, so
// traffic never reaches a pod that is still booting or already going away.
func K8sLookup(c PodLister) Lookup {
	return func(ctx context.Context, id string) (Pod, error) {
		items, err := c.ListPodsAllNamespaces(ctx, sandbox.LabelManagedBy+"="+sandbox.ManagedByValue+","+sandbox.LabelSandboxID+"="+id)
		if err != nil {
			return Pod{}, err
		}
		starting := false
		for _, it := range items {
			st, _ := it["status"].(map[string]any)
			ip, _ := st["podIP"].(string)
			md, _ := it["metadata"].(map[string]any)
			if phase, _ := st["phase"].(string); phase != "Running" || ip == "" || md["deletionTimestamp"] != nil || !podReady(st) {
				if phase, _ := st["phase"].(string); phase == "Pending" || phase == "Running" {
					starting = true
				}
				continue
			}
			ann, _ := md["annotations"].(map[string]any)
			tok, _ := ann[sandbox.AnnotationTrafficToken].(string)
			return Pod{IP: ip, TrafficToken: tok}, nil
		}
		if starting {
			return Pod{}, ErrNotReady
		}
		return Pod{}, ErrNotFound
	}
}

func podReady(status map[string]any) bool {
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}

type cacheEntry struct {
	pod     Pod
	missing bool
	expires time.Time
}

// Cache memoises a Lookup: hits for hitTTL, ErrNotFound misses for missTTL. Other errors are
// not cached.
type Cache struct {
	next            Lookup
	hitTTL, missTTL time.Duration
	now             func() time.Time
	mu              sync.Mutex
	m               map[string]cacheEntry
}

// NewCache wraps next with separate hit and miss TTLs.
func NewCache(next Lookup, hitTTL, missTTL time.Duration) *Cache {
	return &Cache{next: next, hitTTL: hitTTL, missTTL: missTTL, now: time.Now, m: map[string]cacheEntry{}}
}

// Forget drops the cached answer for id.
func (c *Cache) Forget(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// Lookup implements Lookup.
func (c *Cache) Lookup(ctx context.Context, id string) (Pod, error) {
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok && c.now().Before(e.expires) {
		if e.missing {
			return Pod{}, ErrNotFound
		}
		return e.pod, nil
	}
	pod, err := c.next(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Pod{}, err
	}
	ne := cacheEntry{pod: pod, missing: err != nil}
	if err != nil {
		ne.expires = c.now().Add(c.missTTL)
	} else {
		ne.expires = c.now().Add(c.hitTTL)
	}
	c.mu.Lock()
	if len(c.m) > 4096 { // ponytail: wholesale reset bounds memory; use LRU if churn matters
		c.m = map[string]cacheEntry{}
	}
	c.m[id] = ne
	c.mu.Unlock()
	return pod, err
}
