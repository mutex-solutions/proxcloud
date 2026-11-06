package client

// objectstore_rustfs.go = the RustFS backend (provider:"rustfs"), the tenant S3 product on Standard-tier sites.
// Objects, versioning, lifecycle, CORS, tags and bucket policies are plain S3, so it embeds cephBackend's data
// plane. What differs, because RustFS has no bucket owner and no per-user quota:
//   - Ownership = the bucket's ARN in the project's canned IAM policy. Buckets are created with the root key;
//     tenants cannot create or delete them directly (explicit Deny), so the policy stays the truth.
//   - Project quota = the sum of the bucket quotas, enforced here. RustFS quota is HARD but fail-open, so it
//     is not a billing guarantee.
//   - Usage comes from the scanner (datausageinfo): delayed, and sometimes unavailable. Unknown usage is
//     flagged, never written as 0 (see providers.BucketProvider.MergeCached).
//   - Extra keys are plain IAM users with no identity policy, granted through bucket policies. Lab check
//     RUSTFS-POLICY-1 on 1.0.4: a bucket-policy principal matches the BARE user name only; the ARN form that
//     RGW wants is accepted and silently matches nothing.
//
// Admin API: /rustfs/admin/v3, SigV4 service "s3", plain JSON bodies. madmin-go is AGPL: do not link it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// rustfsPolicyMu serialises read-modify-write of canned policies.
// ponytail: one API process only; a pgdoc advisory lock per project if several replicas race on bucket create.
var rustfsPolicyMu sync.Mutex

type rustfsBackend struct {
	*cephBackend
	adminS3 *s3.Client // root-keyed: bucket create/delete and teardown
}

// NewRustFS builds a *client.Client on RustFS. AdminURL is the private endpoint the API reaches the admin paths
// on (the public proxy blocks them); it defaults to S3Endpoint. Empty project keys = admin-only client.
func NewRustFS(_ context.Context, cfg ObjectStoreConfig) (*Client, error) {
	base := strings.TrimRight(cfg.AdminURL, "/")
	if base == "" {
		base = strings.TrimRight(cfg.S3Endpoint, "/")
	}
	if base == "" {
		return nil, fmt.Errorf("cloud: rustfs requires s3Endpoint or adminApiUrl")
	}
	cfg.AdminURL = base + "/rustfs/admin/v3"
	b, err := newCephBackend(cfg)
	if err != nil {
		return nil, err
	}
	b.bareUserGrants = true
	return &Client{objectBackend: &rustfsBackend{
		cephBackend: b,
		adminS3: s3.New(s3.Options{
			Region:                     b.region,
			Credentials:                credentials.NewStaticCredentialsProvider(cfg.AdminAccessKey, cfg.AdminSecretKey, ""),
			BaseEndpoint:               awsv2.String(base),
			UsePathStyle:               true,
			RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
		}),
	}}, nil
}

// principalUserNames names users the way RustFS matches them (RUSTFS-POLICY-1).
func principalUserNames(uids ...string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"AWS": uids})
	return b
}

// Caps: website serving needs per-bucket DNS and is gated off; quota is bytes only.
func (r *rustfsBackend) Caps() ObjectCaps {
	return ObjectCaps{Backend: BackendRustFS, Website: false, QuotaObjects: false}
}

func newRustFSSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// adminJSON is adminDo with a JSON-encoded body.
func (r *rustfsBackend) adminJSON(ctx context.Context, method, path string, q url.Values, doc, out any) error {
	var body []byte
	if doc != nil {
		var err error
		if body, err = json.Marshal(doc); err != nil {
			return err
		}
	}
	return r.adminDo(ctx, method, path, q, body, out)
}

// --- identity ---

