package billingresource

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// SandboxProvider maps a SANDBOX → a "sandbox" BillingResource. Like database_cluster it carries
// the full price: the sandbox runs on platform-owned nodes and nothing in a customer project bills it.
// Price it with `minute` rules on vcpus / ram_gb / gpu_count (filter gpu_model for per-model GPU
// prices).
//
// Billing starts when the sandbox became RUNNING (data.runningSince), never at create: the
// wait for an image pull or for a GPU of the model to free up is not the customer's cost.
// Mechanism, because the engine's own rules would bill that wait:
//   - the engine creates a bill item even for a NotEligibleForBilling resource, with its
//     minute watermark at the resource's createdAt, and later back-bills from there. So while
//     PENDING this provider returns NO billing resource at all (no item, no watermark);
//   - once RUNNING, cr.Info.CreatedAt is set to runningSince, cut to its minute. stampResourceValues prefers
//     Info.CreatedAt, so the item is created with its watermark at the minute the sandbox started.
//
// End of life: an ended sandbox's row is DELETED with its end (sandbox.MarkVanished, or the delete) and archived; the
// charge rates archived rows too, up to DeletedAt = that minute plus the runtime rounded UP to whole minutes, at least
// one. The engine bills every clock minute an item spans, so both ends sit on a minute boundary: from runningSince
// itself, a 39 s sandbox started at :07 would span two clock minutes and pay two (seen on the lab).
// An agent that creates and kills a sandbox within seconds still pays one minute. A row never read while
// running gets an assumed runningSince (its last pending read) when it ends, so a short sandbox is never free.
// ponytail: minute granularity; per-second billing would need its own ledger.
type SandboxProvider struct{}

func NewSandboxProvider() *SandboxProvider { return &SandboxProvider{} }

func (p *SandboxProvider) Type() string { return cloud.TypeSandbox }

func (p *SandboxProvider) GetBillingInformation(_ context.Context, _ billingapi.BillingContext, cr *cloud.CloudResource) ([]*billingapi.BillingResource, error) {
	d := cr.Data
	running, err := time.Parse(time.RFC3339, str(d["runningSince"]))
	status := str(d["status"])
	if err != nil || (status != "RUNNING" && status != "DELETED") {
		return nil, nil
	}
	start := running.Truncate(time.Minute)
	cr.Info = &cloud.Info{CreatedAt: &start}
	// The charge stops at the sandbox's end: endedAt once it is marked DELETED, or its
	// shutdownTime once that has passed (the controller removes it then, and the sync that
	// notices may come minutes later).
	end := ""
	if status == "DELETED" {
		end = str(d["endedAt"])
	} else if sd, err := time.Parse(time.RFC3339, str(d["shutdownTime"])); err == nil && sd.Before(time.Now()) {
		end = str(d["shutdownTime"])
	}
	var deletedAt *time.Time
	if end != "" {
		e := billedEnd(running, end)
		deletedAt = &e
	}
	name := str(d["name"])
	if name == "" {
		name = cr.ExternalID
	}
	values := map[string]any{
		"display_name": name,
		"template":     d["template"],
		"gpu_model":    d["gpuModel"],
		"status":       status,
		"vcpus":        num(d["cpu"]),
		"ram_gb":       num(d["memoryGiB"]),
		"gpu_count":    num(d["gpuCount"]),
	}
	return []*billingapi.BillingResource{{
		ResourceID:          cr.ID,
		ProjectID:           cr.ProjectID,
		ResourceType:        "sandbox",
		Values:              values,
		BillingResourceType: sandboxType(),
		DeletedAt:           deletedAt,
	}}, nil
}

// SandboxPlannedEnd is where a RUNNING sandbox's charge will stop on its own: its shutdownTime, on the
// same minute rounding the charge applies when it ends. nil when it has none (it runs until stopped)
// or is not running. The month-end forecast uses it so a sandbox with a timeout is not projected to
// run until the end of the month.
func SandboxPlannedEnd(cr *cloud.CloudResource) *time.Time {
	if cr == nil || str(cr.Data["status"]) != "RUNNING" {
		return nil
	}
	running, err := time.Parse(time.RFC3339, str(cr.Data["runningSince"]))
	if err != nil {
		return nil
	}
	sd := str(cr.Data["shutdownTime"])
	if _, err := time.Parse(time.RFC3339, sd); err != nil {
		return nil
	}
	end := billedEnd(running, sd)
	return &end
}

// billedEnd is the minute runningSince falls in plus the runtime rounded up to whole minutes, at least one.
func billedEnd(running time.Time, endedAt string) time.Time {
	end, err := time.Parse(time.RFC3339, endedAt)
	if err != nil || end.Before(running) {
		end = running
	}
	minutes := (end.Sub(running) + time.Minute - 1) / time.Minute
	return running.Truncate(time.Minute).Add(max(minutes, 1) * time.Minute)
}

func sandboxType() *billingapi.BillingResourceType {
	s := func(n string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: n, Type: "string"}
	}
	n := func(nm string) billingapi.ResourceAttribute {
		return billingapi.ResourceAttribute{Name: nm, Type: "number"}
	}
	return &billingapi.BillingResourceType{ResourceType: "sandbox", Attributes: []billingapi.ResourceAttribute{
		s("display_name"), s("template"), s("gpu_model"), s("status"),
		n("vcpus"), n("ram_gb"), n("gpu_count"),
	}}
}
