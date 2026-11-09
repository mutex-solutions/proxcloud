package message

// RenderSnapshot renders an already-loaded immutable template snapshot without
// another database read. The caller distinguishes missing/disabled once before
// committing its durable notification decision; legacy Repo.Render is unchanged.
func RenderSnapshot(title, body string, vars map[string]any) Rendered {
	return Rendered{Title: renderMustacheText(title, vars), Body: renderMustache(body, vars)}
}
