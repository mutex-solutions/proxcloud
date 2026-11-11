package sandbox

import (
	"errors"
	"strings"
)

// Release gates of a sandbox provider (docs/managed-sandbox.md §3). The portal checks all three before a provider
// goes live (admin create or update with a status other than DISABLED) and again, cached for a minute, before every
// sandbox create: a gate that stops holding later stops new sandboxes.
const (
	// GateNodeRestriction: the sandbox cluster's API server runs NodeRestriction, so a kubelet cannot drop its
	// taints or claim node-restriction.kubernetes.io/ labels.
	GateNodeRestriction = "noderestriction"
	// GateIsolation: the cluster firewall holds the isolation DROPs for the sandbox network, both ways.
	GateIsolation = "isolation"
	// GateFence: every worker open to tenant pods carries the SB2b VM firewall, pinned to its own address.
	GateFence = "fence"
)

// Gate is one release gate's answer.
type Gate struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// GatesErr is nil when every gate holds, else one error naming each gate that does not.
func GatesErr(gates []Gate) error {
	var failed []string
	for _, g := range gates {
		if !g.OK {
			failed = append(failed, g.Name+": "+g.Detail)
		}
	}
	if len(gates) == 0 {
		failed = append(failed, "no release gate was checked")
	}
	if len(failed) == 0 {
		return nil
	}
	return errors.New("sandbox release gates do not hold: " + strings.Join(failed, "; "))
}
