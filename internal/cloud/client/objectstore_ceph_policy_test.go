package client

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func sids(d *policyDoc) []string {
	out := make([]string, 0, len(d.Statement))
	for _, s := range d.Statement {
		out = append(out, sidOf(s))
	}
	return out
}

func raws(ss ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(ss))
	for _, s := range ss {
		out = append(out, json.RawMessage(s))
	}
	return out
}

// A bucket policy is ONE document shared by the website toggle, per-key grants and the customer's own
// statements. Every mutation must be an upsert/remove keyed on Sid — never a blind replace.
func TestPolicyDocUpsertAndRemovePreserveForeignStatements(t *testing.T) {
	doc := &policyDoc{Version: "2012-10-17", Statement: raws(
		`{"Sid":"CustomerDenyOldClients","Effect":"Deny"}`,
		`{"Effect":"Allow"}`, // an unnamed customer statement
	)}
	b := &cephBackend{}

	doc.upsertStmt(b.websiteStmt("site"))
	if got := sids(doc); len(got) != 3 || got[2] != sidWebsiteRead {
		t.Fatalf("after website upsert, sids = %v", got)
	}
	// Upserting the same Sid replaces in place rather than appending a duplicate.
	doc.upsertStmt(b.websiteStmt("site"))
	if len(doc.Statement) != 3 {
		t.Fatalf("duplicate website statement: %v", sids(doc))
	}
	grants, err := b.grantStmts("site", "proj1-app", PermissionReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		doc.upsertStmt(g)
	}
	if len(doc.Statement) != 5 {
		t.Fatalf("expected 5 statements, got %v", sids(doc))
	}

	// Disabling the website must remove ONLY the website statement.
	if !doc.removeStmt(sidWebsiteRead) {
		t.Fatal("website statement not removed")
	}
	got := sids(doc)
	if len(got) != 4 {
		t.Fatalf("removeStmt removed too much: %v", got)
	}
	for _, s := range got {
		if s == sidWebsiteRead {
			t.Fatal("website statement still present")
		}
	}
	if got[0] != "CustomerDenyOldClients" || got[1] != "" {
		t.Fatalf("customer statements disturbed: %v", got)
	}
	if !strings.HasPrefix(got[2], sidGrantPrefix) {
		t.Fatalf("grant statement lost: %v", got)
	}
}

// A grant needs TWO statements: s3:ListBucket applies to the bucket ARN, s3:GetObject to the object ARN.
// Collapsing them into one silently does not work.
func TestGrantStmtsSplitBucketAndObjectARNs(t *testing.T) {
	b := &cephBackend{uid: "proj1"}
	stmts, err := b.grantStmts("mybucket", "proj1-reader", PermissionRead)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d", len(stmts))
	}
	if !strings.Contains(string(stmts[0].Resource), `"arn:aws:s3:::mybucket"`) {
		t.Errorf("bucket-scoped resource = %s", stmts[0].Resource)
	}
	if !strings.Contains(string(stmts[1].Resource), `"arn:aws:s3:::mybucket/*"`) {
		t.Errorf("object-scoped resource = %s", stmts[1].Resource)
	}
	if !strings.Contains(string(stmts[0].Principal), "arn:aws:iam:::user/proj1-reader") {
		t.Errorf("principal = %s", stmts[0].Principal)
	}
	// A read grant must not carry any write action.
	all := string(stmts[0].Action) + string(stmts[1].Action)
	for _, forbidden := range []string{"s3:PutObject", "s3:DeleteObject", "s3:*"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("READ grant leaked %s: %s", forbidden, all)
		}
	}
}

func TestPermissionRoundTrip(t *testing.T) {
	b := &cephBackend{}
	for _, p := range []BucketPermission{PermissionRead, PermissionReadWrite, PermissionFull} {
		stmts, err := b.grantStmts("bkt", "u", p)
		if err != nil {
			t.Fatal(err)
		}
		if got := permissionFromActions(stmts[0].Action); got != p {
			t.Errorf("permission %s round-tripped to %s", p, got)
		}
	}
	if _, err := b.grantStmts("bkt", "u", BucketPermission("ADMIN")); err == nil {
		t.Error("unknown permission should error")
	}
}

// A hand-supplied policy must never be able to forge or drop a ProxCloud-managed statement.
func TestSetPolicyStripsPlatformSidsFromInput(t *testing.T) {
	var doc policyDoc
	input := `{"Version":"2012-10-17","Statement":[
	  {"Sid":"ProxCloudPublicWebsiteRead","Effect":"Allow"},
	  {"Sid":"ProxCloudGrant-someoneelse","Effect":"Allow"},
	  {"Sid":"MyOwnRule","Effect":"Deny"}]}`
	if err := json.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}
	kept := doc.Statement[:0]
	for _, s := range doc.Statement {
		if !isPlatformSid(sidOf(s)) {
			kept = append(kept, s)
		}
	}
	doc.Statement = kept
	if len(doc.Statement) != 1 || sidOf(doc.Statement[0]) != "MyOwnRule" {
		t.Fatalf("platform sids not stripped: %v", sids(&doc))
	}
}

