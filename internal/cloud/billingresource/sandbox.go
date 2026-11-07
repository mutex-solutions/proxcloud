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
//   - once RUNNING, cr.Info.CreatedAt is set to runningSince. stampResourceValues prefers
//     Info.CreatedAt, so the item is created with its watermark at the moment the sandbox started.
//
// End of life: an ended sandbox's row lingers as DELETED (sandbox.SyncProvider.Linger) and is
// rated up to DeletedAt = its runtime rounded UP to whole minutes from runningSince, at least one
// minute. The engine stops at DeletedAt, so the lingering ticks add nothing beyond that; an
// agent that creates and kills a sandbox within seconds still pays one minute.
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
	cr.Info = &cloud.Info{CreatedAt: &running}
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

// billedEnd is runningSince plus the runtime rounded up to whole minutes, at least one.
func billedEnd(running time.Time, endedAt string) time.Time {
	end, err := time.Parse(time.RFC3339, endedAt)
	if err != nil || end.Before(running) {
		end = running
	}
	minutes := (end.Sub(running) + time.Minute - 1) / time.Minute
	return running.Add(max(minutes, 1) * time.Minute)
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
