package client

// objectstore_ceph_settings.go — the per-bucket configuration surface behind the client "Bucket settings"
// panel (the same fields Ceph's own dashboard shows). Every capability here was verified against a live
// RGW before being exposed; the ones that are NOT exposed, and why:
//
//   - Encryption (SSE-S3): PutBucketEncryption returns 200 on a cluster with no KMS backend, and then
//     every PutObject fails 400 InvalidRequest — the bucket is bricked for writes. Needs
//     rgw_crypt_sse_s3_backend (Vault) first, then a write+HeadObject probe. Deliberately absent.
//   - Replication: requires a multisite zonegroup; this is a single-zone cluster (rules=0).
//   - MFA Delete: per-user TOTP provisioned out of band (radosgw-admin mfa). Out of scope.
//   - Index type / placement rule: fixed by the placement target at bucket creation → READ-ONLY here.

import (
	"context"
	"fmt"
	"strings"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Versioning states.
const (
	VersioningEnabled   = "Enabled"
	VersioningSuspended = "Suspended"
	VersioningDisabled  = "Disabled" // never enabled; S3 has no way back to this once enabled
)

// Object-lock retention modes. COMPLIANCE is deliberately NOT offered — see SetObjectLockDefaults.
const (
	ObjectLockGovernance = "GOVERNANCE"
	ObjectLockCompliance = "COMPLIANCE"
)

// CreateBucketOpts are the create-TIME options. ObjectLockEnabled cannot be turned on later (S3 rule), so
// it has to be decided here; enabling it also forces versioning on.
type CreateBucketOpts struct {
	Name              string
	ObjectLockEnabled bool
}

// ObjectLockSettings is a bucket's default retention (nil when object lock is not enabled).
type ObjectLockSettings struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode,omitempty"` // GOVERNANCE
	Days    int32  `json:"days,omitempty"`
}

// BucketQuota is the RGW per-bucket quota (Admin Ops, not S3). -1 means unlimited.
type BucketQuota struct {
	Enabled    bool  `json:"enabled"`
	MaxSize    int64 `json:"maxSizeBytes"`
	MaxObjects int64 `json:"maxObjects"`
}

// LifecycleRule is the subset of S3 lifecycle we expose (expiration-based cleanup).
type LifecycleRule struct {
	ID             string `json:"id"`
	Prefix         string `json:"prefix,omitempty"`
	Enabled        bool   `json:"enabled"`
	ExpirationDays int32  `json:"expirationDays,omitempty"`
	// NoncurrentDays expires OLD VERSIONS; only meaningful on a versioned bucket.
	NoncurrentDays int32 `json:"noncurrentVersionExpirationDays,omitempty"`
	// AbortIncompleteMultipartDays reclaims space from uploads that were never completed.
	AbortIncompleteMultipartDays int32 `json:"abortIncompleteMultipartUploadDays,omitempty"`
}

// CORSRule is the subset of S3 CORS we expose.
type CORSRule struct {
	AllowedMethods []string `json:"allowedMethods"`
	AllowedOrigins []string `json:"allowedOrigins"`
	AllowedHeaders []string `json:"allowedHeaders,omitempty"`
	ExposeHeaders  []string `json:"exposeHeaders,omitempty"`
	MaxAgeSeconds  int32    `json:"maxAgeSeconds,omitempty"`
}

// BucketSettings is the full read-back of a bucket's configuration.
type BucketSettings struct {
	Versioning    string              `json:"versioning"`
	ObjectLock    *ObjectLockSettings `json:"objectLock,omitempty"`
	Quota         BucketQuota         `json:"quota"`
	Lifecycle     []LifecycleRule     `json:"lifecycle"`
	CORS          []CORSRule          `json:"cors"`
	Tags          map[string]string   `json:"tags"`
	Grants        []BucketGrant       `json:"grants"`
	PolicyJSON    string              `json:"policyJson,omitempty"`
	Website       *BucketWebsite      `json:"website,omitempty"`
	IndexType     string              `json:"indexType,omitempty"`     // read-only
	PlacementRule string              `json:"placementRule,omitempty"` // read-only
}

// notFound reports whether an S3 error means "this configuration was never set" (not a real failure).
func notFound(err error, codes ...string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, c := range codes {
		if strings.Contains(msg, c) {
			return true
		}
	}
	return false
}

// --- versioning ---

// SetBucketVersioning enables or suspends versioning. S3 has no "off" once enabled — suspending stops new
// versions but keeps the existing ones (which continue to consume billable space).
func (c *Client) SetBucketVersioning(ctx context.Context, bucket string, enabled bool) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	if err := c.ceph.needS3(); err != nil {
		return err
	}
	status := s3types.BucketVersioningStatusSuspended
	if enabled {
		status = s3types.BucketVersioningStatusEnabled
	}
	_, err := c.ceph.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: &bucket, VersioningConfiguration: &s3types.VersioningConfiguration{Status: status},
	})
	return err
}

// --- object lock ---

