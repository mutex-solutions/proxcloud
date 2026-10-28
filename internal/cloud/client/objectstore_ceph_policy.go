package client

// objectstore_ceph_policy.go — ProxCloud owns the bucket policy DOCUMENT, not individual statements.
//
// A bucket has exactly ONE policy JSON. Multiple features write into it (static website public-read, and
// per-key grants), and customers may add statements of their own. So every write is a read-modify-write
// keyed on `Sid`: ProxCloud statements are upserted/removed by their Sid and every foreign statement is
// carried through VERBATIM (json.RawMessage). A blind PutBucketPolicy would silently wipe the others —
// which is exactly what the first cut of the website toggle did.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Sids ProxCloud manages. Anything else in the document belongs to the customer and is never touched.
const (
	sidPrefix      = "ProxCloud"
	sidWebsiteRead = sidPrefix + "PublicWebsiteRead"
	// sidPublicRead backs the explicit "make bucket public" toggle. It is SEPARATE from the website Sid so
	// that turning a website off does not silently un-publish a bucket the user published on purpose.
	sidPublicRead  = sidPrefix + "PublicRead"
	sidGrantPrefix = sidPrefix + "Grant-"
)

// grantSid is the Sid of the statement granting one RGW user access to this bucket.
func grantSid(uid string) string { return sidGrantPrefix + uid }

// isPlatformSid reports whether a statement is one the platform generates (and may therefore rewrite).
func isPlatformSid(sid string) bool {
	return sid == sidWebsiteRead || sid == sidPublicRead || strings.HasPrefix(sid, sidGrantPrefix)
}

// policyStmt is the shape ProxCloud GENERATES. It is never used to re-encode a customer's statement: a real
// S3 statement may carry fields this struct does not model (NotPrincipal, NotAction, NotResource, extra
// Condition shapes, …) and round-tripping through it would silently DROP them, rewriting the customer's
// access control. Foreign statements are therefore kept as raw JSON.
type policyStmt struct {
	Sid       string          `json:"Sid,omitempty"`
	Effect    string          `json:"Effect"`
	Principal json.RawMessage `json:"Principal,omitempty"`
	Action    json.RawMessage `json:"Action,omitempty"`
	Resource  json.RawMessage `json:"Resource,omitempty"`
	Condition json.RawMessage `json:"Condition,omitempty"`
}

// policyDoc holds statements VERBATIM. Only the ones ProxCloud owns (by Sid) are ever regenerated.
// Id is modelled so a customer policy that carries one survives ProxCloud's read-modify-write cycles.
type policyDoc struct {
	Version   string            `json:"Version"`
	ID        string            `json:"Id,omitempty"`
	Statement []json.RawMessage `json:"Statement"`
}

// sidOf reads a raw statement's Sid ("" when absent/unparseable).
func sidOf(raw json.RawMessage) string {
	var s struct {
		Sid string `json:"Sid"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.Sid
}

// actionOf reads a raw statement's Action (nil when absent).
func actionOf(raw json.RawMessage) json.RawMessage {
	var s struct {
		Action json.RawMessage `json:"Action"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.Action
}

// mustRaw encodes a ProxCloud-generated statement.
func mustRaw(stmt policyStmt) json.RawMessage {
	b, _ := json.Marshal(stmt)
	return b
}

// getPolicyDoc reads the bucket's policy. A bucket with no policy is NOT an error — it yields an empty doc.
func (b *cephBackend) getPolicyDoc(ctx context.Context, bucket string) (*policyDoc, error) {
	if err := b.needS3(); err != nil {
		return nil, err
	}
	out, err := b.s3.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: &bucket})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchBucketPolicy") {
			return &policyDoc{Version: "2012-10-17"}, nil
		}
		return nil, err
	}
	doc := &policyDoc{}
	if out.Policy != nil && *out.Policy != "" {
		if err := json.Unmarshal([]byte(*out.Policy), doc); err != nil {
			return nil, fmt.Errorf("ceph-s3: bucket %s has an unparseable policy: %w", bucket, err)
		}
	}
	if doc.Version == "" {
		doc.Version = "2012-10-17"
	}
	return doc, nil
}

