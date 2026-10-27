package cephcred

// s3key.go — additional per-project S3 access keys ("IAM keys"). RGW has no notion of attaching a key to a
// bucket: keys belong to USERS, and a bucket grants access to a PRINCIPAL. So an extra key IS an extra RGW
// user (uid = "<projectUid>-<name>"), and "assign key to bucket" = a bucket-policy statement naming that
// user's ARN (see client.GrantBucketAccess).
//
// The project's OWN key lives in Credential (cephcred.go) and is never listed here.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const s3KeyCollection = "cephS3Key"

// keyNamePattern constrains the customer-supplied name: it becomes part of an RGW uid and of policy ARNs.
var keyNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

// ValidateKeyName rejects names that would produce an unusable or ambiguous RGW uid.
func ValidateKeyName(name string) error {
	if !keyNamePattern.MatchString(name) {
		return fmt.Errorf("key name must be 3-32 chars, lowercase letters, digits and hyphens, not starting or ending with a hyphen")
	}
	return nil
}

// S3Key is one cephS3Key document — an extra access key the customer can hand to an app or a person.
type S3Key struct {
	ID        string     `json:"id,omitempty"`
	ProjectID string     `json:"projectId"`
	ServiceID string     `json:"serviceId"`
	Name      string     `json:"name"`
	RGWUID    string     `json:"rgwUid"`
	AccessKey string     `json:"accessKey"`
	SecretKey string     `json:"secretKey"` // encrypted at rest, decrypted on read
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// KeyRepo persists the extra S3 keys (secret encrypted with the shared textcrypt key).
type KeyRepo struct {
	col *pgdoc.Store
	enc *textcrypt.Encryptor
}

func NewKeyRepo(db *pgdoc.DB, enc *textcrypt.Encryptor) *KeyRepo {
	return &KeyRepo{col: db.C(s3KeyCollection), enc: enc}
}

// keyID is deterministic per (project, service, name) so a repeated create is an upsert, not a duplicate.
func keyID(projectID, serviceID, name string) string {
	return projectID + "_" + serviceID + "_" + name
}

// Save upserts the key, encrypting the secret.
func (r *KeyRepo) Save(ctx context.Context, k *S3Key) error {
	k.ID = keyID(k.ProjectID, k.ServiceID, k.Name)
	if k.CreatedAt == nil {
		now := time.Now().UTC()
		k.CreatedAt = &now
	}
	stored := *k
	stored.SecretKey = r.enc.Encrypt(k.SecretKey)
	return r.col.Upsert(ctx, k.ID, &stored)
}

// Get loads one key by its document id, decrypted. Returns nil when absent.
func (r *KeyRepo) Get(ctx context.Context, id string) (*S3Key, error) {
	var k S3Key
	found, err := r.col.Get(ctx, id, &k)
	if err != nil || !found {
		return nil, err
	}
	k.SecretKey = r.enc.Decrypt(k.SecretKey)
	return &k, nil
}

// GetOwned loads a key and verifies it belongs to the given project + service — a request body must never
// reach another project's key by id.
func (r *KeyRepo) GetOwned(ctx context.Context, id, projectID, serviceID string) (*S3Key, error) {
	k, err := r.Get(ctx, id)
	if err != nil || k == nil {
		return nil, err
	}
	if k.ProjectID != projectID || k.ServiceID != serviceID {
		return nil, nil
	}
	return k, nil
}

// List returns the project's extra keys on a service, decrypted, oldest first (createdAt, with the
// deterministic _id as tiebreaker — _id alone is name order, not creation order).
func (r *KeyRepo) List(ctx context.Context, projectID, serviceID string) ([]S3Key, error) {
	var keys []S3Key
	err := r.col.Find(ctx, pgdoc.M{"projectId": projectID, "serviceId": serviceID}, &keys,
		pgdoc.Sort(pgdoc.Asc("createdAt"), pgdoc.Asc("_id")))
	if err != nil {
		return nil, err
	}
	for i := range keys {
		keys[i].SecretKey = r.enc.Decrypt(keys[i].SecretKey)
	}
	return keys, nil
}

// Delete removes one key document.
func (r *KeyRepo) Delete(ctx context.Context, id string) error {
	_, err := r.col.DeleteByID(ctx, id)
	return err
}

// DeleteForProject removes every stored key of a project on a service (project teardown).
func (r *KeyRepo) DeleteForProject(ctx context.Context, projectID, serviceID string) error {
	_, err := r.col.DeleteMany(ctx, pgdoc.M{"projectId": projectID, "serviceId": serviceID})
	return err
}

// ChildUID derives the RGW uid for a key name under a project's user. Mirrors client.Client.ChildUID; kept
// here so the repo can be used without a live cloud client.
func ChildUID(projectUID, name string) string { return projectUID + "-" + name }

// SplitChildName returns the key name from a child uid, or "" when uid is not a child of projectUID.
func SplitChildName(projectUID, uid string) string {
	prefix := projectUID + "-"
	if !strings.HasPrefix(uid, prefix) {
		return ""
	}
	return strings.TrimPrefix(uid, prefix)
}
