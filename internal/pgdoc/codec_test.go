package pgdoc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type codecDoc struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Email     string          `json:"email,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Custom    map[string]any  `json:"customInfo,omitempty"`
	Amount    decimal.Decimal `json:"amount"`
	CreatedAt *time.Time      `json:"createdAt,omitempty"`
	Count     int             `json:"count"`
}

func TestCodecRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 30, 0, 123e6, time.UTC)
	in := codecDoc{
		ID:        "665f00000000000000000001",
		Name:      "acme",
		Tags:      []string{"a", "b"},
		Custom:    map[string]any{"lang": "ro-ro", "n": int64(7)},
		Amount:    decimal.RequireFromString("10.505"),
		CreatedAt: &now,
		Count:     3,
	}
	body, id, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if id != in.ID {
		t.Fatalf("id: got %q", id)
	}
	s := string(body)
	if strings.Contains(s, "_id") || strings.Contains(s, `"id"`) {
		t.Errorf("body must not contain the id: %s", s)
	}
	// omitempty parity: absent email stays absent (null-vs-absent invariant).
	if strings.Contains(s, "email") {
		t.Errorf("empty email must be absent: %s", s)
	}
	// Plain JSON: no ext-JSON wrappers; money is numeric text, time is RFC3339.
	if strings.Contains(s, "$numberDecimal") || strings.Contains(s, "$date") {
		t.Errorf("stored form must not carry ext-JSON wrappers: %s", s)
	}
	if !strings.Contains(s, "10.505") {
		t.Errorf("decimal value missing: %s", s)
	}
	if !strings.Contains(s, "2026-07-06T10:30:00.123Z") {
		t.Errorf("RFC3339 time missing: %s", s)
	}

	var out codecDoc
	if err := Unmarshal(body, id, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Name != in.Name || out.Count != 3 {
		t.Errorf("round trip: %+v", out)
	}
	if !out.Amount.Equal(in.Amount) {
		t.Errorf("amount: %v", out.Amount)
	}
	if !out.CreatedAt.Equal(now) {
		t.Errorf("createdAt: %v", out.CreatedAt)
	}
	if out.Custom["lang"] != "ro-ro" {
		t.Errorf("custom: %v", out.Custom)
	}

	// Stability: second marshal identical (jsonb reorder tolerated separately).
	body2, _, err := Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m1, m2 any
	_ = json.Unmarshal(body, &m1)
	_ = json.Unmarshal(body2, &m2)
	j1, _ := json.Marshal(m1)
	j2, _ := json.Marshal(m2)
	if string(j1) != string(j2) {
		t.Errorf("unstable round trip:\n%s\n%s", j1, j2)
	}
}

func TestCodecEmptyID(t *testing.T) {
	body, id, err := Marshal(codecDoc{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("expected empty id, got %q", id)
	}
	var out codecDoc
	if err := Unmarshal(body, "generated123", &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "generated123" {
		t.Errorf("injected id: %q", out.ID)
	}
}

func TestNewID(t *testing.T) {
	a, b := NewID(), NewID()
	if len(a) != 24 || len(b) != 24 || a == b {
		t.Fatalf("ids: %q %q", a, b)
	}
	// Time-prefix: ids minted later must not sort before earlier ones.
	if b < a[:8] {
		t.Errorf("time prefix ordering broken: %s then %s", a, b)
	}
}
