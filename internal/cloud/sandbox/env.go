package sandbox

import "github.com/mutex-solutions/proxcloud/internal/sandboxenv"

// setSandboxIDEnv replaces all old entries, including valueFrom/duplicates in saved
// CRs. The ID is pod metadata, never caller or image data. Other env is untouched.
func setSandboxIDEnv(container map[string]any, id string) {
	if container == nil {
		return
	}
	env, _ := container["env"].([]any)
	out := make([]any, 0, len(env)+1)
	for _, entry := range env {
		m, _ := entry.(map[string]any)
		if str(m["name"]) != sandboxenv.SandboxID {
			out = append(out, entry)
		}
	}
	container["env"] = append(out, map[string]any{"name": sandboxenv.SandboxID, "value": id})
}
