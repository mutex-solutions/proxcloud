package mcp

// adminCatalogTools drive the remaining catalog surfaces through the internal
// /api/v1/admin routes: live Glance images, image categories, image groups, and
// instance metadata options. (Flavor categories live in tools_admin_platform.go.)
var adminCatalogTools = []toolDef{
	// ── live Glance images (the names image groups bind) ─────────────────────
	{
		name:   "list_live_images",
		desc:   "Live public Glance images across every cloud provider + region: [{serviceId, serviceName, region, images:[{id, name, status}]}]. Use the names when building image groups.",
		method: "GET",
		path:   "/api/v1/admin/service/os-images",
	},

	// ── image categories (top-level buckets) ─────────────────────────────────
	{
		name:   "list_image_categories",
		desc:   "List image categories (top-level buckets; image groups hang under them).",
		method: "GET",
		path:   "/api/v1/admin/images/categories",
	},
	{
		name:   "get_image_category",
		desc:   "Get an image category by id.",
		method: "GET",
		path:   "/api/v1/admin/images/categories/{id}",
		params: []param{{name: "id", typ: "string", desc: "Image category id.", required: true, in: "path"}},
	},
	{
		name:   "list_image_category_groups",
		desc:   "List the image groups under one category.",
		method: "GET",
		path:   "/api/v1/admin/images/categories/{id}/groups",
		params: []param{{name: "id", typ: "string", desc: "Image category id.", required: true, in: "path"}},
	},
	{
		name:   "create_image_category",
		desc:   "Create an image category (e.g. Ubuntu, Windows, GPU / ML).",
		method: "POST",
		path:   "/api/v1/admin/images/categories",
		params: []param{
			{name: "name", typ: "string", desc: "Category name.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Category description.", in: "body"},
			{name: "bareMetal", typ: "boolean", desc: "Bare-metal category flag.", in: "body"},
		},
	},
	{
		name:   "update_image_category",
		desc:   "Update an image category (full replace of the mutable fields).",
		method: "PUT",
		path:   "/api/v1/admin/images/categories/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Image category id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Category name.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Category description.", in: "body"},
			{name: "bareMetal", typ: "boolean", desc: "Bare-metal category flag.", in: "body"},
		},
	},
	{
		name:   "delete_image_category",
		desc:   "Delete an image category (cascade-deletes its child image groups).",
		method: "DELETE",
		path:   "/api/v1/admin/images/categories/{id}",
		params: []param{{name: "id", typ: "string", desc: "Image category id.", required: true, in: "path"}},
	},

	// ── image groups (curated OS images shown at create-server) ──────────────
	{
		name:   "get_image_group",
		desc:   "Get an image group by id.",
		method: "GET",
		path:   "/api/v1/admin/images/groups/{id}",
		params: []param{{name: "id", typ: "string", desc: "Image group id.", required: true, in: "path"}},
	},
	{
		name:   "create_image_group",
		desc:   "Create an image group under a category. images = [{name, version, orderNumber}] where name is a live Glance image name (see list_live_images); labels = [{label, description, color}] optional badges.",
		method: "POST",
		path:   "/api/v1/admin/images/groups",
		params: []param{
			{name: "name", typ: "string", desc: "Group name (e.g. Ubuntu 24.04 LTS).", required: true, in: "body"},
			{name: "categoryId", typ: "string", desc: "Owning image category id.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Group description.", in: "body"},
			{name: "enabled", typ: "boolean", desc: "Show the group to clients.", in: "body"},
			{name: "orderNumber", typ: "integer", desc: "Sort order.", in: "body"},
			{name: "groupLogoUrl", typ: "string", desc: "Logo URL.", in: "body"},
			{name: "images", typ: "array", desc: "Bound images: [{name (Glance image name), version, orderNumber}].", in: "body"},
			{name: "labels", typ: "array", desc: "Optional badges: [{label, description, color}].", in: "body"},
		},
	},
	{
		name:   "update_image_group",
		desc:   "Update an image group (full replace of the mutable fields).",
		method: "PUT",
		path:   "/api/v1/admin/images/groups/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Image group id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Group name.", required: true, in: "body"},
			{name: "categoryId", typ: "string", desc: "Owning image category id.", in: "body"},
			{name: "description", typ: "string", desc: "Group description.", in: "body"},
			{name: "enabled", typ: "boolean", desc: "Show the group to clients.", in: "body"},
			{name: "orderNumber", typ: "integer", desc: "Sort order.", in: "body"},
			{name: "groupLogoUrl", typ: "string", desc: "Logo URL.", in: "body"},
			{name: "images", typ: "array", desc: "Bound images: [{name, version, orderNumber}].", in: "body"},
			{name: "labels", typ: "array", desc: "Optional badges: [{label, description, color}].", in: "body"},
		},
	},
	{
		name:   "delete_image_group",
		desc:   "Delete an image group.",
		method: "DELETE",
		path:   "/api/v1/admin/images/groups/{id}",
		params: []param{{name: "id", typ: "string", desc: "Image group id.", required: true, in: "path"}},
	},

	// ── instance metadata options (custom tags at create-server) ─────────────
	{
		name:   "list_instance_metadata_options",
		desc:   "List the instance metadata options offered to clients (custom key/value tags on servers).",
		method: "GET",
		path:   "/api/v1/admin/instance-metadata-options",
	},
	{
		name:   "get_instance_metadata_option",
		desc:   "Get an instance metadata option by id.",
		method: "GET",
		path:   "/api/v1/admin/instance-metadata-options/{id}",
		params: []param{{name: "id", typ: "string", desc: "Option id.", required: true, in: "path"}},
	},
	{
		name:   "create_instance_metadata_option",
		desc:   "Create an instance metadata option. type = PREDEFINED_VALUES (with options[]) | TEXT | NUMERIC_RANGE (with numericRange{min,max,unit}). key must not use a reserved prefix (hw:, os_, proxcloud_). serviceIds/regions scope it (regions require serviceIds).",
		method: "POST",
		path:   "/api/v1/admin/instance-metadata-options",
		params: []param{
			{name: "key", typ: "string", desc: "Metadata key stamped on the server (e.g. environment).", required: true, in: "body"},
			{name: "displayName", typ: "string", desc: "Label shown to clients.", required: true, in: "body"},
			{name: "type", typ: "string", desc: "PREDEFINED_VALUES | TEXT | NUMERIC_RANGE.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Help text.", in: "body"},
			{name: "options", typ: "array", desc: "For PREDEFINED_VALUES: [{value, displayName, enabled}].", in: "body"},
			{name: "numericRange", typ: "object", desc: "For NUMERIC_RANGE: {min, max, unit}.", in: "body"},
			{name: "serviceIds", typ: "array", desc: "Restrict to these cloud provider ids (empty = all).", in: "body"},
			{name: "regions", typ: "array", desc: "Restrict to these regions (requires serviceIds).", in: "body"},
			{name: "userEditable", typ: "boolean", desc: "Client may edit the value after create.", in: "body"},
			{name: "showInline", typ: "boolean", desc: "Show on the create-server form (vs an advanced section).", in: "body"},
		},
	},
	{
		name:   "update_instance_metadata_option",
		desc:   "Update an instance metadata option (same fields as create; key is immutable).",
		method: "PUT",
		path:   "/api/v1/admin/instance-metadata-options/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Option id.", required: true, in: "path"},
			{name: "displayName", typ: "string", desc: "Label shown to clients.", in: "body"},
			{name: "type", typ: "string", desc: "PREDEFINED_VALUES | TEXT | NUMERIC_RANGE.", in: "body"},
			{name: "description", typ: "string", desc: "Help text.", in: "body"},
			{name: "options", typ: "array", desc: "For PREDEFINED_VALUES: [{value, displayName, enabled}].", in: "body"},
			{name: "numericRange", typ: "object", desc: "For NUMERIC_RANGE: {min, max, unit}.", in: "body"},
			{name: "serviceIds", typ: "array", desc: "Restrict to these cloud provider ids.", in: "body"},
			{name: "regions", typ: "array", desc: "Restrict to these regions.", in: "body"},
			{name: "userEditable", typ: "boolean", desc: "Client may edit the value.", in: "body"},
			{name: "showInline", typ: "boolean", desc: "Show on the create-server form.", in: "body"},
		},
	},
	{
		name:   "delete_instance_metadata_option",
		desc:   "Disable an instance metadata option (soft delete). Pass permanent=true to remove it entirely.",
		method: "DELETE",
		path:   "/api/v1/admin/instance-metadata-options/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Option id.", required: true, in: "path"},
			{name: "permanent", typ: "boolean", desc: "true = hard delete; omitted = soft disable.", in: "query"},
		},
	},
	{
		name:   "reactivate_instance_metadata_option",
		desc:   "Re-enable a soft-disabled instance metadata option.",
		method: "POST",
		path:   "/api/v1/admin/instance-metadata-options/{id}/reactivate",
		params: []param{{name: "id", typ: "string", desc: "Option id.", required: true, in: "path"}},
	},
}