// setUserSecret creates the user, or replaces its secret when it exists (the access key never changes).
func (r *rustfsBackend) setUserSecret(ctx context.Context, uid string) (string, error) {
	secret, err := newRustFSSecret()
	if err != nil {
		return "", err
	}
	if err := r.adminJSON(ctx, "PUT", "/add-user", urlValues("accessKey", uid), map[string]string{"secretKey": secret, "status": "enabled"}, nil); err != nil {
		return "", fmt.Errorf("rustfs: set user %s: %w", uid, err)
	}
	return secret, nil
}

// EnsureProjectUser provisions the project's IAM user and its canned policy. RustFS never returns a secret,
// so a re-provision of an existing user issues a new one; the caller stores what this returns.
func (r *rustfsBackend) EnsureProjectUser(ctx context.Context, _ string) (string, string, error) {
	secret, err := r.setUserSecret(ctx, r.uid)
	if err != nil {
		return "", "", err
	}
	rustfsPolicyMu.Lock()
	defer rustfsPolicyMu.Unlock()
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return "", "", err
	}
	if err := r.putProjectPolicy(ctx, names); err != nil {
		return "", "", err
	}
	return r.uid, secret, nil
}

// PurgeProjectUser removes the project's buckets (data included), its user and its policy.
func (r *rustfsBackend) PurgeProjectUser(ctx context.Context) error {
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := r.forceDelete(ctx, name); err != nil {
			return err
		}
	}
	if err := r.adminDo(ctx, "DELETE", "/remove-user", urlValues("accessKey", r.uid), nil, nil); err != nil && !isAdminNotFound(err) {
		return err
	}
	return r.adminDo(ctx, "DELETE", "/remove-canned-policy", urlValues("name", r.uid), nil, nil)
}

// CreateChildKey creates an IAM user with no identity policy: it reaches nothing until a bucket grants it.
func (r *rustfsBackend) CreateChildKey(ctx context.Context, uid, _ string) (string, string, error) {
	if err := r.assertOwnedUID(uid); err != nil {
		return "", "", err
	}
	secret, err := r.setUserSecret(ctx, uid)
	if err != nil {
		return "", "", err
	}
	return uid, secret, nil
}

func (r *rustfsBackend) DeleteChildKey(ctx context.Context, uid string) error {
	if err := r.assertOwnedUID(uid); err != nil {
		return err
	}
	err := r.adminDo(ctx, "DELETE", "/remove-user", urlValues("accessKey", uid), nil, nil)
	if isAdminNotFound(err) {
		return nil
	}
	return err
}

// RotateKey issues a new secret under the same access key (RGW issues a new pair). The old secret stops
// working at once; grants and policies are untouched.
func (r *rustfsBackend) RotateKey(ctx context.Context, uid, _ string) (string, string, error) {
	if uid != r.uid {
		if err := r.assertOwnedUID(uid); err != nil {
			return "", "", err
		}
	}
	secret, err := r.setUserSecret(ctx, uid)
	if err != nil {
		return "", "", err
	}
	return uid, secret, nil
}

// --- ownership: the project's canned policy ---

const s3ARNPrefix = "arn:aws:s3:::"

type rustfsPolicyStmt struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// policyBuckets returns the bucket names a canned policy grants (the Allow statement's bare-bucket ARNs).
func policyBuckets(stmts []rustfsPolicyStmt) []string {
	out := []string{}
	for _, st := range stmts {
		if st.Effect != "Allow" || len(st.Action) != 1 || st.Action[0] != "s3:*" {
			continue
		}
		for _, res := range st.Resource {
			if name, ok := strings.CutPrefix(res, s3ARNPrefix); ok && !strings.Contains(name, "/") {
				out = append(out, name)
			}
		}
	}
	return out
}

