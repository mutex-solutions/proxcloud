package mailoutbox

import (
	"bytes"
	"encoding/json"
	"time"
)

// Only these two constructors/families are allowed. No caller-selected kind API.
// Keep savings' original aad() bytes and canonical plaintext completely intact.
func payloadAAD(id, kind string, version int) ([]byte, error) {
	if version != Version {
		return nil, ErrPayload
	}
	switch kind {
	case Kind:
		return aad(id), nil
	case InviteKind:
		return []byte(id + "\x00" + InviteKind + "\x001"), nil
	default:
		return nil, ErrPayload
	}
}

// Invite additionally authenticates row/event metadata against the sealed typed
// payload. Rebuilding the finite canonical format prevents unknown-field fallback.
func validInvitePayload(pt []byte, event, recipient string, expiry time.Time) bool {
	var v invitePayload
	if json.Unmarshal(pt, &v) != nil || v.Kind != InviteKind || v.Version != Version || v.EventKey != event {
		return false
	}
	ps, err := prepareInvite(v.Input)
	return err == nil && ps[0].event == event && ps[0].recipient == recipient && ps[0].expiry.Equal(expiry) && bytes.Equal(ps[0].plaintext, pt)
}
