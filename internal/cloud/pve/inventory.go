package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func networkInventoryError(id, reason string, err error) error {
	return &cloud.InventoryUntrustedError{Type: cloud.TypeNetwork, ExternalID: id, Reason: reason, Err: err}
}

// VerifyInventory is a pre-write proof, not an empty-list heuristic. PVE returns
// 200 with only the vnets the token can see; a successful list is not a census.
// Check every would-be disappearance, including subnets of still-listed vnets.
func (p *NetworkSyncProvider) VerifyInventory(ctx context.Context, cached, listed []cloud.CloudResource) error {
	fresh := make(map[string]*cloud.CloudResource, len(listed))
	for i := range listed {
		cr := &listed[i]
		if cr.ProjectID != p.projectID || cr.ExternalID == "" {
			return networkInventoryError(cr.ExternalID, "invalid listed owner or identity", nil)
		}
		if _, dup := fresh[cr.ExternalID]; dup {
			return networkInventoryError(cr.ExternalID, "duplicate listed identity", nil)
		}
		fresh[cr.ExternalID] = cr
	}
	// Even an empty scope needs positive baseline visibility, not a successful
	// filtered list. No root ticket or alternate credential may stand in for it.
	have, err := p.svc.C.Permissions(ctx, "/sdn/zones")
	if err != nil || !have["SDN.Audit"] && !have["SDN.Allocate"] {
		return networkInventoryError("", "cannot prove configured token visibility on /sdn/zones", err)
	}
	// Cache checks only within this pass, never across calls/credential changes.
	checked := map[string]bool{"/sdn/zones": true}
	rights := func(zone, vnet string) error {
		if zone == "" || vnet == "" {
			return networkInventoryError(vnet, "cached or listed SDN zone is unknown", nil)
		}
		for _, path := range []string{"/sdn/zones", "/sdn/zones/" + zone, "/sdn/zones/" + zone + "/" + vnet} {
			if checked[path] {
				continue
			}
			have, err := p.svc.C.Permissions(ctx, path) // self; no userid/root credential
			if err != nil {
				return networkInventoryError(vnet, "cannot verify effective permissions on "+path, err)
			}
			if !have["SDN.Audit"] && !have["SDN.Allocate"] {
				return networkInventoryError(vnet, "configured token lacks SDN.Audit or SDN.Allocate on "+path, nil)
			}
			checked[path] = true
		}
		return nil
	}
	for i := range listed {
		if err := rights(networkZone(&listed[i]), listed[i].ExternalID); err != nil {
			return err
		}
	}
	for i := range cached {
		old := &cached[i]
		if old.ProjectID != p.projectID { // defense in depth: never validate another tenant's row
			return networkInventoryError(old.ExternalID, "cached owner is outside this project", nil)
		}
		cur := fresh[old.ExternalID]
		lost := networkCIDRsMissing(old, cur)
		if cur != nil && len(lost) == 0 {
			continue
		}
		zone := networkZone(old)
		if err := rights(zone, old.ExternalID); err != nil {
			return err
		}
		aclBefore, aclProofErr := p.svc.C.verifyGlobalSDNVisibility(ctx)
		live, err := p.svc.C.GetSDNVnet(ctx, old.ExternalID)
		if inventoryObjectAbsent(err, "vnet", old.ExternalID) && cur == nil {
			aclAfter, err := p.svc.C.verifyGlobalSDNVisibility(ctx)
			if aclProofErr != nil || err != nil || !slices.Equal(aclBefore, aclAfter) {
				return networkInventoryError(old.ExternalID, "cannot distinguish missing from a network moved into a hidden zone; complete stable SDN ACL visibility required", errors.Join(aclProofErr, err))
			}
			// Do not let an authorization-masked 404 stand in for a deletion.
			clear(checked)
			if err := rights(zone, old.ExternalID); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return networkInventoryError(old.ExternalID, "cannot revalidate omitted network", err)
		}
		if live == nil || live.Vnet != old.ExternalID || live.Zone == "" {
			return networkInventoryError(old.ExternalID, "incomplete by-id network response", nil)
		}
		clear(checked) // revalidate after the by-id read, including owner transfers
		if err := rights(live.Zone, live.Vnet); err != nil {
			return err
		}
		owner, _, owned := aliasProject(live.Alias)
		if !owned {
			return networkInventoryError(old.ExternalID, "live network owner is unknown", nil)
		}
		if cur == nil {
			if owner != p.projectID {
				continue // positively reassigned: archive only this project's old cache row
			}
			return networkInventoryError(old.ExternalID, "list omitted a live project-owned network", nil)
		}
		if owner != p.projectID || live.Zone != networkZone(cur) {
			return networkInventoryError(old.ExternalID, "network owner or zone changed during inventory read", nil)
		}
		if err := rights(live.Zone, live.Vnet); err != nil {
			return err
		}
		if zone != live.Zone {
			return networkInventoryError(old.ExternalID, "zone changed while subnets disappeared; retry after visibility is restored", nil)
		}
		for _, cidr := range lost {
			id := live.Zone + "-" + strings.ReplaceAll(cidr, "/", "-")
			aclBefore, aclProofErr := p.svc.C.verifyGlobalSDNVisibility(ctx)
			_, err := p.svc.C.GetSDNSubnet(ctx, old.ExternalID, id)
			if !inventoryObjectAbsent(err, "subnet", id) {
				return networkInventoryError(old.ExternalID, fmt.Sprintf("cannot prove subnet %s disappeared", cidr), err)
			}
			aclAfter, proofErr := p.svc.C.verifyGlobalSDNVisibility(ctx)
			if aclProofErr != nil || proofErr != nil || !slices.Equal(aclBefore, aclAfter) {
				return networkInventoryError(old.ExternalID, "cannot distinguish missing subnet from hidden reassignment", errors.Join(aclProofErr, proofErr))
			}
		}
		clear(checked)
		if err := rights(live.Zone, live.Vnet); err != nil {
			return err
		}
	}
	return nil
}

