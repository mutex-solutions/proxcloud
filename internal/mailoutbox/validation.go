package mailoutbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const ValidationApprovedKind = "validation-approved/v1"

// ValidationApprovalMaxAge is a fixed notification lifetime, not a KYC document
// expiry or an account/service activation deadline.
const ValidationApprovalMaxAge = 24 * time.Hour

// ValidationApproved attests ONLY approval of billingProfileValidation. It has no
// template/kind/transport/attachment selection or credential-bearing links. PII,
// the normalized recipient manifest and the complete fixed snapshot stay sealed.
// Recipient metadata is an opaque per-event ordinal, NEVER an email-derived key.
type ValidationApproved struct {
	ValidationID, ApprovalGeneration  string
	Date, ExpiresAt                   time.Time
	Recipients                        []string
	FullName, LoginURL                string
	From, Subject, TextBody, HTMLBody string
	Disposition                       string
}

func (ValidationApproved) String() string   { return "[validation approval redacted]" }
func (ValidationApproved) GoString() string { return "[validation approval redacted]" }
func (ValidationApproved) LogValue() slog.Value {
	return slog.StringValue("[validation approval redacted]")
}

// EnqueueValidationTx joins this store's already-open primary business TX only.
func (s *Store) EnqueueValidationTx(ctx context.Context, in ValidationApproved) (QueueResult, error) {
	if _, err := s.db.Tx(ctx); err != nil {
		return QueueResult{}, err
	}
	ps, err := prepareValidation(in)
	if err != nil {
		return QueueResult{}, err
	}
	return s.persistPrepared(ctx, ps)
}

// PrepareValidationApproved performs no I/O. Returned envelopes contain PII;
// like Envelope.Wire they must not be logged or exposed in metadata APIs.
func PrepareValidationApproved(in ValidationApproved) ([]Envelope, error) {
	ps, err := prepareValidation(in)
	if err != nil {
		return nil, err
	}
	out := make([]Envelope, 0, len(ps))
	for _, p := range ps {
		var v validationPayload
		if json.Unmarshal(p.plaintext, &v) != nil {
			return nil, ErrInvalid
		}
		if len(v.Payload.Wire) != 0 {
			out = append(out, Envelope{from: v.Payload.From, to: v.Payload.To, wire: v.Payload.Wire})
		}
	}
	return out, nil
}

type validationPayload struct {
	Kind           string             `json:"kind"`
	Version        int                `json:"version"`
	EventKey       string             `json:"eventKey"`
	Input          ValidationApproved `json:"input"`
	SnapshotDigest string             `json:"snapshotDigest"`
	Payload        frozen             `json:"payload"`
}

func prepareValidation(in ValidationApproved) ([]prepared, error) {
	generation, err := uuid.Parse(in.ApprovalGeneration)
	if !validInviteID(in.ValidationID) || err != nil || generation == uuid.Nil || generation.String() != in.ApprovalGeneration || in.Date.IsZero() || !in.ExpiresAt.Equal(in.Date.Add(ValidationApprovalMaxAge)) || len(in.Recipients) > MaxRecipients {
		return nil, ErrInvalid
	}
	u, err := url.Parse(in.LoginURL)
	if err != nil || len(in.LoginURL) > 2048 || !safeHeader(in.LoginURL) || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || len(in.FullName) > 512 || !safeHeader(in.FullName) {
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
	// A nonempty malformed entry fails even for suppressed/blocked snapshots.
	// Normalize BEFORE assigning ordinals; duplicate spelling is not identity.
	addresses := make([]string, 0, len(in.Recipients))
	seen := map[string]bool{}
	for _, raw := range in.Recipients {
		a, err := address(raw)
		if err != nil {
			return nil, ErrInvalid
		}
		v := strings.ToLower(a.Address)
		// Match billing.Recipients: first spelling wins, preserving local-part
		// case. Lowercase is ONLY a dedup key, never an SMTP destination.
		if !seen[v] {
			addresses = append(addresses, a.Address)
			seen[v] = true
		}
	}
	sort.Slice(addresses, func(i, j int) bool { return strings.ToLower(addresses[i]) < strings.ToLower(addresses[j]) })
	if state == NoRecipient && len(addresses) != 0 {
		return nil, ErrInvalid
	}
	if len(addresses) == 0 {
		state = NoRecipient
	}
	in.Recipients = addresses
	in.Date, in.ExpiresAt = in.Date.UTC(), in.ExpiresAt.UTC()
	var from *mail.Address
	if in.From != "" || state == Pending {
		from, err = address(in.From)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	event := hash(ValidationApprovedKind + "\x00" + in.ValidationID + "\x00" + in.ApprovalGeneration)
	snapshot, _ := json.Marshal(in)
	digest := hash(string(snapshot))
	if len(addresses) == 0 {
		addresses = []string{""}
	}
	out := make([]prepared, 0, len(addresses))
	for i, to := range addresses {
		ordinal := fmt.Sprintf("recipient-%02d", i)
		f := frozen{To: to, EndDate: in.ExpiresAt, Disposition: state}
		if from != nil {
			f.From = from.Address
		}
		if from != nil && to != "" {
			id := hash(event + "\x00" + ordinal)
			var b strings.Builder
			fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@mail.proxcloud.invalid>\r\nMIME-Version: 1.0\r\n", from.String(), (&mail.Address{Address: to}).String(), strings.ReplaceAll(mime.QEncoding.Encode("UTF-8", in.Subject), "?= =?", "?=\r\n =?"), in.Date.Format(time.RFC1123Z), id)
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
		pt, err := json.Marshal(validationPayload{Kind: ValidationApprovedKind, Version: Version, EventKey: event, Input: in, SnapshotDigest: digest, Payload: f})
		if err != nil || len(pt) > 1499900 {
			return nil, ErrInvalid
		}
		out = append(out, prepared{kind: ValidationApprovedKind, version: Version, event: event, recipient: hash(ordinal), state: state, expiry: in.ExpiresAt.Truncate(time.Microsecond), plaintext: pt})
	}
	return out, nil
}
