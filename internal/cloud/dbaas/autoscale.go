package dbaas

// autoscale.go — the opt-in vertical autoscale leg, priced as a surcharge (billing attribute
// autoscale_enabled). Design: the VPA is the BRAIN (updateMode Off — it only recommends; a VPA
// that evicts database pods on its own schedule is a data-safety hazard), proxcloud is the HAND —
// each sync cycle reads the recommendation, clamps it into the customer's bounds and applies it
// through the SAME values patch RESIZE uses, so the operator performs its own safe rolling
// change and billing follows the declared size automatically (no declared-vs-actual drift).
// Disk rides the same tick off kubelet volume-stats ratios (Prometheus on the DB cluster):
// ≥80% full grows the volume by 20% (grow-only — PVC expansion cannot shrink). SCALE-UP ONLY
// by design: automatic shrink flaps and is where databases lose caches/data; scaling down
// stays a deliberate customer RESIZE.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// AutoscaleVPAName is the chart-rendered VerticalPodAutoscaler for a database (updateMode Off).
func AutoscaleVPAName(dbID string) string { return dbID + "-vpa" }

const (
	diskGrowThreshold = 0.80 // grow when any of the database's PVCs passes this fill ratio
	diskGrowFactor    = 1.20
)

// DiskUsageFunc returns fill ratios (used/capacity, 0..1) per PVC name for one namespace —
// resolved by the caller from the provider's Prometheus metrics config; nil = no disk leg.
type DiskUsageFunc func(ctx context.Context, ns string) (map[string]float64, error)

// AutoscaleTick walks the project's databases and applies at most one bounded scale-up per
// database per tick (CPU/RAM from the VPA recommendation, disk from volume fill). Returns how
// many databases were bumped. Best-effort per database — one failure must not starve the rest.
func (s *Service) AutoscaleTick(ctx context.Context, projectID string, diskUsage DiskUsageFunc) (int, error) {
	apps, err := s.api.ListApplications(ctx, s.cfg.ArgoNamespace,
		LabelProject+"="+projectID+","+LabelService+"="+s.serviceID+","+LabelManagedBy+"="+ManagedByValue)
	if err != nil {
		return 0, err
	}
	ns := NamespaceFor(projectID)
	bumped := 0
	var errs []error
	for _, app := range apps {
		if dig(app, "metadata", "deletionTimestamp") != nil {
			continue
		}
		dbID := digStr(app, "metadata", "name")
		values, _ := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
		auto, _ := values["autoscale"].(map[string]any)
		if enabled, _ := auto["enabled"].(bool); !enabled {
			continue
		}
		engine := digStr(values, "engine")
		caps := Capabilities[engine]

		newCPU, newMemGi := 0, 0
		if caps["RESIZE"] {
			var aerr error
			newCPU, newMemGi, aerr = s.vpaTarget(ctx, ns, dbID)
			if aerr != nil {
				errs = append(errs, fmt.Errorf("database %s: vpa: %w", dbID, aerr))
			}
		}
		newDiskGi := 0
		if caps["RESIZE_STORAGE"] && diskUsage != nil && intAt(auto, "maxStorageGi") > 0 {
			ratios, derr := diskUsage(ctx, ns)
			if derr != nil {
				errs = append(errs, fmt.Errorf("database %s: disk usage: %w", dbID, derr))
			} else {
				worst := 0.0
				for pvc, r := range ratios {
					// A database's PVCs carry its id in the name across all engines
					// (data-<id>-..., <id>-pool-..., <id>-pg-...).
					if strings.Contains(pvc, dbID) && r > worst {
						worst = r
					}
				}
				if worst >= diskGrowThreshold {
					newDiskGi = -1 // sentinel: grow by factor, resolved against live values below
				}
			}
		}
		if newCPU == 0 && newMemGi == 0 && newDiskGi == 0 {
			continue
		}

		changed := false
		perr := s.PatchDatabaseValues(ctx, dbID, func(values map[string]any) error {
			auto, _ := values["autoscale"].(map[string]any)
			res, _ := values["resources"].(map[string]any)
			if res != nil {
				curCPU, curMem := intAt(res, "cpu"), intAt(res, "memoryGi")
				// Clamp into [current, max]: the tick only ever moves UP, and never past the
				// customer's ceiling. Bounds are read from the LIVE values, same rule as every
				// other action.
				if maxC := intAt(auto, "maxCpu"); newCPU > curCPU && maxC > 0 {
					res["cpu"] = min(newCPU, maxC)
					changed = changed || min(newCPU, maxC) > curCPU
				}
				if maxM := intAt(auto, "maxMemoryGi"); newMemGi > curMem && maxM > 0 {
					res["memoryGi"] = min(newMemGi, maxM)
					changed = changed || min(newMemGi, maxM) > curMem
				}
			}
			if newDiskGi != 0 {
				storage, _ := values["storage"].(map[string]any)
				if storage != nil {
					cur := intAt(storage, "sizeGi")
					grown := int(math.Ceil(float64(cur) * diskGrowFactor))
					if maxS := intAt(auto, "maxStorageGi"); maxS > 0 && cur > 0 {
						next := min(grown, maxS)
						if next > cur {
							storage["sizeGi"] = next
							changed = true
						}
					}
				}
			}
			if !changed {
				return errAutoscaleNoop
			}
			return nil
		})
		if perr != nil && !errors.Is(perr, errAutoscaleNoop) {
			errs = append(errs, fmt.Errorf("database %s: autoscale patch: %w", dbID, perr))
			continue
		}
		if changed {
			bumped++
		}
	}
	return bumped, errors.Join(errs...)
}