// projectPolicyDoc is the project user's identity policy: full S3 on its own buckets, and no bucket
// create/delete anywhere. That Deny is the ownership guard: a tenant who could delete a bucket directly would
// leave its ARN behind, and the ARN would grant access to whoever gets the name next.
func projectPolicyDoc(names []string) map[string]any {
	stmts := []rustfsPolicyStmt{{
		Effect: "Deny", Action: []string{"s3:CreateBucket", "s3:DeleteBucket"}, Resource: []string{s3ARNPrefix + "*"},
	}}
	if len(names) > 0 {
		res := make([]string, 0, 2*len(names))
		for _, n := range names {
			res = append(res, s3ARNPrefix+n, s3ARNPrefix+n+"/*")
		}
		stmts = append(stmts, rustfsPolicyStmt{Effect: "Allow", Action: []string{"s3:*"}, Resource: res})
	}
	return map[string]any{"Version": "2012-10-17", "Statement": stmts}
}

func (r *rustfsBackend) putProjectPolicy(ctx context.Context, names []string) error {
	if err := r.adminJSON(ctx, "PUT", "/add-canned-policy", urlValues("name", r.uid), projectPolicyDoc(names), nil); err != nil {
		return fmt.Errorf("rustfs: put policy %s: %w", r.uid, err)
	}
	q := urlValues("policyName", r.uid, "userOrGroup", r.uid, "isGroup", "false")
	if err := r.adminDo(ctx, "PUT", "/set-user-or-group-policy", q, nil, nil); err != nil {
		return fmt.Errorf("rustfs: attach policy %s: %w", r.uid, err)
	}
	return nil
}

// projectBuckets returns the buckets this project owns. An error is an error, never "none": sync would
// otherwise archive every bucket row of the project on one transient failure.
func (r *rustfsBackend) projectBuckets(ctx context.Context) ([]string, error) {
	var info struct {
		Policy struct {
			Statement []rustfsPolicyStmt `json:"Statement"`
		} `json:"policy"`
	}
	err := r.adminDo(ctx, "GET", "/info-canned-policy", urlValues("name", r.uid), nil, &info)
	if isAdminNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rustfs: read policy %s: %w", r.uid, err)
	}
	return policyBuckets(info.Policy.Statement), nil
}

// assertOwnedBucket is the guard for every root-keyed call by name.
func (r *rustfsBackend) assertOwnedBucket(ctx context.Context, bucket string) error {
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return err
	}
	for _, n := range names {
		if n == bucket {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrBucketNotOwned, bucket)
}

// stripFromOtherPolicies drops a freshly created bucket from any other canned policy. Only the root key
// deletes buckets, but a custom bucket policy can hand s3:DeleteBucket to a child key, which leaves the old
// owner's ARN behind: without this the old owner could reach the bucket of whoever takes the name next.
func (r *rustfsBackend) stripFromOtherPolicies(ctx context.Context, bucket string) error {
	all := map[string]struct {
		Version   string             `json:"Version"`
		Statement []rustfsPolicyStmt `json:"Statement"`
	}{}
	// bucket= is required next to adminDo's format=json, or RustFS answers 400 "failed to decode query".
	if err := r.adminDo(ctx, "GET", "/list-canned-policies", urlValues("bucket", ""), nil, &all); err != nil {
		return fmt.Errorf("rustfs: list policies: %w", err)
	}
	for name, doc := range all {
		// Only project policies: they lead with the Deny that projectPolicyDoc writes.
		isProject := len(doc.Statement) > 0 && doc.Statement[0].Effect == "Deny"
		if name == r.uid || !isProject || !adminValueRe.MatchString(name) {
			continue
		}
		names := policyBuckets(doc.Statement)
		kept := names[:0]
		for _, n := range names {
			if n != bucket {
				kept = append(kept, n)
			}
		}
		if len(kept) == len(names) {
			continue
		}
		stale := &rustfsBackend{cephBackend: &cephBackend{uid: name, adminURL: r.adminURL, adminCreds: r.adminCreds, signer: r.signer, http: r.http, region: r.region}}
		if err := stale.adminJSON(ctx, "PUT", "/add-canned-policy", urlValues("name", name), projectPolicyDoc(kept), nil); err != nil {
			return fmt.Errorf("rustfs: clean policy %s: %w", name, err)
		}
	}
	return nil
}

