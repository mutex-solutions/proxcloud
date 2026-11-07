package client

// objectstore_ceph.go = the Ceph RGW (S3) object-store backend (provider:"ceph-s3"). It implements
// objectBackend, so the higher layers (Writer dispatch, BucketProvider sync, the client object-explore
// handlers) call the Client and never see which backend answers.
//
// Two credential planes (see the plan §4.2/§4.4):
//   - admin keys (Admin Ops REST, SigV4-signed): provision the per-project RGW user + quota, and list/stat
//     the project's buckets for the sync/billing meter. An admin-only client (project keys empty) can do
//     these but NOT data I/O.
//   - project keys (aws-sdk-go-v2 S3): bucket create/delete + object I/O + ACL, run AS the project's own
//     RGW user, which OWNS its buckets — ownership (plus S3's default-deny) is the isolation guard, and
//     the admin bucket list is scoped by `uid`.
//
// We deliberately do NOT use RGW multi-tenancy: a tenanted bucket cannot be reached from the s3website
// endpoint (a DNS hostname cannot encode a tenant), which would make static website hosting impossible.
// The cost is a GLOBAL bucket namespace, as on AWS S3 — see ErrBucketNameTaken.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// emptyPayloadHash is the SHA-256 of an empty body — the x-amz-content-sha256 for a bodyless admin call.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// adminValueRe is the allow-list for RGW Admin Ops query VALUES (hex ids, S3 names, numbers, keywords).
// adminDo rejects any value outside it so attacker-influenced input cannot shape the request URL.
var adminValueRe = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)

// adminParamRe is the allow-list for the query PARAMETER NAMES ("bucket", "purge-objects", "accessKey", …). Every name
// we send is a fixed lowercase literal; checking them anyway keeps the whole query — keys and values —
// behind the same barrier.
var adminParamRe = regexp.MustCompile(`^[A-Za-z-]*$`)

// ObjectStoreConfig carries the connection parameters for one ceph-s3 provider scope. Admin keys are always
// present; project keys are empty on an admin-only client (sync/provision).
//
// RGWUID is the per-project anchor: one RGW user per ProxCloud project, in RGW's DEFAULT tenant. We do NOT
// use RGW multi-tenancy — a tenanted bucket is unreachable from the s3website endpoint (a DNS hostname
// cannot encode a tenant), which would kill static website hosting. Isolation instead comes from bucket
// OWNERSHIP: buckets belong to the project's user, S3 default-denies everyone else, and the admin-ops
// bucket list is scoped by `uid`. The trade-off is a GLOBAL bucket namespace, exactly like AWS S3.
type ObjectStoreConfig struct {
	S3Endpoint        string // S3 data endpoint, e.g. https://s3.rgw.example
	S3WebsiteEndpoint string // s3website endpoint (rgw_dns_s3website_name), e.g. https://s3-website.rgw.example
	AdminURL          string // Admin Ops endpoint, e.g. https://s3.rgw.example/admin
	Region            string // RGW zonegroup/region for SigV4 (default "us-east-1")
	AdminAccessKey    string
	AdminSecretKey    string
	ProjectAccessKey  string // the project user's key (empty = admin-only client)
	ProjectSecretKey  string
	RGWUID            string // the per-project RGW user (uidPrefix + proxcloud project id)
	DefaultQuotaBytes int64  // per-user quota to set at provision (0 = leave unset)
}

// cephBackend holds the RGW S3 (project) + Admin Ops (admin) clients for one project scope.
type cephBackend struct {
	s3              *s3.Client // project-key data client; nil on an admin-only (sync/provision) client
	s3Endpoint      string
	websiteEndpoint string
	adminURL        string
	region          string
	adminCreds      awsv2.Credentials
	signer          *v4.Signer
	http            *http.Client
	uid             string // the project's RGW user id (default tenant)
	quotaBytes      int64
	bareUserGrants  bool // bucket-policy principals name users bare (RustFS) rather than by ARN (RGW)
}

