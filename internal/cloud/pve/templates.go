package pve

import (
	"context"
	"sort"
	"strings"
)

// Template discovery (plan IMG-V3): the image pipeline publishes a template by tagging it, and the
// portal reads the catalog from the cluster instead of hand-edited config. Only pool TemplatePool
// counts: a tag alone is no ownership proof, the pool is.
const (
	TemplatePool     = "pc-templates"
	TagTemplatePub   = "pc-public"
	TagTemplateImage = "pc-img-" // pc-img-<slug>: the catalog entry the template belongs to
	// TagTemplateVersion is pc-v-<UTC yyyymmddhhmm>; fixed width, so the newest sorts last.
	TagTemplateVersion = "pc-v-"
	// TagTemplateWithdrawn marks a template whose artifact an admin withdrew (docs/template-updates.md,
	// lifecycle): it never enters the catalog again, even if pc-public is put back by hand.
	TagTemplateWithdrawn = "pc-tu-withdrawn"
)

// TemplateCopy is one cluster template of a catalog entry.
type TemplateCopy struct {
	VMID    int
	Version string // "" for a template tagged without a version (ranks oldest)
	DiskGb  int
}

// DiscoveredTemplate is one catalog entry found in the cluster. A clone to local storage must run
// on the template's own node (PVE refuses to clone to non-shared storage elsewhere), so a site with
// local VM disks publishes a copy per node.
type DiscoveredTemplate struct {
	Slug string
	// Node and VMID are the newest copy overall; ties go to the first node by name.
	Node string
	VMID int
	// Copies is the newest copy on each node.
	Copies map[string]TemplateCopy
}

// DiscoverTemplates groups the qemu rows of /cluster/resources into catalog entries, sorted by slug.
func DiscoverTemplates(rows []ClusterResource) []DiscoveredTemplate {
	bySlug := map[string]*DiscoveredTemplate{}
	for _, r := range rows {
		if r.Template != 1 || r.NodeDown || r.Pool != TemplatePool || !hasTag(r.Tags, TagTemplatePub) || hasTag(r.Tags, TagTemplateWithdrawn) {
			continue
		}
		slug := TagValue(r.Tags, TagTemplateImage)
		if slug == "" {
			continue
		}
		d := bySlug[slug]
		if d == nil {
			d = &DiscoveredTemplate{Slug: slug, Copies: map[string]TemplateCopy{}}
			bySlug[slug] = d
		}
		c := TemplateCopy{VMID: r.VMID, Version: TagValue(r.Tags, TagTemplateVersion), DiskGb: int((r.MaxDisk + 1<<30 - 1) >> 30)}
		if cur, ok := d.Copies[r.Node]; !ok || c.Version > cur.Version || (c.Version == cur.Version && c.VMID > cur.VMID) {
			d.Copies[r.Node] = c
		}
	}
	out := make([]DiscoveredTemplate, 0, len(bySlug))
	for _, d := range bySlug {
		nodes := make([]string, 0, len(d.Copies))
		for n := range d.Copies {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		best := ""
		for _, n := range nodes {
			if c := d.Copies[n]; d.Node == "" || c.Version > best {
				d.Node, d.VMID, best = n, c.VMID, c.Version
			}
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// Templates reads the tagged template catalog from the cluster.
func (c *Client) Templates(ctx context.Context) ([]DiscoveredTemplate, error) {
	rows, err := c.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	return DiscoverTemplates(rows), nil
}

func hasTag(tags, want string) bool {
	for _, t := range strings.Split(tags, ";") {
		if t == want {
			return true
		}
	}
	return false
}