// putPolicyDoc writes the document back, DELETING the policy entirely when no statements remain (an empty
// Statement array is rejected by S3).
func (b *cephBackend) putPolicyDoc(ctx context.Context, bucket string, doc *policyDoc) error {
	if err := b.needS3(); err != nil {
		return err
	}
	if len(doc.Statement) == 0 {
		_, err := b.s3.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: &bucket})
		if err != nil && strings.Contains(err.Error(), "NoSuchBucketPolicy") {
			return nil
		}
		return err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	s := string(raw)
	_, err = b.s3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: &bucket, Policy: &s})
	return err
}

// upsertStmt replaces the statement with the same Sid, else appends. Order is otherwise preserved.
// Only ProxCloud-generated statements are ever written this way; foreign ones are never re-encoded.
func (d *policyDoc) upsertStmt(stmt policyStmt) {
	raw := mustRaw(stmt)
	for i := range d.Statement {
		if sid := sidOf(d.Statement[i]); sid != "" && sid == stmt.Sid {
			d.Statement[i] = raw
			return
		}
	}
	d.Statement = append(d.Statement, raw)
}

// removeStmt drops the statement with the given Sid; reports whether anything was removed.
func (d *policyDoc) removeStmt(sid string) bool {
	for i := range d.Statement {
		if sidOf(d.Statement[i]) == sid {
			d.Statement = append(d.Statement[:i], d.Statement[i+1:]...)
			return true
		}
	}
	return false
}

// ownStmts returns the ProxCloud-managed statements verbatim (re-applied over a customer-supplied doc).
func (d *policyDoc) ownStmts() []json.RawMessage {
	out := make([]json.RawMessage, 0, len(d.Statement))
	for _, s := range d.Statement {
		if isPlatformSid(sidOf(s)) {
			out = append(out, s)
		}
	}
	return out
}

// rawJSONList marshals a []string as a JSON array (Action/Resource accept a string or an array; we always
// emit an array for predictability).
func rawJSONList(vals ...string) json.RawMessage {
	b, _ := json.Marshal(vals)
	return b
}

// principalUsers builds `{"AWS":["arn:aws:iam:::user/<uid>", …]}`. RGW substitutes the tenant for AWS's
// 12-digit account id; our users live in the DEFAULT tenant, so the account segment is empty.
// (Live-verified: a reader user granted this way can read the bucket and nothing else.)
func principalUsers(uids ...string) json.RawMessage {
	arns := make([]string, 0, len(uids))
	for _, u := range uids {
		arns = append(arns, "arn:aws:iam:::user/"+u)
	}
	b, _ := json.Marshal(map[string]any{"AWS": arns})
	return b
}

// principalAnyone is the anonymous/public principal.
func principalAnyone() json.RawMessage {
	b, _ := json.Marshal(map[string]any{"AWS": []string{"*"}})
	return b
}

// anonymousReadStmt grants anonymous s3:GetObject on every object in the bucket, under the given Sid.
// A public-read bucket ACL only grants READ on the BUCKET (listing); anonymous OBJECT reads need this.
func (b *cephBackend) anonymousReadStmt(bucket, sid string) policyStmt {
	return policyStmt{
		Sid: sid, Effect: "Allow", Principal: principalAnyone(),
		Action: rawJSONList("s3:GetObject"), Resource: rawJSONList(b.bucketARN(bucket)),
	}
}

// websiteStmt is the public-read statement a static website needs.
func (b *cephBackend) websiteStmt(bucket string) policyStmt {
	return b.anonymousReadStmt(bucket, sidWebsiteRead)
}

