package project

import (
	"crypto/dsa"
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
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

// golang.org/x/crypto 0.59.0 removed DSA from its ssh package: a well-formed ssh-dss key is refused, and the
// answer says why instead of calling the key invalid.
func TestKeypairImportRefusesDSA(t *testing.T) {
	var key dsa.PrivateKey
	if err := dsa.GenerateParameters(&key.Parameters, rand.Reader, dsa.L1024N160); err != nil {
		t.Fatal(err)
	}
	if err := dsa.GenerateKey(&key, rand.Reader); err != nil {
		t.Fatal(err)
	}
	blob := ssh.Marshal(struct {
		Name       string
		P, Q, G, Y *big.Int
	}{"ssh-dss", key.P, key.Q, key.G, key.Y})
	for line, want := range map[string]string{
		"ssh-dss " + base64.StdEncoding.EncodeToString(blob) + " old@host": "ssh-dss (DSA) keys are no longer accepted",
		"not a key": "publicKey is not a valid SSH public key",
	} {
		w := httptest.NewRecorder()
		(&Handler{}).pveCreateKeypair(w, httptest.NewRequest(http.MethodPost, "/", nil), &user.User{ID: "u1"}, nil, nil,
			map[string]any{"name": "k", "publicKey": line})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%.20q: got %d %s, want 400 with %q", line, w.Code, w.Body, want)
		}
	}
}
