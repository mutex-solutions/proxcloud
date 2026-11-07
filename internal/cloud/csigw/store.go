package csigw

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Volume states. A row is written before PVE is asked to create the disk, so the primary key decides a race on a name.
const (
	StateCreating = "creating"
	StateReady    = "ready"
	StateDeleting = "deleting"
	StateDeleted  = "deleted"
)

// TokenUser is the realm part of every cluster's token id. It is not a PVE user: PVE never sees these tokens.
const TokenUser = "csi@proxcloud"

// TokenID is the token id a cluster's proxmox-csi controller presents: csi@proxcloud!<clusterId>.
func TokenID(clusterID string) string { return TokenUser + "!" + clusterID }

// Credential is one tenant cluster's gateway credential (collection csiGatewayCredential, _id = cluster id). Only a hash
// of the secret is kept, and everything the gateway lets the cluster touch comes from this row, never from a request.
type Credential struct {
	ID               string   `json:"id"`
	ProjectID        string   `json:"projectId"`
	ComputeServiceID string   `json:"computeServiceId"`
	Region           string   `json:"region"`
	Pool             string   `json:"pool"`
	Storages         []string `json:"storages"`
	// ControllerVMID is the plugin's features.controllerVmID (9999 when unset): every disk it creates is vm-<id>-….
	ControllerVMID int        `json:"controllerVmId"`
	SecretSHA256   string     `json:"secretSHA256"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
}

// Volume is one disk the gateway created for a cluster (collection csiVolume, _id = VolumeID). It is the proof of
// ownership: a disk without a row of the cluster does not exist for that cluster.
type Volume struct {
	ID               string     `json:"id"`
	ClusterID        string     `json:"clusterId"`
	ProjectID        string     `json:"projectId"`
	ComputeServiceID string     `json:"computeServiceId"`
	Storage          string     `json:"storage"`
	Node             string     `json:"node"`
	VolID            string     `json:"volid"`
	SizeBytes        int64      `json:"sizeBytes"`
	SourceID         string     `json:"sourceId,omitempty"`
	State            string     `json:"state"`
	CreatedAt        *time.Time `json:"createdAt,omitempty"`
	DeletedAt        *time.Time `json:"deletedAt,omitempty"`
}

// VolumeID keys a disk by provider, store and name. The name is the file name without the vmid directory file-based
// stores put it under, so one disk has one id whichever form a request names it by.
func VolumeID(serviceID, storage, name string) string { return serviceID + "/" + storage + "/" + name }

// Task is a UPID the gateway answered a cluster with (collection csiTask, _id = UPID). A cluster may only poll its own.
type Task struct {
	ID        string    `json:"id"`
	ClusterID string    `json:"clusterId"`
	Kind      string    `json:"kind"` // attach, copy, delete, resize
	VolumeID  string    `json:"volumeId,omitempty"`
	SizeBytes int64     `json:"sizeBytes,omitempty"`
	Done      bool      `json:"done,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ErrExists is a volume insert that lost to an existing row.
var ErrExists = errors.New("csigw: volume exists")

// Registry is what the gateway keeps in Postgres. Store is the implementation; the tests use one in memory.
type Registry interface {
	Credential(ctx context.Context, clusterID string) (*Credential, error)
	InsertVolume(ctx context.Context, v *Volume) error
	Volume(ctx context.Context, id string) (*Volume, error)
	UpdateVolume(ctx context.Context, id string, set pgdoc.M) error
	DeleteVolume(ctx context.Context, id string) error
	// ClusterVolumes lists the cluster's rows on one store of one provider, all states but deleted.
	ClusterVolumes(ctx context.Context, clusterID, serviceID, storage string) ([]Volume, error)
	// ProjectBytes sums the project's rows on a provider, all states but deleted: what quota counts.
	ProjectBytes(ctx context.Context, projectID, serviceID string) (int64, error)
	PutTask(ctx context.Context, t *Task) error
	Task(ctx context.Context, upid string) (*Task, error)
	// FinishTask marks the task done; only the first caller gets true, so a result is applied once.
	FinishTask(ctx context.Context, upid string) (bool, error)
}

// Store is the Postgres registry.
type Store struct{ creds, vols, tasks *pgdoc.Store }

func NewStore(db *pgdoc.DB) *Store {
	return &Store{creds: db.C("csiGatewayCredential"), vols: db.C("csiVolume"), tasks: db.C("csiTask")}
}

// MintCredential stores c with a fresh secret and returns the token id and secret for the cluster's plugin config. The
// secret exists only in the answer. Minting again replaces the row, which is how a credential is rotated.
func (s *Store) MintCredential(ctx context.Context, c Credential) (tokenID, secret string, err error) {
	if c.ID == "" || c.ComputeServiceID == "" || c.Pool == "" || c.ControllerVMID <= 0 {
		return "", "", errors.New("csigw: credential needs a cluster, a compute provider, a pool and a controller vmid")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	secret = hex.EncodeToString(b)
	now := time.Now().UTC()
	c.SecretSHA256, c.CreatedAt, c.RevokedAt = hashSecret(secret), &now, nil
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

func (s *Store) InsertVolume(ctx context.Context, v *Volume) error {
	_, err := s.vols.InsertOne(ctx, v)
	if pgdoc.IsDup(err) {
		return ErrExists
	}
	return err
}

func (s *Store) Volume(ctx context.Context, id string) (*Volume, error) {
	var v Volume
	found, err := s.vols.Get(ctx, id, &v)
	if err != nil || !found {
		return nil, err
	}
	return &v, nil
}

func (s *Store) UpdateVolume(ctx context.Context, id string, set pgdoc.M) error {
	_, err := s.vols.SetByID(ctx, id, set, nil)
	return err
}

func (s *Store) DeleteVolume(ctx context.Context, id string) error {
	_, err := s.vols.DeleteByID(ctx, id)
	return err
}

func (s *Store) ClusterVolumes(ctx context.Context, clusterID, serviceID, storage string) ([]Volume, error) {
	var out []Volume
	err := s.vols.Find(ctx, pgdoc.M{"clusterId": clusterID, "computeServiceId": serviceID, "storage": storage,
		"state": pgdoc.M{"$ne": StateDeleted}}, &out)
	return out, err
}

func (s *Store) ProjectBytes(ctx context.Context, projectID, serviceID string) (int64, error) {
	var rows []Volume
	if err := s.vols.Find(ctx, pgdoc.M{"projectId": projectID, "computeServiceId": serviceID,
		"state": pgdoc.M{"$ne": StateDeleted}}, &rows); err != nil {
		return 0, err
	}
	var n int64
	for _, v := range rows {
		n += v.SizeBytes
	}
	return n, nil
}

// PutTask records a UPID and drops the cluster's expired ones.
func (s *Store) PutTask(ctx context.Context, t *Task) error {
	if _, err := s.tasks.DeleteMany(ctx, pgdoc.M{"clusterId": t.ClusterID, "expiresAt": pgdoc.M{"$lt": time.Now().UTC()}}); err != nil {
		return err
	}
	return s.tasks.Upsert(ctx, t.ID, t)
}

func (s *Store) Task(ctx context.Context, upid string) (*Task, error) {
	var t Task
	found, err := s.tasks.Get(ctx, upid, &t)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

func (s *Store) FinishTask(ctx context.Context, upid string) (bool, error) {
	return s.tasks.SetFieldsOne(ctx, pgdoc.M{"_id": upid, "done": pgdoc.M{"$ne": true}}, pgdoc.M{"done": true}, nil)
}