// setPublicRead flips the explicit "public bucket" state: a public-read ACL (so the bucket can be listed)
// PLUS a policy statement granting anonymous s3:GetObject (so the objects can actually be read). The ACL
// alone leaves every object 403 on S3 — the toggle would lie.
func (b *cephBackend) setPublicRead(ctx context.Context, bucket string, public bool) error {
	doc, err := b.getPolicyDoc(ctx, bucket)
	if err != nil {
		return err
	}
	changed := false
	if public {
		doc.upsertStmt(b.anonymousReadStmt(bucket, sidPublicRead))
		changed = true
	} else {
		changed = doc.removeStmt(sidPublicRead)
	}
	if changed {
		if err := b.putPolicyDoc(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// hasAnonymousReadStmt reports whether the policy currently publishes the bucket's objects (either via the
// explicit public toggle or because a static website is enabled).
func (b *cephBackend) hasAnonymousReadStmt(ctx context.Context, bucket string) (bool, error) {
	doc, err := b.getPolicyDoc(ctx, bucket)
	if err != nil {
		return false, err
	}
	for _, s := range doc.Statement {
		if sid := sidOf(s); sid == sidPublicRead || sid == sidWebsiteRead {
			return true, nil
		}
	}
	return false, nil
}

// BucketPermission is the access level a project S3 key may be granted on ONE bucket.
type BucketPermission string

const (
	PermissionRead      BucketPermission = "READ"       // list + read objects
	PermissionReadWrite BucketPermission = "READ_WRITE" // + put/delete objects
	PermissionFull      BucketPermission = "FULL"       // + bucket-level configuration
)

// grantActions maps a permission to the S3 actions granted on the bucket and on its objects.
func grantActions(p BucketPermission) (bucketActions, objectActions []string, err error) {
	switch p {
	case PermissionRead:
		return []string{"s3:ListBucket", "s3:GetBucketLocation"},
			[]string{"s3:GetObject"}, nil
	case PermissionReadWrite:
		return []string{"s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"},
			[]string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}, nil
	case PermissionFull:
		return []string{"s3:*"}, []string{"s3:*"}, nil
	default:
		return nil, nil, fmt.Errorf("ceph-s3: unknown bucket permission %q", p)
	}
}

// grantStmts builds the (bucket-scoped, object-scoped) statements granting uid the given permission.
// Two statements are required because s3:ListBucket applies to the BUCKET arn while s3:GetObject applies
// to the OBJECT arn — a single statement with both would silently not work.
func (b *cephBackend) grantStmts(bucket, uid string, p BucketPermission) ([]policyStmt, error) {
	bucketActions, objectActions, err := grantActions(p)
	if err != nil {
		return nil, err
	}
	principal := principalUsers(uid)
	bucketARN := "arn:aws:s3:::" + bucket
	return []policyStmt{
		{
			Sid: grantSid(uid), Effect: "Allow", Principal: principal,
			Action: rawJSONList(bucketActions...), Resource: rawJSONList(bucketARN),
		},
		{
			Sid: grantSid(uid) + "-Objects", Effect: "Allow", Principal: principal,
			Action: rawJSONList(objectActions...), Resource: rawJSONList(b.bucketARN(bucket)),
		},
	}, nil
}

// GrantBucketAccess gives one RGW user (a project S3 key) access to a single bucket, merging into whatever
// policy already exists. Idempotent: re-granting replaces the previous level for that user.
func (c *Client) GrantBucketAccess(ctx context.Context, bucket, uid string, p BucketPermission) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	stmts, err := c.ceph.grantStmts(bucket, uid, p)
	if err != nil {
		return err
	}
	doc, err := c.ceph.getPolicyDoc(ctx, bucket)
	if err != nil {
		return err
	}
	for _, s := range stmts {
		doc.upsertStmt(s)
	}
	return c.ceph.putPolicyDoc(ctx, bucket, doc)
}

// RevokeBucketAccess removes a key's grant from the bucket, leaving every other statement intact.
func (c *Client) RevokeBucketAccess(ctx context.Context, bucket, uid string) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	doc, err := c.ceph.getPolicyDoc(ctx, bucket)
	if err != nil {
		return err
	}
	doc.removeStmt(grantSid(uid))
	doc.removeStmt(grantSid(uid) + "-Objects")
	return c.ceph.putPolicyDoc(ctx, bucket, doc)
}

// BucketGrant is one key's access level on a bucket, as read back from the policy document.
type BucketGrant struct {
	UID        string           `json:"uid"`
	Permission BucketPermission `json:"permission"`
}

// ListBucketGrants reports the ProxCloud-managed per-key grants on a bucket (foreign statements ignored).
func (c *Client) ListBucketGrants(ctx context.Context, bucket string) ([]BucketGrant, error) {
	if c.ceph == nil {
		return nil, ErrBucketFeatureUnsupported
	}
	doc, err := c.ceph.getPolicyDoc(ctx, bucket)
	if err != nil {
		return nil, err
	}
	out := []BucketGrant{}
	for _, s := range doc.Statement {
		sid := sidOf(s)
		// The bucket-scoped statement carries the canonical Sid; the "-Objects" twin is its detail.
		if !strings.HasPrefix(sid, sidGrantPrefix) || strings.HasSuffix(sid, "-Objects") {
			continue
		}
		uid := strings.TrimPrefix(sid, sidGrantPrefix)
		out = append(out, BucketGrant{UID: uid, Permission: permissionFromActions(actionOf(s))})
	}
	return out, nil
}

