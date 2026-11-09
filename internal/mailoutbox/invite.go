package mailoutbox

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const InviteKind = "project-invite/v1"

// Invite is the single-recipient, fixed PROJECT_INVITE contract, not a generic
// event/template/URL API. The rendered join URL is credential-bearing: ALL draft
// inputs, including that URL, are sealed and keyed-fingerprinted, never metadata.
// InviteID is the persistent business document ID, never an email or invite token.
// Date and ExpiresAt are frozen once by the primary business transaction.
type Invite struct {
	InviteID    string
	Date        time.Time
	ExpiresAt   time.Time
	Recipient   Recipient
	From        string
	Subject     string
	TextBody    string
	HTMLBody    string
	Disposition string
}

func (Invite) String() string       { return "[project invite redacted]" }
func (Invite) LogValue() slog.Value { return slog.StringValue("[project invite redacted]") }

// EnqueueInviteTx joins ONLY this store's already-open primary business TX. It
// neither ensures tables nor opens another transaction or performs network I/O.
func (s *Store) EnqueueInviteTx(ctx context.Context, in Invite) (QueueResult, error) {
	if _, err := s.db.Tx(ctx); err != nil {
		return QueueResult{}, err
	}
	ps, err := prepareInvite(in)
	if err != nil {
		return QueueResult{}, err
	}
	return s.persistPrepared(ctx, ps)
}

// PrepareInvite freezes the typed contract in memory, without I/O. Returned wire
// bytes are credential-bearing, just like Envelope.Wire; do not log or serialize.
func PrepareInvite(in Invite) ([]Envelope, error) {
	ps, err := prepareInvite(in)
	if err != nil {
		return nil, err
	}
	var f invitePayload
	if json.Unmarshal(ps[0].plaintext, &f) != nil {
		return nil, ErrInvalid
	}
	if len(f.Payload.Wire) == 0 {
		return nil, nil
	}
	return []Envelope{{from: f.Payload.From, to: f.Payload.To, wire: f.Payload.Wire}}, nil
}

type invitePayload struct {
	Kind     string `json:"kind"`
	Version  int    `json:"version"`
	EventKey string `json:"eventKey"`
	Input    Invite `json:"input"`
	Payload  frozen `json:"payload"`
}

func validInviteID(id string) bool {
	if len(id) == 24 {
		_, err := hex.DecodeString(id)
		return err == nil
	}
	if len(id) != 36 || !opaque.MatchString(id) {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

func prepareInvite(in Invite) ([]prepared, error) {
	if !validInviteID(in.InviteID) || !opaque.MatchString(in.Recipient.Key) || in.Date.IsZero() || !in.ExpiresAt.After(in.Date) || in.ExpiresAt.After(in.Date.Add(24*time.Hour)) {
		return nil, ErrInvalid
	}
	if len(in.Subject) > 512 || !safeHeader(in.Subject) || !utf8.ValidString(in.TextBody) || !utf8.ValidString(in.HTMLBody) || len(in.TextBody)+len(in.HTMLBody) > MaxWireBytes {
		return nil, ErrInvalid
	}
	state := in.Disposition
	switch state {
	case "":
		state = Pending
	case Suppressed, Blocked, NoRecipient:
	default:
		return nil, ErrInvalid
	}
	var to, from *mail.Address
	var err error
	if in.Recipient.Address != "" {
		to, err = address(in.Recipient.Address)
		if err != nil || state == NoRecipient {
			return nil, ErrInvalid
		}
	} else if state != NoRecipient {
		return nil, ErrInvalid
	}
	if in.From != "" || state == Pending {
		from, err = address(in.From)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	// Preserve every input (even subsecond decision times and non-delivery drafts)
	// in the sealed canonical payload. MIME's Date rounding is NOT dedup identity.
	in.Date, in.ExpiresAt = in.Date.UTC(), in.ExpiresAt.UTC()
	event := hash(InviteKind + "\x00" + in.InviteID)
	f := frozen{EndDate: in.ExpiresAt, Disposition: state}
	if to != nil {
		f.To = to.Address
	}
	if from != nil {
		f.From = from.Address
	}
	if from != nil && to != nil {
		id := hash(event + "\x00" + in.Recipient.Key)
		var b strings.Builder
		fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@mail.proxcloud.invalid>\r\nMIME-Version: 1.0\r\n", from.String(), (&mail.Address{Address: to.Address}).String(), strings.ReplaceAll(mime.QEncoding.Encode("UTF-8", in.Subject), "?= =?", "?=\r\n =?"), in.Date.Format(time.RFC1123Z), id)
		write := func(kind, body string) {
			fmt.Fprintf(&b, "Content-Type: %s; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", kind)
			encodeBody(&b, []byte(body))
		}
		if in.TextBody != "" && in.HTMLBody != "" {
			boundary := "pc_" + id
			fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n--%s\r\n", boundary, boundary)
			write("text/plain", in.TextBody)
			fmt.Fprintf(&b, "--%s\r\n", boundary)
			write("text/html", in.HTMLBody)
			fmt.Fprintf(&b, "--%s--\r\n", boundary)
		} else if in.HTMLBody != "" {
			write("text/html", in.HTMLBody)
		} else {
			write("text/plain", in.TextBody)
		}
		f.Wire = []byte(b.String())
		if len(f.Wire) > MaxWireBytes {
			return nil, ErrInvalid
		}
	}
	pt, err := json.Marshal(invitePayload{Kind: InviteKind, Version: Version, EventKey: event, Input: in, Payload: f})
	if err != nil || len(pt) > 1499900 { // ciphertext must fit the existing bounded column
		return nil, ErrInvalid
	}
	return []prepared{{kind: InviteKind, version: Version, event: event, recipient: hash(in.Recipient.Key), state: state, expiry: in.ExpiresAt.Truncate(time.Microsecond), plaintext: pt}}, nil
}