// --- buckets ---

// CreateBucket creates the bucket with the root key and then grants it to the project. With a project quota,
// the bucket gets what is left of it as its own HARD quota (lower it later to make room for another bucket).
func (r *rustfsBackend) CreateBucket(ctx context.Context, o CreateBucketOpts) (map[string]any, error) {
	if !adminValueRe.MatchString(o.Name) {
		return nil, fmt.Errorf("rustfs: invalid bucket name %q", o.Name)
	}
	rustfsPolicyMu.Lock()
	defer rustfsPolicyMu.Unlock()
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return nil, err
	}
	var quota int64
	if r.quotaBytes > 0 {
		if quota, err = r.quotaLeft(ctx, names, ""); err != nil {
			return nil, err
		}
		if quota <= 0 {
			return nil, fmt.Errorf("%w: lower the quota of another bucket first", ErrProjectQuotaExceeded)
		}
	}
	// The root key owns every bucket, so RustFS answers 200 to a create on a name that exists: without this
	// check the next project would take over the bucket and its data.
	if _, err := r.adminS3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &o.Name}); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrBucketNameTaken, o.Name)
	} else if !notFound(err, "NotFound", "NoSuchBucket", "StatusCode: 404") {
		return nil, fmt.Errorf("rustfs: check bucket %s: %w", o.Name, err)
	}
	in := &s3.CreateBucketInput{Bucket: &o.Name}
	if o.ObjectLockEnabled {
		in.ObjectLockEnabledForBucket = awsv2.Bool(true)
	}
	if _, err := r.adminS3.CreateBucket(ctx, in); err != nil {
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return nil, fmt.Errorf("%w: %s", ErrBucketNameTaken, o.Name)
		}
		return nil, err
	}
	err = r.stripFromOtherPolicies(ctx, o.Name)
	if err == nil {
		err = r.putProjectPolicy(ctx, append(names, o.Name))
	}
	if err == nil && quota > 0 {
		err = r.putBucketQuota(ctx, o.Name, quota)
	}
	if err != nil {
		// Half a bucket would be unowned and unbilled.
		_, _ = r.adminS3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &o.Name})
		return nil, err
	}
	d := bucketData(BackendRustFS, o.Name, 0, 0)
	if quota > 0 {
		d["quotaBytes"] = quota
	}
	return d, nil
}

// rustfsUsage is the scanner's per-bucket snapshot; nil means unavailable (503 while scanning, rustfs#8924).
type rustfsUsage map[string]struct {
	Size    int64 `json:"size"`
	Objects int64 `json:"objects_count"`
}

func (r *rustfsBackend) usage(ctx context.Context) rustfsUsage {
	var snap struct {
		Buckets rustfsUsage `json:"buckets_usage"`
	}
	if err := r.adminDo(ctx, "GET", "/datausageinfo", nil, nil, &snap); err != nil {
		return nil
	}
	return snap.Buckets
}

// data builds a bucket's cache row. usageUnknown tells the sync to keep the last value instead of writing 0.
func (r *rustfsBackend) data(name string, use rustfsUsage, quota int64) map[string]any {
	u, known := use[name]
	d := bucketData(BackendRustFS, name, u.Objects, u.Size)
	if !known {
		d["usageUnknown"] = true
	}
	if quota > 0 {
		d["quotaBytes"] = quota
	}
	return d
}

// GetBucket reads one bucket's usage and quota.
func (r *rustfsBackend) GetBucket(ctx context.Context, name string) (map[string]any, error) {
	if err := r.assertOwnedBucket(ctx, name); err != nil {
		return nil, err
	}
	quota, err := r.bucketQuota(ctx, name)
	if err != nil {
		return nil, err
	}
	return r.data(name, r.usage(ctx), quota), nil
}

