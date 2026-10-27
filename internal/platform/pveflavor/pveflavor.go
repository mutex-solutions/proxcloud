// Package pveflavor is the platform-side flavor catalog for Proxmox providers.
// Proxmox has no flavor concept, so the admin curates one here (pgdoc collection
// `flavor`). The client wizard reads it through the existing LIST_FLAVORS bulk
// action shaped exactly like a live nova flavor — zero FE change — and the create
// path resolves flavorId (the slug) back through this repo.
package pveflavor

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Flavor is one curated flavor document.
type Flavor struct {
	ID             string     `json:"id,omitempty"` // pgdoc _id = Slug
	Slug           string     `json:"slug"`         // rides VM tags ("fl-<slug>") — lowercase [a-z0-9-]
	Name           string     `json:"name"`         // display name; defaults to Slug
	VCPUs          int        `json:"vcpus"`
	RAMMb          int        `json:"ramMb"`
	DiskGb         int        `json:"diskGb"` // root disk size a created VM is resized to
	StorageProfile string     `json:"storageProfile,omitempty"`
	GPUModel       string     `json:"gpuModel,omitempty"` // normalized alias ("nvidia-4090")
	GPUCount       int        `json:"gpuCount,omitempty"`
	Active         bool       `json:"active"`
	OrderNumber    int        `json:"orderNumber,omitempty"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
}

// slugRe: the slug lands in PVE VM tags (lowercase alnum + dash) and in billing
// attribute values — keep it tight.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$|^[a-z0-9]$`)

// Validate rejects an unusable flavor before it hits the catalog.
func (f *Flavor) Validate() error {
	if !slugRe.MatchString(f.Slug) {
		return fmt.Errorf("slug must match %s", slugRe)
	}
	if f.VCPUs <= 0 || f.RAMMb <= 0 {
		return fmt.Errorf("vcpus and ramMb must be positive")
	}
	if f.DiskGb < 0 || f.GPUCount < 0 {
		return fmt.Errorf("diskGb and gpuCount cannot be negative")
	}
	if (f.GPUModel == "") != (f.GPUCount == 0) {
		return fmt.Errorf("gpuModel and gpuCount go together")
	}
	return nil
}

// ExtraSpecs renders the flavor's GPU demand in the OpenStack extra_specs shape
// the existing GPU parsing/billing (cloud.GPUFromFlavor) already understands.
func (f *Flavor) ExtraSpecs() map[string]any {
	if f.GPUModel == "" || f.GPUCount <= 0 {
		return map[string]any{}
	}
	return map[string]any{"pci_passthrough:alias": fmt.Sprintf("%s:%d", f.GPUModel, f.GPUCount)}
}

// LiveShape renders the flavor exactly like the LIST_FLAVORS live-nova rows the
// FE wizard consumes ({externalId, data:{id,name,vcpus,ram,disk,extra_specs}}).
func (f *Flavor) LiveShape(serviceID, region string) map[string]any {
	name := f.Name
	if name == "" {
		name = f.Slug
	}
	return map[string]any{
		"externalId": f.Slug,
		"serviceId":  serviceID,
		"region":     region,
		"type":       "SERVER",
		"data": map[string]any{
			"id":          f.Slug,
			"name":        name,
			"vcpus":       f.VCPUs,
			"ram":         f.RAMMb,
			"disk":        f.DiskGb,
			"ephemeral":   0,
			"swap":        0,
			"extra_specs": f.ExtraSpecs(),
		},
	}
}

// Repo persists the flavor catalog.
type Repo struct{ col *pgdoc.Store }

func New(db *pgdoc.DB) *Repo { return &Repo{col: db.C("flavor")} }

// Save upserts a flavor keyed by slug.
func (r *Repo) Save(ctx context.Context, f *Flavor) error {
	if err := f.Validate(); err != nil {
		return err
	}
	f.ID = f.Slug
	if f.CreatedAt == nil {
		now := time.Now().UTC()
		f.CreatedAt = &now
	}
	return r.col.Upsert(ctx, f.ID, f)
}

// Get loads one flavor by slug (nil when absent).
func (r *Repo) Get(ctx context.Context, slug string) (*Flavor, error) {
	var f Flavor
	found, err := r.col.Get(ctx, slug, &f)
	if err != nil || !found {
		return nil, err
	}
	return &f, nil
}

// List returns the whole catalog (activeOnly filters client-facing reads).
func (r *Repo) List(ctx context.Context, activeOnly bool) ([]Flavor, error) {
	var out []Flavor
	filter := pgdoc.M{}
	if activeOnly {
		filter["active"] = true
	}
	if err := r.col.Find(ctx, filter, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a flavor (idempotent). Existing VMs keep billing by their tag —
// the flavor block is denormalized onto the VM at create time.
func (r *Repo) Delete(ctx context.Context, slug string) error {
	_, err := r.col.DeleteByID(ctx, slug)
	return err
}