// errAutoscaleNoop aborts the values patch when clamping left nothing to change.
var errAutoscaleNoop = errors.New("dbaas: autoscale no-op")

// vpaTarget reads the VPA recommendation (updateMode Off) and returns the ceil'd whole-unit
// targets — the LARGEST per-container recommendation, since the database container dominates.
// (0, 0, nil) when the VPA is absent (CRD not installed / not yet synced) or has no
// recommendation yet.
func (s *Service) vpaTarget(ctx context.Context, ns, dbID string) (cpu, memGi int, err error) {
	vpa, err := s.api.GetVPA(ctx, ns, AutoscaleVPAName(dbID))
	if err != nil || vpa == nil {
		return 0, 0, err
	}
	recs, _ := dig(vpa, "status", "recommendation", "containerRecommendations").([]any)
	for _, raw := range recs {
		rec, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		target, _ := rec["target"].(map[string]any)
		if c, perr := parseCPUQty(digStr(target, "cpu")); perr == nil && c > cpu {
			cpu = c
		}
		if m, perr := parseMemGiQty(digStr(target, "memory")); perr == nil && m > memGi {
			memGi = m
		}
	}
	return cpu, memGi, nil
}

// parseCPUQty parses a Kubernetes CPU quantity into whole cores, rounded UP ("1500m" → 2,
// "2" → 2) — reserving less than the recommendation would defeat the point.
func parseCPUQty(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	if milli, ok := strings.CutSuffix(q, "m"); ok {
		n, err := strconv.ParseFloat(milli, 64)
		if err != nil {
			return 0, err
		}
		return int(math.Ceil(n / 1000)), nil
	}
	n, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, err
	}
	return int(math.Ceil(n)), nil
}

// parseMemGiQty parses a Kubernetes memory quantity into whole Gi, rounded up ("1500Mi" → 2,
// "2Gi" → 2, plain bytes too).
func parseMemGiQty(q string) (int, error) {
	if q == "" {
		return 0, fmt.Errorf("empty quantity")
	}
	mult := 1.0
	for suffix, m := range map[string]float64{
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
	} {
		if rest, ok := strings.CutSuffix(q, suffix); ok {
			q, mult = rest, m
			break
		}
	}
	n, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, err
	}
	return int(math.Ceil(n * mult / float64(1<<30))), nil
}

// intAt reads an int-ish value out of a values map (JSON round-trips numbers as float64).
func intAt(m map[string]any, key string) int {
	switch n := m[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
