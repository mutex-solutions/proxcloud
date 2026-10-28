package providers

// write.go — the ceph-s3 BUCKET write service. Buckets are the only resource
// family that writes through this seam (Proxmox has its own pve.Service path, kamaji
// and dbaas their own services). The interface stays injectable so bucket writes
// are testable against a fake.

import (
	"context"
	"fmt"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
)

// Writer is the cloud-side surface WriteService needs — the bucket subset,
// satisfied by *client.Client (ceph-s3).
type Writer interface {
	CreateBucket(ctx context.Context, o client.CreateBucketOpts) (map[string]any, error)
	GetBucket(ctx context.Context, name string) (map[string]any, error)
	ListBuckets(ctx context.Context) ([]map[string]any, error)
	DeleteBucket(ctx context.Context, name string) error
}

// WriteService creates/deletes buckets and keeps the CloudResource cache in step.
type WriteService struct {
	w    Writer
	repo *cloud.Repo
}

func NewWriteService(w Writer, repo *cloud.Repo) *WriteService {
	return &WriteService{w: w, repo: repo}
}

// CreateRequest is the client create body: {type, data}.
type CreateRequest struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

func mstr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func mbool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// Create provisions one resource and caches it.
func (s *WriteService) Create(ctx context.Context, serviceID, region, projectID, userID string, req CreateRequest) (*cloud.CloudResource, error) {
	now := time.Now().UTC()
	cr := &cloud.CloudResource{
		ServiceID: serviceID, Region: region, ProjectID: projectID, Type: req.Type,
		CreatedAt: &now, UpdatedAt: &now,
	}
	d := req.Data
	if d == nil {
		d = map[string]any{}
	}
	switch req.Type {
	case cloud.TypeBucket:
		// FE: data{bucketName, objectLockEnabled?}. externalId = the bucket name;
		// data = DataBucket{bucketName, objectCount:0, sizeInGb:0, sizeInBytes:0,
		// storageBackend}. objectLockEnabled is CREATE-TIME ONLY (S3 forbids
		// enabling it later).
		name := mstr(d, "bucketName")
		if name == "" {
			return nil, fmt.Errorf("bucket name is required")
		}
		bucket, err := s.w.CreateBucket(ctx, client.CreateBucketOpts{
			Name: name, ObjectLockEnabled: mbool(d, "objectLockEnabled"),
		})
		if err != nil {
			return nil, err
		}
		cr.ExternalID = name
		cr.Data = bucket
	default:
		return nil, fmt.Errorf("unsupported create type %q", req.Type)
	}
	saved, err := s.repo.Insert(ctx, cr)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// Delete removes the cloud object of a CACHED resource and archives its row.
// A missing cache row is a no-op (the id resolution happened upstream).
func (s *WriteService) Delete(ctx context.Context, serviceID, externalID string) error {
	cr, err := s.repo.FindByServiceIDAndExternalID(ctx, serviceID, externalID)
	if err != nil {
		return err
	}
	if cr == nil {
		return nil
	}
	if err := s.deleteCloudObject(ctx, cr); err != nil {
		return err
	}
	return s.repo.DeleteAndArchive(ctx, cr, time.Now().UTC())
}

func (s *WriteService) deleteCloudObject(ctx context.Context, cr *cloud.CloudResource) error {
	switch cr.Type {
	case cloud.TypeBucket:
		return s.w.DeleteBucket(ctx, cr.ExternalID)
	default:
		return fmt.Errorf("unsupported delete type %q", cr.Type)
	}
}