// PVE 9 can mask both absent and forbidden objects as a target-specific
// parameter400. Rights are proved separately before/after this result. Generic
// 500 "does not exist" errors (e.g. a missing zone/dependency) prove nothing.
func inventoryObjectAbsent(err error, kind, id string) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == http.StatusNotFound {
		return true
	}
	return ae.Status == http.StatusBadRequest && len(ae.Errors) == 1 && ae.Errors[kind] == "could not find "+kind+" '"+id+"'"
}

// verifyGlobalSDNVisibility proves there is no hidden destination for a moved
// vnet/subnet. Self Sys.Audit on /access makes ListACL COMPLETE (otherwise PVE
// filters it). Effective privileges at every ACL boundary avoid a role/group
// translator and catch NoAccess shadows, including for other users/groups.
// Require propagation for children not represented by an explicit ACL.
func (c *Client) verifyGlobalSDNVisibility(ctx context.Context) ([]ACLEntry, error) {
	audit, err := c.Permissions(ctx, "/access")
	if err != nil || !audit["Sys.Audit"] {
		return nil, fmt.Errorf("complete SDN ACL proof requires existing self Sys.Audit on /access (no automatic grant)")
	}
	before, err := c.ListACL(ctx)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{"/sdn/zones": true}
	for _, acl := range before {
		if strings.HasPrefix(acl.Path, "/sdn/zones/") {
			paths[acl.Path] = true
		}
	}
	for path := range paths {
		var out map[string]map[string]json.RawMessage
		if err := c.get(ctx, "/access/permissions", formVals{"path": path}.values(), &out); err != nil {
			return nil, err
		}
		propagates := func(priv string) bool { return string(out[path][priv]) == "1" || string(out[path][priv]) == "true" }
		if !propagates("SDN.Audit") && !propagates("SDN.Allocate") {
			return nil, fmt.Errorf("token cannot prove propagated SDN visibility at %s", path)
		}
	}
	after, err := c.ListACL(ctx)
	if err != nil {
		return nil, err
	}
	// ACL order is not stable (Perl hash iteration), so compare sorted records.
	key := func(a ACLEntry) string {
		return fmt.Sprintf("%s/%s/%s/%s/%d", a.Path, a.Type, a.UGID, a.RoleID, a.Propagate)
	}
	slices.SortFunc(before, func(a, b ACLEntry) int { return strings.Compare(key(a), key(b)) })
	slices.SortFunc(after, func(a, b ACLEntry) int { return strings.Compare(key(a), key(b)) })
	if !slices.Equal(before, after) {
		return nil, fmt.Errorf("SDN permissions changed during visibility proof")
	}
	audit, err = c.Permissions(ctx, "/access")
	if err != nil || !audit["Sys.Audit"] {
		return nil, fmt.Errorf("complete ACL visibility was revoked during proof")
	}
	return before, nil
}

func networkZone(cr *cloud.CloudResource) string {
	net, _ := cr.Data["network"].(map[string]any)
	zone, _ := net["zone"].(string)
	return zone
}

// NetworkCIDRs returns the canonical CIDRs of a cached or freshly listed network.
func NetworkCIDRs(cr *cloud.CloudResource) []string {
	if cr == nil {
		return nil
	}
	net, _ := cr.Data["network"].(map[string]any)
	var out []string
	if cidr, _ := net["cidr"].(string); cidr != "" {
		out = append(out, cidr)
	}
	// Stored JSON and fresh provider values both use []any; older direct callers
	// may use []string. Neither should hide a disappearing subnet.
	if subs, ok := net["subnets"].([]any); ok {
		for _, sub := range subs {
			if cidr, ok := sub.(string); ok && cidr != "" && !slices.Contains(out, cidr) {
				out = append(out, cidr)
			}
		}
	} else if subs, ok := net["subnets"].([]string); ok {
		for _, cidr := range subs {
			if cidr != "" && !slices.Contains(out, cidr) {
				out = append(out, cidr)
			}
		}
	}
	return out
}

func networkCIDRsMissing(old, cur *cloud.CloudResource) []string {
	now := NetworkCIDRs(cur)
	return slices.DeleteFunc(NetworkCIDRs(old), func(cidr string) bool { return slices.Contains(now, cidr) })
}
