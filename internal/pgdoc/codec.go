package pgdoc

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Documents are stored as plain JSON honouring the `json` struct tags: times
// become RFC3339 strings, money (shopspring/decimal) becomes a JSON number
// (preserved at full precision by Postgres jsonb's numeric storage), and every
// number in a free-form value decodes back through json.Number so int/float
// round-trips are stable. The filter translator + expression indexes read the
// field text directly (times via the IMMUTABLE pgdoc_ts() helper, money via a
// numeric cast); no ext-JSON wrappers.
//
// The `_id` field is NOT stored inside doc — it lives in the id column and is
// injected back on read (as "id", matching the json:"id" struct tag).

// Marshal encodes v to the stored JSON form, returning the body without the id
// and the extracted id value ("" when absent/empty). The id is taken from the
// top-level "_id" (free-form maps) or "id" (json-tagged structs) key.
func Marshal(v any) (body []byte, id string, err error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", fmt.Errorf("pgdoc marshal: %w", err)
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, "", fmt.Errorf("pgdoc marshal reparse: %w", err)
	}
	for _, key := range []string{"_id", "id"} {
		if rawID, ok := m[key]; ok {
			var s string
			if json.Unmarshal(rawID, &s) == nil && s != "" && id == "" {
				id = s
			}
			delete(m, key)
		}
	}
	body, err = json.Marshal(m)
	if err != nil {
		return nil, "", fmt.Errorf("pgdoc marshal re-encode: %w", err)
	}
	return body, id, nil
}

// Unmarshal decodes a stored document into out, injecting the id so `json:"id"`
// fields are populated. Numbers in free-form (interface/map) targets decode as
// float64 (encoding/json's default), which is what the domain's map readers
// expect; writes preserve exact numeric text (Marshal reparses as RawMessage).
func Unmarshal(body []byte, id string, out any) error {
	if len(body) == 0 {
		return fmt.Errorf("pgdoc unmarshal: empty document")
	}
	// Inject the id under BOTH "id" (json:"id" struct fields) and "_id" (free-form pgdoc.M reads that
	// still key on "_id") without disturbing the rest. Marshal strips both back out to the id column,
	// so the doc never actually stores them.
	buf := make([]byte, 0, len(body)+2*len(id)+24)
	idJSON, _ := json.Marshal(id)
	buf = append(buf, `{"id":`...)
	buf = append(buf, idJSON...)
	buf = append(buf, `,"_id":`...)
	buf = append(buf, idJSON...)
	if len(body) > 2 { // non-empty object
		buf = append(buf, ',')
		buf = append(buf, body[1:]...)
	} else {
		buf = append(buf, '}')
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("pgdoc unmarshal: %w", err)
	}
	return nil
}

// marshalPatch encodes a $set-style map to a JSON object fragment (values in
// stored form, keys as given).
func marshalPatch(set map[string]any) ([]byte, error) {
	b, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("pgdoc patch: %w", err)
	}
	return b, nil
}

// encodeValue encodes one Go value to its stored-JSON fragment.
func encodeValue(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("pgdoc encode value: %w", err)
	}
	return string(b), nil
}
