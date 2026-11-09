package billing

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// BankTransferPublic maps _id → id and reduces the proof to its metadata (never the bytes),
// without mutating the stored doc.
func TestBankTransferPublicStripsProof(t *testing.T) {
	doc := pgdoc.M{
		"_id": "bt-1", "status": "PENDING", "bankReference": "TRX-42",
		"proof": map[string]any{"fileName": "slip.png", "contentType": "image/png", "dataBase64": "iVBORw0KGgo=", "size": float64(8)},
	}
	out := BankTransferPublic(doc)
	if out["id"] != "bt-1" || out["_id"] != nil || out["bankReference"] != "TRX-42" {
		t.Fatalf("shape: %v", out)
	}
	p, _ := out["proof"].(pgdoc.M)
	if p == nil || p["fileName"] != "slip.png" || p["contentType"] != "image/png" || p["size"] != float64(8) || p["dataBase64"] != nil {
		t.Fatalf("proof: %v", out["proof"])
	}
	if doc["_id"] != "bt-1" || doc["proof"].(map[string]any)["dataBase64"] != "iVBORw0KGgo=" {
		t.Fatalf("input mutated: %v", doc)
	}
	if BankTransferPublic(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	if out := BankTransferPublic(pgdoc.M{"_id": "x"}); out["proof"] != nil || out["id"] != "x" {
		t.Fatalf("no-proof doc: %v", out)
	}
}