// NewCephS3 builds a *client.Client backed by a Ceph RGW object store. When ProjectAccessKey is set, the S3 data client is built; otherwise it is an admin-only client
// (list/stat/provision, no data I/O).
func NewCephS3(_ context.Context, cfg ObjectStoreConfig) (*Client, error) {
	b, err := newCephBackend(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{objectBackend: b}, nil
}

// Caps: RGW offers everything the settings panel shows; websites only where the operator set s3WebsiteEndpoint.
func (b *cephBackend) Caps() ObjectCaps {
	return ObjectCaps{Backend: BackendCephS3, Website: b.websiteEndpoint != "", QuotaObjects: true}
}

func newCephBackend(cfg ObjectStoreConfig) (*cephBackend, error) {
	if cfg.S3Endpoint == "" && cfg.AdminURL == "" {
		return nil, fmt.Errorf("cloud: ceph-s3 requires s3Endpoint or adminApiUrl")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	b := &cephBackend{
		s3Endpoint:      strings.TrimRight(cfg.S3Endpoint, "/"),
		websiteEndpoint: strings.TrimRight(cfg.S3WebsiteEndpoint, "/"),
		adminURL:        strings.TrimRight(cfg.AdminURL, "/"),
		region:          region,
		adminCreds:      awsv2.Credentials{AccessKeyID: cfg.AdminAccessKey, SecretAccessKey: cfg.AdminSecretKey},
		signer:          v4.NewSigner(),
		http:            &http.Client{Timeout: 90 * time.Second},
		uid:             cfg.RGWUID,
		quotaBytes:      cfg.DefaultQuotaBytes,
	}
	if cfg.ProjectAccessKey != "" {
		b.s3 = s3.New(s3.Options{
			Region:       region,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.ProjectAccessKey, cfg.ProjectSecretKey, ""),
			BaseEndpoint: awsv2.String(cfg.S3Endpoint),
			UsePathStyle: true, // RGW is path-style (no per-bucket DNS)
			// RGW may not support aws-chunked trailing checksums on streamed PUTs — only checksum when the
			// operation strictly requires it, so ordinary object uploads stream with a plain Content-Length.
			RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
		})
	}
	return b, nil
}

// ErrBucketNameTaken is returned when a bucket name is already in use. RGW's bucket namespace is GLOBAL
// (we deliberately do not use RGW tenants — see ObjectStoreConfig), so a name can be taken by another project.
var ErrBucketNameTaken = errors.New("cloud: bucket name is already taken")

// ErrBucketNotOwned is returned when an admin-by-name Ceph operation targets a bucket this project's RGW
// user does not own. Because the namespace is global and the admin key can touch any bucket by name, a
// recycled name on a stale cache row could otherwise let one project read, requota, or purge another
// project's bucket — so every admin-by-name path verifies ownership first.
var ErrBucketNotOwned = errors.New("cloud: bucket is not owned by this project")

func (b *cephBackend) needS3() error {
	if b.s3 == nil {
		return fmt.Errorf("ceph-s3: no project S3 credentials on this client (admin-only)")
	}
	return nil
}

// --- Admin Ops (SigV4-signed REST) ---

// adminKey is one S3 key pair on an RGW user.
type adminKey struct {
	User      string `json:"user"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// adminUser is the subset of the RGW admin user info we read (the generated S3 keys).
type adminUser struct {
	UserID string     `json:"user_id"`
	Keys   []adminKey `json:"keys"`
}

// adminBucketStat is the subset of an RGW admin bucket-stats entry we read: name, OWNER (the uid the
// bucket belongs to — the ownership guard for every admin-by-name call), per-category usage, the per-bucket
// quota, and the two create-time placement fields the UI shows read-only.
type adminBucketStat struct {
	Bucket string `json:"bucket"`
	Tenant string `json:"tenant"`
	Owner  string `json:"owner"`
	Usage  map[string]struct {
		Size       int64 `json:"size"`
		SizeActual int64 `json:"size_actual"`
		NumObjects int64 `json:"num_objects"`
	} `json:"usage"`
	IndexType     string `json:"index_type"`
	PlacementRule string `json:"placement_rule"`
	BucketQuota   struct {
		Enabled    bool  `json:"enabled"`
		MaxSize    int64 `json:"max_size"`
		MaxObjects int64 `json:"max_objects"`
	} `json:"bucket_quota"`
}

// urlValues builds url.Values from alternating key/value pairs (an admin-ops call is mostly query params).
func urlValues(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

// adminBucketInfo reads one bucket's full admin record (owner + quota + placement + usage) and REFUSES a
// bucket this project's user does not own. The admin key can address ANY bucket by name (the RGW namespace
// is global), so a stale cache row whose name was recreated by another project must never expose or mutate
// the foreign bucket. Every admin-by-name path funnels through here (getBucket, SetBucketQuota,
// ForceDeleteBucket), so ownership is verified in exactly one place.
func (b *cephBackend) adminBucketInfo(ctx context.Context, bucket string) (adminBucketStat, error) {
	var st adminBucketStat
	if err := b.adminDo(ctx, "GET", "/bucket", urlValues("bucket", bucket, "stats", "true"), nil, &st); err != nil {
		return st, err
	}
	if st.Owner != b.uid {
		return st, fmt.Errorf("%w: bucket %q is owned by %q, not %q", ErrBucketNotOwned, bucket, st.Owner, b.uid)
	}
	return st, nil
}

// adminDeleteBucket removes a bucket via Admin Ops, optionally purging its objects first. S3's DeleteBucket
// refuses a non-empty bucket; this is the teardown path that can actually clear one.
func (b *cephBackend) adminDeleteBucket(ctx context.Context, bucket string, purgeObjects bool) error {
	q := urlValues("bucket", bucket)
	if purgeObjects {
		q.Set("purge-objects", "true")
	}
	err := b.adminDo(ctx, "DELETE", "/bucket", q, nil, nil)
	if isAdminNotFound(err) {
		return nil
	}
	return err
}

// totals sums logical size + object count across all usage categories (rgw.main + shadow/multimeta). Uses
// logical `size` (what the customer stored) as the billed usage; falls back to size_actual.
func (s adminBucketStat) totals() (bytes, objects int64) {
	for _, u := range s.Usage {
		sz := u.Size
		if sz == 0 {
			sz = u.SizeActual
		}
		bytes += sz
		objects += u.NumObjects
	}
	return
}

// bucketData is the DataBucket map for this stat; quotaBytes is present only when a size quota is on.
func (s adminBucketStat) bucketData() map[string]any {
	bytes, objects := s.totals()
	d := bucketData(BackendCephS3, s.Bucket, objects, bytes)
	if s.BucketQuota.Enabled && s.BucketQuota.MaxSize > 0 {
		d["quotaBytes"] = s.BucketQuota.MaxSize
	}
	return d
}

// adminError is a non-2xx Admin Ops response. It carries the status so callers can distinguish a genuine
// "absent" (404) from an auth/permission failure instead of silently treating both as "not there".
type adminError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *adminError) Error() string {
	return fmt.Sprintf("object store admin %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// isAdminNotFound reports whether err is an Admin Ops 404 (the resource genuinely does not exist).
func isAdminNotFound(err error) bool {
	var ae *adminError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// adminDo signs (SigV4, service "s3") and executes one Admin Ops request, decoding a JSON response into
// out (nil to ignore). A non-2xx status returns an *adminError carrying the RGW body.
func (b *cephBackend) adminDo(ctx context.Context, method, path string, q url.Values, body []byte, out any) error {
	if b.adminURL == "" {
		return fmt.Errorf("ceph-s3: admin API URL not configured")
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("format", "json")
	// Rebuild the query from an ALLOW-LIST so the exact value strings that reach the request URL have each
	// passed the barrier. Every Admin Ops query value we send is a hex id, an S3 name, a number, or a fixed
	// keyword; the scheme+host+path come from operator config + constant literals, so with the values
	// constrained the request destination is fully determined by configuration and attacker-influenced
	// input (bucket / key names, project-derived uids) cannot shape it (CWE-918 request-forgery). Building a
	// fresh url.Values keeps the barrier on the same data-flow node that reaches the sink.
	safe := url.Values{}
	for k, vs := range q {
		if !adminParamRe.MatchString(k) {
			return fmt.Errorf("ceph-s3: refusing admin request: unsafe query parameter %q", k)
		}
		for _, v := range vs {
			if !adminValueRe.MatchString(v) {
				return fmt.Errorf("ceph-s3: refusing admin request: unsafe %s value %q", k, v)
			}
			safe.Add(k, v)
		}
	}
	// The request URL handed to NewRequest is built from operator config + the constant path ONLY; the
	// checked values go in via RawQuery afterwards. A query string cannot change the scheme/host/path of
	// the request, so attacker-influenced input can never redirect it (CWE-918) — and the taint never
	// reaches the URL argument.
	req, err := http.NewRequestWithContext(ctx, method, b.adminURL+path, bodyReader(body))
	if err != nil {
		return err
	}
	// url.Values.Encode() sorts keys — SigV4's canonical query string requires sorted params, and RGW
	// canonicalizes the received query the same way. (Signed values must also be space-free: Encode()
	// emits "+" for a space where SigV4 canonicalization expects %20.) Set BEFORE SignHTTP so the
	// signature covers the query.
	req.URL.RawQuery = safe.Encode()
	payloadHash := emptyPayloadHash
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}
	// x-amz-content-sha256 MUST be sent for SigV4 against S3/RGW: the server recomputes the canonical
	// request from it. v4.Signer.SignHTTP signs with the payload hash but does NOT add the header (the
	// S3 client's middleware normally does), so set it here or every call is SignatureDoesNotMatch.
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := b.signer.SignHTTP(ctx, b.adminCreds, req, payloadHash, "s3", b.region, time.Now().UTC()); err != nil {
		return err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &adminError{Status: resp.StatusCode, Method: method, Path: path, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func bodyReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

// ensureUser is the idempotent RGW tenant-user provision: GET the user (return its keys if it exists),
// else create it (uid + tenant + display-name) and return the generated keys. displayName MUST be
// space-free — it goes in the SigV4-signed query and url.Encode's `+` for space breaks the signature.
func (b *cephBackend) ensureUser(ctx context.Context, displayName string) (access, secret string, err error) {
	var got adminUser
	switch e := b.adminDo(ctx, "GET", "/user", url.Values{"uid": {b.uid}}, nil, &got); {
	case e == nil && len(got.Keys) > 0:
		return got.Keys[0].AccessKey, got.Keys[0].SecretKey, nil
	case e != nil && !isAdminNotFound(e):
		// An auth/permission failure must NOT be mistaken for "user absent" — that would blindly attempt
		// a create and report the wrong cause.
		return "", "", fmt.Errorf("ceph-s3: lookup user %s: %w", b.uid, e)
	}
	var created adminUser
	q := url.Values{"uid": {b.uid}, "display-name": {displayName}}
	if err = b.adminDo(ctx, "PUT", "/user", q, nil, &created); err != nil {
		return "", "", err
	}
	if len(created.Keys) == 0 {
		return "", "", fmt.Errorf("ceph-s3: user %s created with no keys", b.uid)
	}
	return created.Keys[0].AccessKey, created.Keys[0].SecretKey, nil
}

// setUserQuota caps the tenant-user's total storage (max-size bytes, -1 = unlimited). No-op when bytes<=0.
func (b *cephBackend) setUserQuota(ctx context.Context, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	q := url.Values{
		"quota": {""}, "uid": {b.uid}, "quota-type": {"user"},
		"max-size": {fmt.Sprint(maxBytes)}, "enabled": {"true"},
	}
	return b.adminDo(ctx, "PUT", "/user", q, nil, nil)
}

// purgeUser removes the tenant-user and all its data (deprovision).
func (b *cephBackend) purgeUser(ctx context.Context) error {
	return b.adminDo(ctx, "DELETE", "/user", url.Values{"uid": {b.uid}, "purge-data": {"true"}}, nil, nil)
}

// --- bucket surface (behind objectstore.go's Client methods) ---

// createBucket creates the bucket as the project user, which OWNS it (the isolation boundary). The RGW
// bucket namespace is global, so a name already used by ANY project — or by this one — surfaces as
// ErrBucketNameTaken rather than a raw S3 error. Returns DataBucket 0/0.
func (b *cephBackend) CreateBucket(ctx context.Context, o CreateBucketOpts) (map[string]any, error) {
	if err := b.needS3(); err != nil {
		return nil, err
	}
	in := &s3.CreateBucketInput{Bucket: &o.Name}
	if o.ObjectLockEnabled {
		// Object lock can ONLY be turned on at creation (S3 rule) and implies versioning.
		in.ObjectLockEnabledForBucket = awsv2.Bool(true)
	}
	if _, err := b.s3.CreateBucket(ctx, in); err != nil {
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return nil, fmt.Errorf("%w: %s", ErrBucketNameTaken, o.Name)
		}
		return nil, err
	}
	return bucketData(BackendCephS3, o.Name, 0, 0), nil
}

// getBucket reads one bucket's stats via Admin Ops (admin keys) → DataBucket. It goes through
// adminBucketInfo, which REFUSES a bucket this project does not own — otherwise a stale cache row whose
// name was recreated by another project would leak that project's object count / size (finding F4).
func (b *cephBackend) GetBucket(ctx context.Context, name string) (map[string]any, error) {
	st, err := b.adminBucketInfo(ctx, name)
	if err != nil {
		return nil, err
	}
	return st.bucketData(), nil
}

// listBuckets lists the project user's buckets WITH stats (Admin Ops, admin keys) — the sync/billing meter.
func (b *cephBackend) ListBuckets(ctx context.Context) ([]map[string]any, error) {
	var stats []adminBucketStat
	if err := b.adminDo(ctx, "GET", "/bucket", url.Values{"uid": {b.uid}, "stats": {"true"}}, nil, &stats); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(stats))
	for _, st := range stats {
		if st.Bucket == "" {
			continue
		}
		out = append(out, st.bucketData())
	}
	return out, nil
}

// deleteBucket removes the (empty) bucket as the project user. RGW rejects a non-empty delete.
func (b *cephBackend) DeleteBucket(ctx context.Context, name string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	_, err := b.s3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &name})
	if notFound(err, "BucketNotEmpty") {
		return ErrBucketNotEmpty
	}
	return err
}

// listBucketObjects lists a bucket's objects/folders in the FE display shape (S3 ListObjectsV2 with a "/"
// delimiter → CommonPrefixes become directory rows).
func (b *cephBackend) ListBucketObjects(ctx context.Context, bucket, prefix string) ([]map[string]any, error) {
	if err := b.needS3(); err != nil {
		return nil, err
	}
	in := &s3.ListObjectsV2Input{Bucket: &bucket, Delimiter: awsv2.String("/")}
	if prefix != "" {
		p := strings.TrimSuffix(prefix, "/") + "/"
		in.Prefix = &p
	}
	out := []map[string]any{}
	for {
		page, err := b.s3.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, cp := range page.CommonPrefixes {
			sub := awsv2.ToString(cp.Prefix) // e.g. "a/b/"
			name0 := strings.TrimSuffix(sub, "/")
			display := name0
			if prefix != "" {
				display = strings.TrimPrefix(name0, strings.TrimSuffix(prefix, "/")+"/")
			}
			out = append(out, map[string]any{
				"name": sub, "bucketName": bucket, "displayName": display,
				"directoryName": prefix, "directory": true, "sizeInBytes": 0, "mimeType": "", "lastModified": "",
			})
		}
		for i := range page.Contents {
			key := awsv2.ToString(page.Contents[i].Key)
			if key == "" || strings.HasSuffix(key, "/") { // skip folder markers
				continue
			}
			display := key
			if prefix != "" {
				display = strings.TrimPrefix(key, strings.TrimSuffix(prefix, "/")+"/")
			}
			lastMod := ""
			if page.Contents[i].LastModified != nil {
				lastMod = page.Contents[i].LastModified.UTC().Format("2006-01-02T15:04:05Z")
			}
			out = append(out, map[string]any{
				"name": key, "bucketName": bucket, "displayName": display, "directoryName": prefix,
				"directory": false, "sizeInBytes": awsv2.ToInt64(page.Contents[i].Size), "mimeType": "",
				"lastModified": lastMod,
			})
		}
		if !awsv2.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			break
		}
		in.ContinuationToken = page.NextContinuationToken
	}
	return out, nil
}

// createFolder writes a 0-byte pseudo-folder marker "<folderName>/".
func (b *cephBackend) CreateFolder(ctx context.Context, bucket, folderName string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	key := strings.TrimSuffix(folderName, "/") + "/"
	ct := "application/directory"
	_, err := b.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: &key, ContentType: &ct, Body: strings.NewReader(""),
	})
	return err
}

// uploadBucketObject streams an object into the bucket (S3 PutObject with an explicit Content-Length).
func (b *cephBackend) UploadBucketObject(ctx context.Context, bucket, objectName, contentType string, contentLength int64, payload io.Reader) error {
	if err := b.needS3(); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := b.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: &objectName, Body: payload,
		ContentType: &contentType, ContentLength: &contentLength,
	})
	return err
}

// deleteBucketObject deletes an object — or a folder + everything under it — by prefix.
func (b *cephBackend) DeleteBucketObject(ctx context.Context, bucket, objectName string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	in := &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &objectName}
	for {
		page, err := b.s3.ListObjectsV2(ctx, in)
		if err != nil {
			return err
		}
		for i := range page.Contents {
			key := awsv2.ToString(page.Contents[i].Key)
			if _, err := b.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}); err != nil {
				return err
			}
		}
		if !awsv2.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			break
		}
		in.ContinuationToken = page.NextContinuationToken
	}
	return nil
}

// updateBucketObject replaces an object's custom metadata (S3 has no in-place metadata edit → copy-self
// with MetadataDirective=REPLACE).
func (b *cephBackend) UpdateBucketObject(ctx context.Context, bucket, objectName string, metadata map[string]string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	src := bucket + "/" + objectName
	_, err := b.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: &bucket, Key: &objectName, CopySource: &src,
		Metadata: metadata, MetadataDirective: s3types.MetadataDirectiveReplace,
	})
	return err
}

// downloadObject reads an object's bytes + content-type (whole object into memory).
func (b *cephBackend) DownloadObject(ctx context.Context, bucket, objectName string) ([]byte, string, error) {
	if err := b.needS3(); err != nil {
		return nil, "", err
	}
	out, err := b.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &objectName})
	if err != nil {
		return nil, "", err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	ct := awsv2.ToString(out.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	return data, ct, nil
}

// isBucketPublic reports whether the bucket's OBJECTS are anonymously readable. On S3 that is decided by
// the bucket POLICY, not the ACL: a public-read ACL only allows listing the bucket. Checking the ACL alone
// would report "public" for a bucket whose objects all 403.
func (b *cephBackend) IsBucketPublic(ctx context.Context, bucket string) (bool, error) {
	if err := b.needS3(); err != nil {
		return false, err
	}
	return b.hasAnonymousReadStmt(ctx, bucket)
}

// setBucketRead flips public read on the bucket: the canned ACL (bucket listing) AND the anonymous
// s3:GetObject policy statement (object reads). Empty acl = private, which removes both.
func (b *cephBackend) SetBucketRead(ctx context.Context, bucket, acl string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	canned := s3types.BucketCannedACLPrivate
	if acl != "" {
		canned = s3types.BucketCannedACLPublicRead
	}
	if _, err := b.s3.PutBucketAcl(ctx, &s3.PutBucketAclInput{Bucket: &bucket, ACL: canned}); err != nil {
		return err
	}
	return b.setPublicRead(ctx, bucket, acl != "")
}

// bucketAPIs returns the bucket's S3 access URL (path-style).
func (b *cephBackend) bucketURLs(bucket string) []string {
	if b.s3Endpoint == "" {
		return []string{}
	}
	return []string{b.s3Endpoint + "/" + bucket}
}

// --- static website hosting (RGW s3website API) ---

// bucketARN is the S3 ARN of a bucket's objects (default tenant → classic empty-account form).
func (b *cephBackend) bucketARN(bucket string) string {
	return "arn:aws:s3:::" + bucket + "/*"
}

// websiteURL is the bucket's virtual-hosted static-website URL (<bucket>.<websiteHost>). Empty when the
// provider has no s3WebsiteEndpoint configured.
func (b *cephBackend) websiteURL(bucket string) string {
	if b.websiteEndpoint == "" {
		return ""
	}
	u, err := url.Parse(b.websiteEndpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + bucket + "." + u.Host
}

// enableWebsite turns the bucket into a static website: an index/error document config, PLUS a bucket
// policy making every object publicly readable. Enabling therefore EXPOSES THE BUCKET'S OBJECTS TO
// ANONYMOUS READERS — callers must surface that to the user rather than flipping it silently.
func (b *cephBackend) enableWebsite(ctx context.Context, bucket, indexDoc, errorDoc string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	if b.websiteEndpoint == "" {
		return ErrWebsiteUnsupported
	}
	if indexDoc == "" {
		indexDoc = "index.html"
	}
	// PUBLISH LAST. The website config alone exposes nothing, but the public-read policy does — so write
	// the website config first and only then open the bucket. If the policy write fails we roll the website
	// config back; the bucket is never left publicly readable by a half-completed enable.
	cfg := &s3types.WebsiteConfiguration{IndexDocument: &s3types.IndexDocument{Suffix: &indexDoc}}
	if errorDoc != "" {
		cfg.ErrorDocument = &s3types.ErrorDocument{Key: &errorDoc}
	}
	if _, err := b.s3.PutBucketWebsite(ctx, &s3.PutBucketWebsiteInput{Bucket: &bucket, WebsiteConfiguration: cfg}); err != nil {
		return websiteErr(err)
	}
	// Merge the public-read statement INTO the existing policy (by Sid) — a blind replace would delete the
	// customer's own statements and every per-key grant on this bucket.
	doc, err := b.getPolicyDoc(ctx, bucket)
	if err == nil {
		doc.upsertStmt(b.websiteStmt(bucket))
		err = b.putPolicyDoc(ctx, bucket, doc)
	}
	if err != nil {
		// Best-effort rollback: an "enabled" website that serves 403 is confusing, but a silently public
		// bucket is dangerous. Neither is left behind.
		if _, derr := b.s3.DeleteBucketWebsite(ctx, &s3.DeleteBucketWebsiteInput{Bucket: &bucket}); derr != nil {
			return fmt.Errorf("ceph-s3: public-read policy failed (%w) and rolling back the website config also failed: %v", err, derr)
		}
		return fmt.Errorf("ceph-s3: public-read policy: %w", err)
	}
	return nil
}

// websiteErr names a gateway without rgw_enable_static_website: it answers 405 to every website call.
func websiteErr(err error) error {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusMethodNotAllowed {
		return ErrWebsiteUnsupported
	}
	return err
}

// disableWebsite removes the public-read statement enableWebsite installed AND the website config, so
// turning the website off closes anonymous access again — while LEAVING every other statement (customer
// policy, per-key grants) untouched. CLOSE FIRST, the mirror of enableWebsite's publish last: the statement
// is what exposes the objects, and it serves on the S3 endpoint whether or not the gateway still answers
// website calls, so nothing about the config may stand in the way of removing it.
func (b *cephBackend) disableWebsite(ctx context.Context, bucket string) error {
	if err := b.needS3(); err != nil {
		return err
	}
	doc, err := b.getPolicyDoc(ctx, bucket)
	if err != nil {
		return err
	}
	if doc.removeStmt(sidWebsiteRead) { // no statement of ours: never touch a policy we did not write
		if err := b.putPolicyDoc(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return b.dropWebsiteConfig(ctx, bucket)
}

// dropWebsiteConfig deletes the bucket's website config. No config, or a gateway without the website API
// (405: the config is not in effect there), is already the wanted end state.
func (b *cephBackend) dropWebsiteConfig(ctx context.Context, bucket string) error {
	_, err := b.s3.DeleteBucketWebsite(ctx, &s3.DeleteBucketWebsiteInput{Bucket: &bucket})
	if err == nil || strings.Contains(err.Error(), "NoSuchWebsiteConfiguration") || errors.Is(websiteErr(err), ErrWebsiteUnsupported) {
		return nil
	}
	return err
}

// getWebsite reports the bucket's website config. A bucket with no config is NOT an error (enabled=false).
func (b *cephBackend) getWebsite(ctx context.Context, bucket string) (enabled bool, indexDoc, errorDoc string, err error) {
	if err := b.needS3(); err != nil {
		return false, "", "", err
	}
	out, err := b.s3.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{Bucket: &bucket})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchWebsiteConfiguration") {
			return false, "", "", nil
		}
		return false, "", "", err
	}
	if out.IndexDocument != nil {
		indexDoc = awsv2.ToString(out.IndexDocument.Suffix)
	}
	if out.ErrorDocument != nil {
		errorDoc = awsv2.ToString(out.ErrorDocument.Key)
	}
	return true, indexDoc, errorDoc, nil
}

// BucketAPIs returns the bucket's S3 access URLs.
func (b *cephBackend) BucketAPIs(_ context.Context, bucket string) ([]string, error) {
	return b.bucketURLs(bucket), nil
}

// UpdateBucketMetadata would update a bucket's custom metadata. S3 has no per-bucket custom
// metadata, so this is a no-op success.
// ponytail: map to S3 bucket tagging if the FE ever needs to read it back.
func (b *cephBackend) UpdateBucketMetadata(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

// GetBucketWebsite reports the bucket's static-website state. The statement, not the config, is what
// publishes the objects, and it outlives the config on a gateway that lost the website API (405) or after a
// client deleted the config directly. Either counts as enabled, so the panel still offers the way to close it.
func (b *cephBackend) GetBucketWebsite(ctx context.Context, bucket string) (*BucketWebsite, error) {
	enabled, index, errDoc, err := b.getWebsite(ctx, bucket)
	if err != nil && !errors.Is(websiteErr(err), ErrWebsiteUnsupported) {
		return nil, err
	}
	doc, err := b.getPolicyDoc(ctx, bucket)
	if err != nil {
		return nil, err
	}
	public := slices.ContainsFunc(doc.Statement, func(s json.RawMessage) bool { return sidOf(s) == sidWebsiteRead })
	w := &BucketWebsite{Enabled: enabled || public, IndexDocument: index, ErrorDocument: errDoc, PublicObjects: public}
	if enabled {
		w.URL = b.websiteURL(bucket)
	}
	return w, nil
}

// EnableBucketWebsite serves the bucket as a static website. ⚠ This also installs a bucket policy that
// makes EVERY object in the bucket anonymously readable — the returned BucketWebsite says so
// (PublicObjects), and callers must show that to the user.
func (b *cephBackend) EnableBucketWebsite(ctx context.Context, bucket, indexDoc, errorDoc string) (*BucketWebsite, error) {
	if err := b.enableWebsite(ctx, bucket, indexDoc, errorDoc); err != nil {
		return nil, err
	}
	return b.GetBucketWebsite(ctx, bucket)
}

// DisableBucketWebsite stops serving the bucket as a website and removes the public-read policy.
func (b *cephBackend) DisableBucketWebsite(ctx context.Context, bucket string) error {
	return b.disableWebsite(ctx, bucket)
}

// --- registration check ---

// adminCap is one entry of an RGW user's caps.
type adminCap struct {
	Type string `json:"type"`
	Perm string `json:"perm"`
}

// CheckCephS3 proves a ceph-s3 connection before it is stored: a stored row that cannot reach RGW fails the
// bootstrap of every new project. It reads the admin key's own user, so an endpoint that is not RGW or a key
// that does not exist fails instead of reading as "user absent".
func CheckCephS3(ctx context.Context, cfg ObjectStoreConfig) error {
	for _, u := range []string{cfg.S3Endpoint, cfg.AdminURL} {
		if p, err := url.Parse(u); err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https") {
			return fmt.Errorf("%q is not an http(s) URL", u)
		}
	}
	if cfg.AdminAccessKey == "" || cfg.AdminSecretKey == "" {
		return errors.New("the admin access key and secret key are required")
	}
	cfg.ProjectAccessKey, cfg.ProjectSecretKey = cfg.AdminAccessKey, cfg.AdminSecretKey // the S3 leg signs with the same key
	b, err := newCephBackend(cfg)
	if err != nil {
		return err
	}
	if _, err := b.s3.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		return fmt.Errorf("S3 endpoint %s: %w", cfg.S3Endpoint, err)
	}
	var self struct {
		Caps []adminCap `json:"caps"`
	}
	if err := b.adminDo(ctx, "GET", "/user", url.Values{"access-key": {cfg.AdminAccessKey}}, nil, &self); err != nil {
		return fmt.Errorf("Admin Ops %s: %w", cfg.AdminURL, err)
	}
	for _, want := range []string{"users", "buckets", "usage"} {
		if !slices.Contains(self.Caps, adminCap{Type: want, Perm: "*"}) {
			return fmt.Errorf("the admin key lacks the RGW cap %s=* (it needs users=*;buckets=*;usage=*)", want)
		}
	}
	return nil
}

// --- provisioning (used by the bootstrap) ---

// EnsureProjectUser idempotently provisions the project's RGW tenant-user and returns its S3 keys, then sets
// the configured storage quota. displayName MUST be space-free (SigV4-signed query).
func (b *cephBackend) EnsureProjectUser(ctx context.Context, displayName string) (access, secret string, err error) {
	access, secret, err = b.ensureUser(ctx, displayName)
	if err != nil {
		return "", "", err
	}
	// Quota is a soft limit — a failure to set it must NOT fail provisioning (the user + keys are what
	// make the project usable). Best-effort. ponytail: surface a warning if quota enforcement matters.
	_ = b.setUserQuota(ctx, b.quotaBytes)
	return access, secret, nil
}

// PurgeProjectUser removes the project's RGW user and its data (deprovision).
func (b *cephBackend) PurgeProjectUser(ctx context.Context) error {
	return b.purgeUser(ctx)
}

// ForceDeleteBucket removes a bucket AND its objects via Admin Ops. TEARDOWN path only — the
// customer-facing DeleteBucket keeps S3 semantics and refuses a non-empty bucket.
//
// It first verifies the live bucket is still OWNED by this project's user. Because the namespace is global
// and teardown deletes by cached name, a stale row whose name was recreated by another project must NOT
// purge the foreign bucket (finding F3): if the bucket is gone, or now owned by someone else, this is a
// no-op success so the caller archives the stale cache row without destroying another tenant's data. The
// final PurgeProjectUser(purge-data) still removes every bucket this project genuinely owns.
func (b *cephBackend) ForceDeleteBucket(ctx context.Context, bucket string) error {
	owner, err := b.bucketOwner(ctx, bucket)
	if isAdminNotFound(err) {
		return nil // already gone — nothing to delete, archive the stale row
	}
	if err != nil {
		return err
	}
	if owner != b.uid {
		return nil // recycled name now owned by another project — must not delete it
	}
	return b.adminDeleteBucket(ctx, bucket, true)
}

// bucketOwner returns a bucket's live owner uid (Admin Ops). isAdminNotFound(err) → the bucket is absent.
func (b *cephBackend) bucketOwner(ctx context.Context, bucket string) (string, error) {
	var st adminBucketStat
	if err := b.adminDo(ctx, "GET", "/bucket", urlValues("bucket", bucket), nil, &st); err != nil {
		return "", err
	}
	return st.Owner, nil
}

// ChildUID is the RGW uid of an extra S3 key belonging to this project: "<projectUid>-<name>". Deriving it
// in ONE place is what makes the ownership guard below meaningful.
func (b *cephBackend) ChildUID(name string) string { return b.uid + "-" + name }

// assertOwnedUID refuses to touch any RGW user that is not this project's own child key. Without it a bad
// id from a request body could create — or PURGE — another project's user.
func (b *cephBackend) assertOwnedUID(uid string) error {
	if uid == "" || !strings.HasPrefix(uid, b.uid+"-") {
		return fmt.Errorf("ceph-s3: refusing to operate on RGW user %q: not owned by project user %q", uid, b.uid)
	}
	return nil
}

// CreateChildKey provisions an extra RGW user (an additional S3 access key for this project) and
// returns its keys. The uid MUST be under this project's prefix.
//
// max-buckets=-1 FORBIDS this user from creating its own buckets (live-measured: -1 → 403 AccessDenied on
// CreateBucket, whereas 0 lets it create freely — the opposite of the usual "-1 = unlimited" convention).
// This matters: a child user's own buckets would sit outside the parent's per-bucket grants AND outside
// syncCephService (which lists only the parent uid), so they would never be metered or billed. An extra
// key gets access ONLY through explicit bucket grants.
func (b *cephBackend) CreateChildKey(ctx context.Context, uid, displayName string) (access, secret string, err error) {
	if err := b.assertOwnedUID(uid); err != nil {
		return "", "", err
	}
	var created adminUser
	q := urlValues("uid", uid, "display-name", displayName, "max-buckets", "-1")
	if err := b.adminDo(ctx, "PUT", "/user", q, nil, &created); err != nil {
		return "", "", err
	}
	if len(created.Keys) == 0 {
		return "", "", fmt.Errorf("ceph-s3: user %s created with no keys", uid)
	}
	return created.Keys[0].AccessKey, created.Keys[0].SecretKey, nil
}

// DeleteChildKey removes one of this project's extra S3 keys (and any data it owns).
func (b *cephBackend) DeleteChildKey(ctx context.Context, uid string) error {
	if err := b.assertOwnedUID(uid); err != nil {
		return err
	}
	err := b.adminDo(ctx, "DELETE", "/user", urlValues("uid", uid, "purge-data", "true"), nil, nil)
	if isAdminNotFound(err) {
		return nil
	}
	return err
}

// RotateKey issues a FRESH access/secret pair for an RGW user and removes the old one, returning
// the new pair. Admin Ops `PUT /admin/user?key` (generate-key) then `DELETE /admin/user?key&access-key=…`.
//
// The old key stops working the moment it is removed — that IS the point of a rotation — so callers must
// persist the new pair before anything else can read it. Order matters: create first, then delete, so a
// failure mid-way leaves the user with a WORKING key rather than none.
//
// uid must be this project's own user or one of its child keys.
func (b *cephBackend) RotateKey(ctx context.Context, uid, oldAccessKey string) (access, secret string, err error) {
	if uid != b.uid {
		if err := b.assertOwnedUID(uid); err != nil {
			return "", "", err
		}
	}
	// NOTE: unlike PUT /user (which returns the user object), PUT /user?key returns a BARE ARRAY of the
	// user's s3 keys. Live-verified — decoding it as an object fails.
	var keys []adminKey
	q := urlValues("key", "", "uid", uid, "key-type", "s3", "generate-key", "true")
	if err := b.adminDo(ctx, "PUT", "/user", q, nil, &keys); err != nil {
		return "", "", fmt.Errorf("ceph-s3: generate key for %s: %w", uid, err)
	}
	// The response lists ALL of the user's s3 keys; the new one is whichever is not the old one.
	for _, k := range keys {
		if k.AccessKey != "" && k.AccessKey != oldAccessKey {
			access, secret = k.AccessKey, k.SecretKey
		}
	}
	if access == "" {
		return "", "", fmt.Errorf("ceph-s3: rotation for %s produced no new key", uid)
	}
	if oldAccessKey != "" {
		if err := b.adminDo(ctx, "DELETE", "/user",
			urlValues("key", "", "uid", uid, "key-type", "s3", "access-key", oldAccessKey), nil, nil); err != nil && !isAdminNotFound(err) {
			// The new key already works; surface the failure so the operator can retire the old one.
			return access, secret, fmt.Errorf("ceph-s3: new key issued but the OLD key %s could not be removed: %w", oldAccessKey, err)
		}
	}
	return access, secret, nil
}

// Endpoints exposes the provider's S3 + website endpoints so the customer can point aws-cli at them.
func (b *cephBackend) Endpoints() (s3Endpoint, websiteEndpoint, region string) {
	return b.s3Endpoint, b.websiteEndpoint, b.region
}
