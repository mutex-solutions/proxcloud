package project

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestKeypairOwnedByUser(t *testing.T) {
	kp := func(userID, projectID string) *cloud.CloudResource {
		return &cloud.CloudResource{Type: cloud.TypeKeypair, UserID: userID, ProjectID: projectID}
	}
	cases := []struct {
		name string
		cr   *cloud.CloudResource
		uid  string
		want bool
	}{
		{"own keypair", kp("u1", ""), "u1", true},
		{"another user's keypair", kp("u2", ""), "u1", false}, // must not authorize across users
		{"blank caller uid", kp("", ""), "", false},           // blank must never match
		{"keypair with a projectId (not user-scoped)", kp("u1", "p1"), "u1", false},
		{"non-keypair type", &cloud.CloudResource{Type: cloud.TypeServer, UserID: "u1"}, "u1", false},
		{"nil resource", nil, "u1", false},
	}
	for _, c := range cases {
		if got := keypairOwnedByUser(c.cr, c.uid); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