// ListBuckets is the sync/billing read. A bucket whose quota cannot be read still lists, with its usage flagged
// unknown; the row must not vanish on a transient error. One that is really gone is dropped from the policy.
func (r *rustfsBackend) ListBuckets(ctx context.Context) ([]map[string]any, error) {
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return nil, err
	}
	use := r.usage(ctx)
	out := make([]map[string]any, 0, len(names))
	live := make([]string, 0, len(names))
	for _, name := range names {
		quota, err := r.bucketQuota(ctx, name)
		if isAdminNotFound(err) {
			continue
		}
		live = append(live, name)
		d := r.data(name, use, quota)
		if err != nil {
			d["usageUnknown"] = true
		}
		out = append(out, d)
	}
	if len(live) != len(names) {
		rustfsPolicyMu.Lock()
		defer rustfsPolicyMu.Unlock()
		if err := r.putProjectPolicy(ctx, live); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteBucket refuses a non-empty bucket (S3 semantics) and then takes the ARN out of the policy.
func (r *rustfsBackend) DeleteBucket(ctx context.Context, name string) error {
	if err := r.assertOwnedBucket(ctx, name); err != nil {
		return err
	}
	if _, err := r.adminS3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &name}); err != nil && !notFound(err, "NoSuchBucket") {
		if notFound(err, "BucketNotEmpty") {
			return ErrBucketNotEmpty
		}
		return err
	}
	return r.dropFromPolicy(ctx, name)
}

// ForceDeleteBucket is the teardown path: bucket and objects go. A name this project does not own is a no-op,
// so a stale row can never destroy another tenant's bucket. Refused for Object Lock buckets by RustFS >= 1.0.2.
func (r *rustfsBackend) ForceDeleteBucket(ctx context.Context, bucket string) error {
	err := r.assertOwnedBucket(ctx, bucket)
	if errors.Is(err, ErrBucketNotOwned) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.forceDelete(ctx, bucket); err != nil {
		return err
	}
	return r.dropFromPolicy(ctx, bucket)
}

func (r *rustfsBackend) forceDelete(ctx context.Context, bucket string) error {
	_, err := r.adminS3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket},
		s3.WithAPIOptions(smithyhttp.AddHeaderValue("x-rustfs-force-delete", "true")))
	if err != nil && !notFound(err, "NoSuchBucket") {
		return err
	}
	return nil
}

func (r *rustfsBackend) dropFromPolicy(ctx context.Context, bucket string) error {
	rustfsPolicyMu.Lock()
	defer rustfsPolicyMu.Unlock()
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(names))
	for _, n := range names {
		if n != bucket {
			kept = append(kept, n)
		}
	}
	return r.putProjectPolicy(ctx, kept)
}

// --- quota ---

// bucketQuota returns a bucket's HARD quota in bytes, 0 when none is set. isAdminNotFound(err) = no such bucket.
func (r *rustfsBackend) bucketQuota(ctx context.Context, bucket string) (int64, error) {
	if !adminValueRe.MatchString(bucket) {
		return 0, fmt.Errorf("rustfs: invalid bucket name %q", bucket)
	}
	var q struct {
		Quota *int64 `json:"quota"`
	}
	if err := r.adminDo(ctx, "GET", "/quota/"+bucket, nil, nil, &q); err != nil {
		return 0, err
	}
	if q.Quota == nil {
		return 0, nil
	}
	return *q.Quota, nil
}

