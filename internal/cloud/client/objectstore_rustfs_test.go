package client

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestProjectPolicyRoundTrip(t *testing.T) {
	raw, _ := json.Marshal(projectPolicyDoc([]string{"a", "b"}))
	var doc struct{ Statement []rustfsPolicyStmt }
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := policyBuckets(doc.Statement); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("buckets = %v", got)
	}
	// The Deny on bucket create/delete never reads as ownership, and an empty project owns nothing.
	raw, _ = json.Marshal(projectPolicyDoc(nil))
	doc.Statement = nil
	_ = json.Unmarshal(raw, &doc)
	if got := policyBuckets(doc.Statement); len(got) != 0 {
		t.Fatalf("empty project owns %v", got)
	}
	if doc.Statement[0].Effect != "Deny" {
		t.Fatalf("first statement = %+v", doc.Statement[0])
	}
}

func TestKeepLastUsage(t *testing.T) {
	old := map[string]any{"sizeInBytes": 42.0, "objectCount": 3.0, "sizeInGb": "0.01"}
	fresh := map[string]any{"sizeInBytes": int64(0), "objectCount": int64(0), "usageUnknown": true, "quotaBytes": int64(9)}
	KeepLastUsage(old, fresh)
	if fresh["sizeInBytes"] != 42.0 || fresh["objectCount"] != 3.0 || fresh["usageUnknown"] != nil || fresh["quotaBytes"] != int64(9) {
		t.Fatalf("merged = %v", fresh)
	}
	known := map[string]any{"sizeInBytes": int64(7)}
	KeepLastUsage(old, known)
	if known["sizeInBytes"] != int64(7) {
		t.Fatalf("known usage overwritten: %v", known)
	}
}
