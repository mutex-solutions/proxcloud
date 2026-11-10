package kamaji

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// GatewayFabricChartVersion is the first cluster chart carrying the Gateway API Standard bundle
// and the NGINX Gateway Fabric controller. Old Applications retain their chart and ingress values.
const GatewayFabricChartVersion = "0.1.10"
const GatewayFabricAddon = "nginxGatewayFabric"
const LegacyIngressAddon = "ingress"

// ChartHasGatewayFabric follows the existing CSI chart-capability convention: prerelease/build
// suffixes are accepted for separately pinned operator builds, never inferred from "latest".
func ChartHasGatewayFabric(v string) bool {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	var have [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return false
		}
		have[i] = n
	}
	return have[0] > 0 || have[1] > 1 || have[1] == 1 && have[2] >= 10
}

// ValidateCreateAddons runs before any namespace, VM, token or address reservation. A false legacy
// pick from an older client is inert, but new clusters cannot install the retired ingress controller.
func ValidateCreateAddons(chartVersion string, picks map[string]bool) error {
	for name, enabled := range picks {
		if name == LegacyIngressAddon {
			if enabled {
				return fmt.Errorf("ingress-nginx is retired; select NGINX Gateway Fabric for a new cluster")
			}
			continue
		}
		if _, ok := ClusterAddons[name]; !ok {
			return fmt.Errorf("unknown add-on %q", name)
		}
		if name == GatewayFabricAddon && enabled && !ChartHasGatewayFabric(chartVersion) {
			return fmt.Errorf("NGINX Gateway Fabric requires platform chart %s or later; this provider pins %s", GatewayFabricChartVersion, chartVersion)
		}
	}
	return nil
}

// ValidateGatewayFabricKubernetes enforces the pinned controller's supported Kubernetes floor.
func ValidateGatewayFabricKubernetes(version string, picks map[string]bool) error {
	if !picks[GatewayFabricAddon] {
		return nil
	}
	major, minor, _, err := ParseVersion(version)
	if err != nil || major != 1 || minor < 32 {
		return fmt.Errorf("NGINX Gateway Fabric requires Kubernetes 1.32 or later in the supported 1.x series")
	}
	return nil
}

func addonEnabled(current map[string]any, key string) (enabled, exists bool, err error) {
	raw, exists := current[key]
	if !exists {
		return false, false, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return false, true, fmt.Errorf("add-on %q has malformed stored values; operator reconciliation required", key)
	}
	enabled, ok = m["enabled"].(bool)
	if !ok {
		return false, true, fmt.Errorf("add-on %q has no boolean stored enabled value; operator reconciliation required", key)
	}
	return enabled, true, nil
}

// MergeAddonValues merges only curated boolean choices. Omission preserves existing choices so an
// older client cannot erase a newer controller during an unrelated edit. The legacy ingress block
// is retained verbatim; ordinary edits cannot enable, disable or silently migrate it. Operator-only
// and obsolete credential-push values are deliberately not copied into the customer block.
func MergeAddonValues(chartVersion string, current map[string]any, picks map[string]bool) (map[string]any, error) {
	for name := range picks {
		if _, ok := ClusterAddons[name]; !ok && name != LegacyIngressAddon {
			return nil, fmt.Errorf("unknown add-on %q", name)
		}
	}
	legacy, legacyExists, err := addonEnabled(current, LegacyIngressAddon)
	if err != nil {
		return nil, err
	}
	if requested, sent := picks[LegacyIngressAddon]; sent && requested != legacy {
		return nil, fmt.Errorf("legacy ingress-nginx changes require an explicit operator traffic migration; ordinary add-on edits preserve it")
	}
	out := map[string]any{}
	for name, fallback := range ClusterAddons {
		enabled, exists, err := addonEnabled(current, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			enabled = fallback
		}
		if requested, sent := picks[name]; sent {
			enabled = requested
		}
		if name == GatewayFabricAddon {
			if enabled && legacy {
				return nil, fmt.Errorf("legacy ingress-nginx is still enabled; migrate traffic explicitly before selecting NGINX Gateway Fabric")
			}
			if enabled && !ChartHasGatewayFabric(chartVersion) {
				return nil, fmt.Errorf("NGINX Gateway Fabric requires platform chart %s or later; this cluster pins %s", GatewayFabricChartVersion, chartVersion)
			}
			// Do not introduce an unsupported key into an older pinned chart.
			if !enabled && !ChartHasGatewayFabric(chartVersion) {
				continue
			}
		}
		out[name] = map[string]any{"enabled": enabled}
	}
	if legacyExists {
		out[LegacyIngressAddon] = current[LegacyIngressAddon]
	}
	return out, nil
}

// PatchAddons validates the actual live Application pin and picks in the same read/mutate closure.
// It cannot silently re-pin an old cluster or delete a legacy ingress release.
func validateAddonChartTransition(app map[string]any, target string) error {
	raw := dig(app, "spec", "source", "helm", "valuesObject", "addons")
	if raw == nil {
		return nil
	}
	adds, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("kamaji: malformed add-on block; operator reconciliation required")
	}
	enabled, _, err := addonEnabled(adds, GatewayFabricAddon)
	if err != nil {
		return err
	}
	if enabled && !ChartHasGatewayFabric(target) {
		return fmt.Errorf("kamaji: cannot move a Gateway Fabric cluster below chart %s", GatewayFabricChartVersion)
	}
	return nil
}

func (s *Service) PatchAddons(ctx context.Context, clusterID string, picks map[string]bool) error {
	return s.PatchClusterApp(ctx, clusterID, func(app map[string]any) error {
		values, ok := dig(app, "spec", "source", "helm", "valuesObject").(map[string]any)
		if !ok {
			return fmt.Errorf("kamaji: cluster %s: application carries no values", clusterID)
		}
		var current map[string]any
		if raw, exists := values["addons"]; exists {
			current, ok = raw.(map[string]any)
			if !ok {
				return fmt.Errorf("kamaji: cluster %s: malformed add-on block", clusterID)
			}
		}
		block, err := MergeAddonValues(digStr(app, "spec", "source", "targetRevision"), current, picks)
		if err != nil {
			return httpx.BadRequest(err.Error())
		}
		enabled, _, err := addonEnabled(block, GatewayFabricAddon)
		if err != nil {
			return httpx.BadRequest(err.Error())
		}
		if err := ValidateGatewayFabricKubernetes(digStr(values, "kubernetesVersion"), map[string]bool{GatewayFabricAddon: enabled}); err != nil {
			return httpx.BadRequest(err.Error())
		}
		values["addons"] = block
		return nil
	})
}