// SetObjectLockDefaults sets the bucket's default retention. Only GOVERNANCE is accepted:
// COMPLIANCE objects cannot be deleted by ANYONE until retention expires, which would make the project's
// RGW user un-purgeable and therefore block project deletion entirely. Requires a bucket that was CREATED
// with object lock enabled.
func (c *Client) SetObjectLockDefaults(ctx context.Context, bucket, mode string, days int32) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	if err := c.ceph.needS3(); err != nil {
		return err
	}
	if strings.EqualFold(mode, ObjectLockCompliance) {
		return fmt.Errorf("ceph-s3: COMPLIANCE retention is not offered — it would prevent the project from ever being deleted; use GOVERNANCE")
	}
	if !strings.EqualFold(mode, ObjectLockGovernance) {
		return fmt.Errorf("ceph-s3: unknown object lock mode %q", mode)
	}
	if days <= 0 {
		return fmt.Errorf("ceph-s3: object lock retention days must be > 0")
	}
	_, err := c.ceph.s3.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
		Bucket: &bucket,
		ObjectLockConfiguration: &s3types.ObjectLockConfiguration{
			ObjectLockEnabled: s3types.ObjectLockEnabledEnabled,
			Rule: &s3types.ObjectLockRule{DefaultRetention: &s3types.DefaultRetention{
				Mode: s3types.ObjectLockRetentionModeGovernance, Days: awsv2.Int32(days),
			}},
		},
	})
	return err
}

// --- quota (RGW Admin Ops) ---

// SetBucketQuota caps a single bucket's size and/or object count. maxSize/maxObjects <= 0 mean unlimited.
// Uses the ADMIN keys (this is not an S3 call), so it works even on a client without project data keys.
func (c *Client) SetBucketQuota(ctx context.Context, bucket string, maxSizeBytes, maxObjects int64, enabled bool) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	// Admin-by-name mutation → prove ownership first (finding F3): a stale/recycled name must not let this
	// project set a quota on another project's bucket.
	if _, err := c.ceph.adminBucketInfo(ctx, bucket); err != nil {
		return err
	}
	size, objs := "-1", "-1"
	if maxSizeBytes > 0 {
		size = fmt.Sprint(maxSizeBytes)
	}
	if maxObjects > 0 {
		objs = fmt.Sprint(maxObjects)
	}
	q := urlValues(
		"quota", "",
		"bucket", bucket,
		"uid", c.ceph.uid,
		"max-size", size,
		"max-objects", objs,
		"enabled", fmt.Sprint(enabled),
	)
	return c.ceph.adminDo(ctx, "PUT", "/bucket", q, nil, nil)
}

// --- lifecycle ---

// SetBucketLifecycle replaces the bucket's lifecycle rules; an empty list removes the configuration.
func (c *Client) SetBucketLifecycle(ctx context.Context, bucket string, rules []LifecycleRule) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	if err := c.ceph.needS3(); err != nil {
		return err
	}
	if len(rules) == 0 {
		_, err := c.ceph.s3.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: &bucket})
		return err
	}
	out := make([]s3types.LifecycleRule, 0, len(rules))
	for i := range rules {
		r := rules[i]
		if r.ID == "" {
			return fmt.Errorf("ceph-s3: lifecycle rule %d has no id", i)
		}
		status := s3types.ExpirationStatusDisabled
		if r.Enabled {
			status = s3types.ExpirationStatusEnabled
		}
		lr := s3types.LifecycleRule{
			ID: awsv2.String(r.ID), Status: status,
			Filter: &s3types.LifecycleRuleFilter{Prefix: awsv2.String(r.Prefix)},
		}
		if r.ExpirationDays > 0 {
			lr.Expiration = &s3types.LifecycleExpiration{Days: awsv2.Int32(r.ExpirationDays)}
		}
		if r.NoncurrentDays > 0 {
			lr.NoncurrentVersionExpiration = &s3types.NoncurrentVersionExpiration{NoncurrentDays: awsv2.Int32(r.NoncurrentDays)}
		}
		if r.AbortIncompleteMultipartDays > 0 {
			lr.AbortIncompleteMultipartUpload = &s3types.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: awsv2.Int32(r.AbortIncompleteMultipartDays),
			}
		}
		out = append(out, lr)
	}
	_, err := c.ceph.s3.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: &bucket, LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: out},
	})
	return err
}

// --- CORS ---

// SetBucketCORS replaces the bucket's CORS rules; an empty list removes the configuration.
func (c *Client) SetBucketCORS(ctx context.Context, bucket string, rules []CORSRule) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	if err := c.ceph.needS3(); err != nil {
		return err
	}
	if len(rules) == 0 {
		_, err := c.ceph.s3.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: &bucket})
		return err
	}
	out := make([]s3types.CORSRule, 0, len(rules))
	for _, r := range rules {
		cr := s3types.CORSRule{
			AllowedMethods: r.AllowedMethods, AllowedOrigins: r.AllowedOrigins,
			AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders,
		}
		if r.MaxAgeSeconds > 0 {
			cr.MaxAgeSeconds = awsv2.Int32(r.MaxAgeSeconds)
		}
		out = append(out, cr)
	}
	_, err := c.ceph.s3.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: &bucket, CORSConfiguration: &s3types.CORSConfiguration{CORSRules: out},
	})
	return err
}

