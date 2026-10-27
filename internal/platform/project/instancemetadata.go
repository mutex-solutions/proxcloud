package project

import (
	"context"
	"sort"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// instancemetadata.go holds the CLIENT read of the admin-configured instanceMetadataOption
// collection: getAvailableOptions(serviceId, region), projected through the client DTO. The
// create-form metadata panel reads this to render admin-defined keys (userEditable ones are
// editable); a custom (non-admin) key the user already set still renders, but only admin options
// drive the "add a metadata field" dropdown.

// InstanceMetadataReader reads the instanceMetadataOption collection for the client metadata panel.
type InstanceMetadataReader struct {
	col *pgdoc.Store
}

// NewInstanceMetadataReader binds the reader to the instanceMetadataOption collection.
func NewInstanceMetadataReader(db *pgdoc.DB) *InstanceMetadataReader {
	return &InstanceMetadataReader{col: db.C("instanceMetadataOption")}
}

// AvailableOptions returns the enabled options for (serviceId, region): enabled==true → filter
// appliesToRegion(serviceId, region) → sort by createdAt asc → map to the client DTO (nulls omitted).
func (r *InstanceMetadataReader) AvailableOptions(ctx context.Context, serviceID, region string) ([]map[string]any, error) {
	var docs []pgdoc.M
	if err := r.col.Find(ctx, pgdoc.M{"enabled": true}, &docs); err != nil {
		return nil, err
	}
	kept := docs[:0]
	for _, d := range docs {
		if metadataAppliesToRegion(d, serviceID, region) {
			kept = append(kept, d)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		return metadataCreatedAtMillis(kept[i]) < metadataCreatedAtMillis(kept[j])
	})
	out := make([]map[string]any, 0, len(kept))
	for _, d := range kept {
		out = append(out, instanceMetadataDTO(d))
	}
	return out, nil
}

// metadataAppliesToRegion: global (no serviceIds) → always; else the serviceId must be listed AND
// (no regions → all regions, or the region listed).
func metadataAppliesToRegion(d pgdoc.M, serviceID, region string) bool {
	svcIDs := metadataStrings(d["serviceIds"])
	if len(svcIDs) == 0 {
		return true
	}
	if !metadataContains(svcIDs, serviceID) {
		return false
	}
	regions := metadataStrings(d["regions"])
	return len(regions) == 0 || metadataContains(regions, region)
}

// instanceMetadataDTO builds the client-safe projection (drops
// serviceIds/regions/enabled/disabled*/timestamps; filters value-options to the enabled ones). Blank
// optional strings + a nil numericRange are omitted (null fields dropped).
func instanceMetadataDTO(d pgdoc.M) map[string]any {
	dto := map[string]any{
		"key":          d["key"],
		"userEditable": metadataBool(d["userEditable"]),
		"showInline":   metadataBool(d["showInline"]),
	}
	if id, ok := d["_id"]; ok {
		dto["id"] = id // injected by the store as the String id
	}
	if s := metadataString(d["displayName"]); s != "" {
		dto["displayName"] = s
	}
	if s := metadataString(d["description"]); s != "" {
		dto["description"] = s
	}
	if s := metadataString(d["type"]); s != "" {
		dto["type"] = s
	}
	// options: only the enabled value-options (null → []).
	opts := []map[string]any{}
	if arr, ok := d["options"].(pgdoc.A); ok {
		for _, o := range arr {
			om, ok := o.(map[string]any)
			if !ok || !metadataBool(om["enabled"]) {
				continue
			}
			vo := map[string]any{"enabled": true}
			if s := metadataString(om["value"]); s != "" {
				vo["value"] = s
			}
			if s := metadataString(om["displayName"]); s != "" {
				vo["displayName"] = s
			}
			opts = append(opts, vo)
		}
	}
	dto["options"] = opts
	if nr, ok := d["numericRange"]; ok && nr != nil {
		dto["numericRange"] = nr
	}
	return dto
}

func metadataCreatedAtMillis(d pgdoc.M) int64 {
	switch v := d["createdAt"].(type) {
	case time.Time:
		return v.UnixMilli()
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func metadataStrings(v any) []string {
	arr, ok := v.(pgdoc.A)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func metadataContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func metadataBool(v any) bool { b, _ := v.(bool); return b }

func metadataString(v any) string { s, _ := v.(string); return s }