// bucketQuotaWait is bucketQuota for decisions: it waits out the 503 RustFS 1.0.4 answers ("authoritative bucket
// usage is not available yet") until the scanner has seen a new bucket, and fails closed after that.
func (r *rustfsBackend) bucketQuotaWait(ctx context.Context, bucket string) (int64, error) {
	var ae *adminError
	for range 8 {
		q, err := r.bucketQuota(ctx, bucket)
		if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
			return q, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return 0, ErrObjectStoreBusy
}

// putBucketQuota retries a 503 for a while: the API answers that right after a RustFS start (rustfs#8014).
func (r *rustfsBackend) putBucketQuota(ctx context.Context, bucket string, bytes int64) error {
	if !adminValueRe.MatchString(bucket) {
		return fmt.Errorf("rustfs: invalid bucket name %q", bucket)
	}
	doc := map[string]any{"quota": bytes, "quota_type": "HARD"}
	var err error
	for range 5 {
		err = r.adminJSON(ctx, "PUT", "/quota/"+bucket, nil, doc, nil)
		var ae *adminError
		if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return err
}

// quotaLeft is the project quota minus the quotas of its buckets other than `except`.
func (r *rustfsBackend) quotaLeft(ctx context.Context, names []string, except string) (int64, error) {
	left := r.quotaBytes
	for _, n := range names {
		if n == except {
			continue
		}
		q, err := r.bucketQuotaWait(ctx, n)
		if err != nil && !isAdminNotFound(err) {
			return 0, err
		}
		left -= q
	}
	return left, nil
}

// SetBucketQuota sets the bucket's size cap. An object-count cap does not exist on RustFS. With a project quota
// a bucket must keep a quota, and all of them together must fit in it.
func (r *rustfsBackend) SetBucketQuota(ctx context.Context, bucket string, maxSizeBytes, maxObjects int64, enabled bool) error {
	if enabled && maxObjects > 0 {
		return ErrBucketFeatureUnsupported
	}
	rustfsPolicyMu.Lock()
	defer rustfsPolicyMu.Unlock()
	names, err := r.projectBuckets(ctx)
	if err != nil {
		return err
	}
	owned := false
	for _, n := range names {
		owned = owned || n == bucket
	}
	if !owned {
		return fmt.Errorf("%w: %q", ErrBucketNotOwned, bucket)
	}
	if !enabled || maxSizeBytes <= 0 {
		if r.quotaBytes > 0 {
			return fmt.Errorf("%w: this project has a storage limit, so every bucket needs a quota", ErrProjectQuotaExceeded)
		}
		err := r.adminDo(ctx, "DELETE", "/quota/"+bucket, nil, nil, nil)
		if isAdminNotFound(err) {
			return nil
		}
		return err
	}
	if r.quotaBytes > 0 {
		left, err := r.quotaLeft(ctx, names, bucket)
		if err != nil {
			return err
		}
		if maxSizeBytes > left {
			return fmt.Errorf("%w: %d bytes left for this bucket", ErrProjectQuotaExceeded, left)
		}
	}
	return r.putBucketQuota(ctx, bucket, maxSizeBytes)
}

// GetBucketSettings: the S3 part as usual; quota from the admin API, no website, no RGW placement fields.
func (r *rustfsBackend) GetBucketSettings(ctx context.Context, bucket string) (*BucketSettings, error) {
	s, err := r.s3Settings(ctx, bucket)
	if err != nil {
		return nil, err
	}
	s.Caps = r.Caps()
	if q, err := r.bucketQuota(ctx, bucket); err == nil && q > 0 {
		s.Quota = BucketQuota{Enabled: true, MaxSize: q, MaxObjects: -1}
	}
	return s, nil
}

// --- access ---

// SetBucketRead flips the anonymous s3:GetObject statement. RustFS decides on the policy, so no ACL is involved.
func (r *rustfsBackend) SetBucketRead(ctx context.Context, bucket, acl string) error {
	if err := r.needS3(); err != nil {
		return err
	}
	return r.setPublicRead(ctx, bucket, acl != "")
}

// --- website: gated off until per-bucket DNS serving is verified on the pinned version ---

func (r *rustfsBackend) GetBucketWebsite(context.Context, string) (*BucketWebsite, error) {
	return nil, ErrWebsiteUnsupported
}

func (r *rustfsBackend) EnableBucketWebsite(context.Context, string, string, string) (*BucketWebsite, error) {
	return nil, ErrWebsiteUnsupported
}

func (r *rustfsBackend) DisableBucketWebsite(context.Context, string) error {
	return ErrWebsiteUnsupported
}
