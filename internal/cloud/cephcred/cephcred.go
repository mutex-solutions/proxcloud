// Package cephcred stores the per-project Ceph RGW (S3) service-account credentials. A ceph-s3 project has
// no Keystone tenant; its bucket + object I/O runs as its OWN RGW user (rgwUid, in RGW's default tenant)
// whose S3 keys are provisioned at bootstrap and kept HERE (secret key encrypted at rest), keyed by
// (projectId, serviceId). Kept OUT of the project doc on purpose — the project document is serialized to
// the client, so the secret key would leak; this collection is never serialized.
package cephcred

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// Credential is one cephRgwCredential document — the project's RGW user keys for a ceph-s3 service.
type Credential struct {
	ID        string     `json:"id,omitempty"` // pgdoc _id = projectId + "_" + serviceId
	ProjectID string     `json:"projectId"`
	ServiceID string     `json:"serviceId"`
	RGWUID    string     `json:"rgwUid"` // the project's RGW user (uidPrefix + projectId)
	AccessKey string     `json:"accessKey"`
	SecretKey string     `json:"secretKey"` // encrypted at rest, decrypted on read
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// Repo persists ceph credentials (secret key encrypted via the shared textcrypt key).
type Repo struct {
	col *pgdoc.Store
	enc *textcrypt.Encryptor
}

func New(db *pgdoc.DB, enc *textcrypt.Encryptor) *Repo {
	return &Repo{col: db.C("cephRgwCredential"), enc: enc}
}

func credID(projectID, serviceID string) string { return projectID + "_" + serviceID }

// Save upserts the credential (encrypting the secret key). Idempotent per (projectId, serviceId), so a
// re-provision / key rotation replaces in place without ever touching the project doc.
func (r *Repo) Save(ctx context.Context, c *Credential) error {
	c.ID = credID(c.ProjectID, c.ServiceID)
	if c.CreatedAt == nil {
		now := time.Now().UTC()
		c.CreatedAt = &now
	}
	stored := *c
	stored.SecretKey = r.enc.Encrypt(c.SecretKey)
	return r.col.Upsert(ctx, c.ID, &stored)
}

// Get loads the (decrypted) credential for a project on a ceph-s3 service, or nil when absent.
func (r *Repo) Get(ctx context.Context, projectID, serviceID string) (*Credential, error) {
	var c Credential
	found, err := r.col.Get(ctx, credID(projectID, serviceID), &c)
	if err != nil || !found {
		return nil, err
	}
	c.SecretKey = r.enc.Decrypt(c.SecretKey)
	return &c, nil
}

// Delete removes a project's stored credential (deprovision).
func (r *Repo) Delete(ctx context.Context, projectID, serviceID string) error {
	_, err := r.col.DeleteByID(ctx, credID(projectID, serviceID))
	return err
}
