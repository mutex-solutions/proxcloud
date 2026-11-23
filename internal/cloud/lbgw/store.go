package lbgw

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// TokenUser is the realm part of every cluster's load balancer credential. Like csi@proxcloud it is not a PVE user.
const TokenUser = "lb@proxcloud"

// TokenID is the token id a cluster's load balancer controller presents: lb@proxcloud!<clusterId>.
func TokenID(clusterID string) string { return TokenUser + "!" + clusterID }

// Credential is one tenant cluster's load balancer credential (collection lbGatewayCredential, _id = cluster id). Only
// a hash of the secret is kept, and everything a call may touch comes from this row, never from the request.
type Credential struct {
	ID               string `json:"id"`
	ProjectID        string `json:"projectId"`
	ComputeServiceID string `json:"computeServiceId"`
	// KubernetesServiceID is the provider whose config.cluster.loadBalancer names the public network and the flavor of
	// the cluster's balancer when its first Service comes.
	KubernetesServiceID string `json:"kubernetesServiceId"`
	// NetworkID is the cluster's network (the row the tenant picked), Vnet its bridge: the appliances' tenant side.
	NetworkID    string     `json:"networkId"`
	Vnet         string     `json:"vnet"`
	SecretSHA256 string     `json:"secretSHA256"`
	CreatedAt    *time.Time `json:"createdAt,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	// LastSeenAt is the last authenticated call, written at most once a minute.
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
}

// State is what a cluster's controller last asked for and the portal accepted (collection lbGatewayState, _id =
// cluster id): the Services that passed the gateway's checks, and the addresses of the workers that are the cluster's
// own. The PUT writes it, the converge reads it; neither waits on the other.
type State struct {
	ID       string    `json:"id"`
	Services []Service `json:"services"`
	Nodes    []string  `json:"nodes"`
	// CertHashes are the hashes of the Secrets the cluster's Services name, in the last request and in Services:
	// the certificates the portal keeps for the cluster. A hash, never a certificate.
	CertHashes []string   `json:"certHashes,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
}

// Store is the Postgres side of the gateway.
type Store struct{ creds, states *pgdoc.Store }

func NewStore(db *pgdoc.DB) *Store {
	return &Store{creds: db.C("lbGatewayCredential"), states: db.C("lbGatewayState")}
}

// MintCredential stores c with a fresh secret and returns the token id and the secret for the controller's Secret.
// The secret exists only in the answer. Minting again replaces the row, which is how a credential is rotated; the
// cluster's state stays.
func (s *Store) MintCredential(ctx context.Context, c Credential) (tokenID, secret string, err error) {
	if c.ID == "" || c.ProjectID == "" || c.ComputeServiceID == "" || c.KubernetesServiceID == "" || c.NetworkID == "" || c.Vnet == "" {
		return "", "", errors.New("lbgw: credential needs a cluster, a project, a compute and a Kubernetes provider, and the cluster's network")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	secret = hex.EncodeToString(b)
	now := time.Now().UTC()
	c.SecretSHA256, c.CreatedAt, c.RevokedAt, c.LastSeenAt = hashSecret(secret), &now, nil, nil
	if err := s.creds.Upsert(ctx, c.ID, c); err != nil {
		return "", "", err
	}
	return TokenID(c.ID), secret, nil
}

// RevokeCredential refuses the cluster from now on. No row is not an error.
func (s *Store) RevokeCredential(ctx context.Context, clusterID string) error {
	_, err := s.creds.SetByID(ctx, clusterID, pgdoc.M{"revokedAt": time.Now().UTC()}, nil)
	return err
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Credential(ctx context.Context, clusterID string) (*Credential, error) {
	var c Credential
	found, err := s.creds.Get(ctx, clusterID, &c)
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// Seen records an authenticated call.
func (s *Store) Seen(ctx context.Context, clusterID string, at time.Time) error {
	_, err := s.creds.SetByID(ctx, clusterID, pgdoc.M{"lastSeenAt": at}, nil)
	return err
}

// PutState replaces the cluster's accepted state.
func (s *Store) PutState(ctx context.Context, st *State) error {
	now := time.Now().UTC()
	st.UpdatedAt = &now
	return s.states.Upsert(ctx, st.ID, st)
}

// State is the cluster's accepted state, nil when its controller never sent one.
func (s *Store) State(ctx context.Context, clusterID string) (*State, error) {
	var st State
	found, err := s.states.Get(ctx, clusterID, &st)
	if err != nil || !found {
		return nil, err
	}
	return &st, nil
}

// DeleteState drops the cluster's state at its release.
func (s *Store) DeleteState(ctx context.Context, clusterID string) error {
	_, err := s.states.DeleteByID(ctx, clusterID)
	return err
}
