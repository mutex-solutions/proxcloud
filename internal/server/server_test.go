package server

import "testing"

func TestAccessLogRedactsPathTokens(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v1/nocloud/abc123/user-data":            "/api/v1/nocloud/REDACTED/user-data",
		"/api/v1/download/abc123":                     "/api/v1/download/REDACTED",
		"/api/v1/console/abc123/websockify":           "/api/v1/console/REDACTED/websockify",
		"/api/v1/project-invites/abc123":              "/api/v1/project-invites/REDACTED",
		"/api/v1/project-invites/accept/abc123":       "/api/v1/project-invites/accept/REDACTED",
		"/api/v1/project-invites/decline/abc123":      "/api/v1/project-invites/decline/REDACTED",
		"/api/v1/project/6aee58/cloud/6aee59/action":  "/api/v1/project/6aee58/cloud/6aee59/action",
		"/api/v1/bill/6aee58/download/6aee59/receipt": "/api/v1/bill/6aee58/download/REDACTED/receipt",
	} {
		if got := redactPath(in); got != want {
			t.Errorf("%s = %s, want %s", in, got, want)
		}
	}
}