// --- tagging ---

// SetBucketTags replaces the bucket's tag set; an empty map removes it.
func (c *Client) SetBucketTags(ctx context.Context, bucket string, tags map[string]string) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	if err := c.ceph.needS3(); err != nil {
		return err
	}
	if len(tags) == 0 {
		_, err := c.ceph.s3.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: &bucket})
		return err
	}
	set := make([]s3types.Tag, 0, len(tags))
	for k, v := range tags {
		set = append(set, s3types.Tag{Key: awsv2.String(k), Value: awsv2.String(v)})
	}
	_, err := c.ceph.s3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket: &bucket, Tagging: &s3types.Tagging{TagSet: set},
	})
	return err
}

// --- read-back ---

// GetBucketSettings reads the whole configuration panel in one shot. Every leg is best-effort: an absent
// configuration (no lifecycle, no CORS, …) is a normal state, not an error.
func (c *Client) GetBucketSettings(ctx context.Context, bucket string) (*BucketSettings, error) {
	if c.ceph == nil {
		return nil, ErrBucketFeatureUnsupported
	}
	if err := c.ceph.needS3(); err != nil {
		return nil, err
	}
	s := &BucketSettings{Versioning: VersioningDisabled, Lifecycle: []LifecycleRule{}, CORS: []CORSRule{}, Tags: map[string]string{}, Grants: []BucketGrant{}}

	if v, err := c.ceph.s3.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &bucket}); err == nil && v.Status != "" {
		s.Versioning = string(v.Status)
	}
	if ol, err := c.ceph.s3.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: &bucket}); err == nil && ol.ObjectLockConfiguration != nil {
		l := &ObjectLockSettings{Enabled: ol.ObjectLockConfiguration.ObjectLockEnabled == s3types.ObjectLockEnabledEnabled}
		if r := ol.ObjectLockConfiguration.Rule; r != nil && r.DefaultRetention != nil {
			l.Mode = string(r.DefaultRetention.Mode)
			l.Days = awsv2.ToInt32(r.DefaultRetention.Days)
		}
		s.ObjectLock = l
	} else if err != nil && !notFound(err, "ObjectLockConfigurationNotFound", "NoSuchObjectLockConfiguration", "InvalidRequest") {
		return nil, err
	}
	if lc, err := c.ceph.s3.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &bucket}); err == nil {
		for _, r := range lc.Rules {
			lr := LifecycleRule{ID: awsv2.ToString(r.ID), Enabled: r.Status == s3types.ExpirationStatusEnabled}
			if r.Filter != nil {
				lr.Prefix = awsv2.ToString(r.Filter.Prefix)
			}
			if r.Expiration != nil {
				lr.ExpirationDays = awsv2.ToInt32(r.Expiration.Days)
			}
			if r.NoncurrentVersionExpiration != nil {
				lr.NoncurrentDays = awsv2.ToInt32(r.NoncurrentVersionExpiration.NoncurrentDays)
			}
			if r.AbortIncompleteMultipartUpload != nil {
				lr.AbortIncompleteMultipartDays = awsv2.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation)
			}
			s.Lifecycle = append(s.Lifecycle, lr)
		}
	}
	if cs, err := c.ceph.s3.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: &bucket}); err == nil {
		for _, r := range cs.CORSRules {
			s.CORS = append(s.CORS, CORSRule{
				AllowedMethods: r.AllowedMethods, AllowedOrigins: r.AllowedOrigins,
				AllowedHeaders: r.AllowedHeaders, ExposeHeaders: r.ExposeHeaders,
				MaxAgeSeconds: awsv2.ToInt32(r.MaxAgeSeconds),
			})
		}
	}
	if tg, err := c.ceph.s3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &bucket}); err == nil {
		for _, t := range tg.TagSet {
			s.Tags[awsv2.ToString(t.Key)] = awsv2.ToString(t.Value)
		}
	}
	if grants, err := c.ListBucketGrants(ctx, bucket); err == nil {
		s.Grants = grants
	}
	if pol, err := c.GetBucketPolicyJSON(ctx, bucket); err == nil {
		s.PolicyJSON = pol
	}
	if site, err := c.GetBucketWebsite(ctx, bucket); err == nil {
		s.Website = site
	}
	// Quota + the read-only placement fields come from Admin Ops (admin keys).
	if st, err := c.ceph.adminBucketInfo(ctx, bucket); err == nil {
		s.Quota = BucketQuota{Enabled: st.BucketQuota.Enabled, MaxSize: st.BucketQuota.MaxSize, MaxObjects: st.BucketQuota.MaxObjects}
		s.IndexType, s.PlacementRule = st.IndexType, st.PlacementRule
	}
	return s, nil
}