// permissionFromActions infers the level back from the bucket-scoped actions we wrote.
func permissionFromActions(raw json.RawMessage) BucketPermission {
	var actions []string
	_ = json.Unmarshal(raw, &actions)
	for _, a := range actions {
		if a == "s3:*" {
			return PermissionFull
		}
		if a == "s3:ListBucketMultipartUploads" {
			return PermissionReadWrite
		}
	}
	return PermissionRead
}

// GetBucketPolicyJSON returns the bucket's raw policy document ("" when none).
func (c *Client) GetBucketPolicyJSON(ctx context.Context, bucket string) (string, error) {
	if c.ceph == nil {
		return "", ErrBucketFeatureUnsupported
	}
	doc, err := c.ceph.getPolicyDoc(ctx, bucket)
	if err != nil {
		return "", err
	}
	if len(doc.Statement) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(doc)
	return string(raw), err
}

// ErrInvalidBucketPolicy marks a customer-supplied document that parsed as JSON but is not a bucket
// policy (wrong document type, typo'd keys, or no statements). Handlers map it to a 400. Without this
// gate, plain json.Unmarshal ignored unknown fields, so pasting e.g. a CORS configuration into the
// policy editor decoded to an EMPTY doc, saved nothing, and still reported success.
var ErrInvalidBucketPolicy = errors.New("invalid bucket policy")

// parseCustomerPolicyDoc STRICTLY parses a hand-supplied policy document: unknown top-level fields are
// rejected instead of ignored, and a document with no Statement is refused rather than silently saved
// as "no custom policy".
func parseCustomerPolicyDoc(policyJSON string) (*policyDoc, error) {
	doc := &policyDoc{}
	dec := json.NewDecoder(strings.NewReader(policyJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(doc); err != nil {
		if strings.Contains(err.Error(), `unknown field "CORSRules"`) {
			return nil, fmt.Errorf("%w: this is a CORS configuration, not a bucket policy — CORS rules are configured separately from the policy", ErrInvalidBucketPolicy)
		}
		return nil, fmt.Errorf(`%w: %v — a policy document has the shape {"Version":"2012-10-17","Statement":[…]}`, ErrInvalidBucketPolicy, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after the policy document", ErrInvalidBucketPolicy)
	}
	if len(doc.Statement) == 0 {
		return nil, fmt.Errorf(`%w: the document has no "Statement" — to remove the custom policy, use clear instead`, ErrInvalidBucketPolicy)
	}
	if doc.Version == "" {
		doc.Version = "2012-10-17"
	}
	return doc, nil
}

// SetBucketPolicyJSON replaces the CUSTOMER portion of the bucket policy with the supplied document. Any
// ProxCloud-managed statements (website public-read, per-key grants) are stripped from the input and then
// re-applied from the CURRENT live policy, so a hand-edited policy can never silently drop a grant or
// leave a website publicly readable after it was disabled. Empty input clears the customer statements.
func (c *Client) SetBucketPolicyJSON(ctx context.Context, bucket, policyJSON string) error {
	if c.ceph == nil {
		return ErrBucketFeatureUnsupported
	}
	next := &policyDoc{Version: "2012-10-17"}
	if strings.TrimSpace(policyJSON) != "" {
		parsed, err := parseCustomerPolicyDoc(policyJSON)
		if err != nil {
			return err
		}
		next = parsed
		// Drop any ProxCloud Sid the caller tried to set by hand — those are ours to manage. Everything else
		// is carried through byte-for-byte, so fields we do not model (NotAction, NotPrincipal, …) survive.
		kept := next.Statement[:0]
		for _, s := range next.Statement {
			if !isPlatformSid(sidOf(s)) {
				kept = append(kept, s)
			}
		}
		next.Statement = kept
	}
	live, err := c.ceph.getPolicyDoc(ctx, bucket)
	if err != nil {
		return err
	}
	next.Statement = append(next.Statement, live.ownStmts()...)
	return c.ceph.putPolicyDoc(ctx, bucket, next)
}