// A customer statement may use S3 fields ProxCloud does not model (NotPrincipal, NotAction, NotResource,
// exotic Condition shapes). Mutating OUR statements must not silently rewrite theirs: the foreign
// statement has to survive byte-for-byte, or we would be quietly widening/narrowing their access control.
func TestPolicyMutationPreservesUnmodelledFields(t *testing.T) {
	foreign := `{"Sid":"CustomerRule","Effect":"Deny","NotPrincipal":{"AWS":["arn:aws:iam:::user/keep"]},` +
		`"NotAction":["s3:GetObject"],"NotResource":["arn:aws:s3:::b/*"],` +
		`"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}`
	doc := &policyDoc{Version: "2012-10-17", Statement: raws(foreign)}
	b := &cephBackend{}

	// Every kind of ProxCloud mutation touches the document.
	doc.upsertStmt(b.websiteStmt("b"))
	grants, err := b.grantStmts("b", "proj1-app", PermissionFull)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		doc.upsertStmt(g)
	}
	doc.removeStmt(sidWebsiteRead)

	if len(doc.Statement) != 3 {
		t.Fatalf("unexpected statements: %v", sids(doc))
	}
	// The foreign statement must be the SAME bytes we started with.
	var want, got any
	if err := json.Unmarshal([]byte(foreign), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(doc.Statement[0], &got); err != nil {
		t.Fatal(err)
	}
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if string(wb) != string(gb) {
		t.Fatalf("foreign statement was rewritten:\n want %s\n got  %s", wb, gb)
	}
	for _, field := range []string{"NotPrincipal", "NotAction", "NotResource", "Condition"} {
		if !strings.Contains(string(doc.Statement[0]), field) {
			t.Errorf("field %s was dropped from the customer statement", field)
		}
	}
}

// The policy editor used to accept ANY JSON object: unknown fields were ignored, so a pasted CORS
// configuration (or a typo'd "Statements" key) decoded to an empty doc, saved NOTHING, and still
// toasted "Policy saved" — the text was gone on the next load. Non-policy documents must be rejected.
func TestParseCustomerPolicyRejectsNonPolicyJSON(t *testing.T) {
	cases := map[string]struct {
		input    string
		wantHint string
	}{
		"CORS config pasted into the policy editor": {
			input:    `{"CORSRules":[{"AllowedOrigins":["https://app.example.com"],"AllowedMethods":["GET"]}]}`,
			wantHint: "CORS configuration",
		},
		"typo'd Statements key": {
			input:    `{"Version":"2012-10-17","Statements":[{"Effect":"Allow"}]}`,
			wantHint: `unknown field "Statements"`,
		},
		"empty object": {
			input:    `{}`,
			wantHint: `no "Statement"`,
		},
		"explicit empty statement array": {
			input:    `{"Version":"2012-10-17","Statement":[]}`,
			wantHint: `no "Statement"`,
		},
		"trailing garbage": {
			input:    `{"Version":"2012-10-17","Statement":[{"Effect":"Allow"}]} extra`,
			wantHint: "trailing data",
		},
		"not JSON at all": {
			input:    `hello`,
			wantHint: "invalid",
		},
	}
	for name, tc := range cases {
		_, err := parseCustomerPolicyDoc(tc.input)
		if err == nil {
			t.Errorf("%s: accepted, want rejection", name)
			continue
		}
		if !errors.Is(err, ErrInvalidBucketPolicy) {
			t.Errorf("%s: error not marked ErrInvalidBucketPolicy: %v", name, err)
		}
		if !strings.Contains(err.Error(), tc.wantHint) {
			t.Errorf("%s: error %q does not carry hint %q", name, err, tc.wantHint)
		}
	}
}

// A real policy document must parse, default its Version, and keep an Id if it has one — the Id used to
// be silently dropped on every read-modify-write cycle.
func TestParseCustomerPolicyAcceptsRealPolicies(t *testing.T) {
	doc, err := parseCustomerPolicyDoc(`{"Statement":[{"Sid":"Mine","Effect":"Deny"}]}`)
	if err != nil {
		t.Fatalf("minimal policy rejected: %v", err)
	}
	if doc.Version != "2012-10-17" {
		t.Errorf("Version not defaulted: %q", doc.Version)
	}
	doc, err = parseCustomerPolicyDoc(`{"Version":"2008-10-17","Id":"MyPolicy","Statement":[{"Effect":"Allow"}]}`)
	if err != nil {
		t.Fatalf("policy with Id rejected: %v", err)
	}
	if doc.ID != "MyPolicy" || doc.Version != "2008-10-17" {
		t.Errorf("Id/Version not preserved: %+v", doc)
	}
	raw, _ := json.Marshal(doc)
	if !strings.Contains(string(raw), `"Id":"MyPolicy"`) {
		t.Errorf("Id dropped on re-encode: %s", raw)
	}
}

// COMPLIANCE retention would make the project's RGW user un-purgeable → project undeletable. Refuse it.
func TestObjectLockRefusesCompliance(t *testing.T) {
	cc, err := NewCephS3(t.Context(), ObjectStoreConfig{S3Endpoint: "http://x", Region: "us-east-1", RGWUID: "p", ProjectAccessKey: "a", ProjectSecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	err = cc.SetObjectLockDefaults(t.Context(), "b", ObjectLockCompliance, 1)
	if err == nil || !strings.Contains(err.Error(), "COMPLIANCE") {
		t.Fatalf("want COMPLIANCE refusal, got %v", err)
	}
}

// A project must never be able to create or purge an RGW user outside its own uid prefix.
func TestChildUIDOwnershipGuard(t *testing.T) {
	b := &cephBackend{uid: "dev_proj1"}
	if err := b.assertOwnedUID("dev_proj1-app"); err != nil {
		t.Errorf("own child rejected: %v", err)
	}
	for _, bad := range []string{"dev_proj2-app", "dev_proj1", "", "admin", "dev_proj1x-app"} {
		if err := b.assertOwnedUID(bad); err == nil {
			t.Errorf("uid %q should be refused", bad)
		}
	}
}
