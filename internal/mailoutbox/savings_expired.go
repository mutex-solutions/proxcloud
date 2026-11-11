package mailoutbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const SavingsExpiredKind = "savings-contract-expired/v1"
const SavingsExpiredMaxAge = 24 * time.Hour

// SavingsExpired is ONLY a contract-expired snapshot, never a reminder or an
// activation receipt. TermGeneration is SHA256 of UTC-microsecond EndDate, not a
// lifecycle UUID: reactivation with the same EndDate deliberately cannot resend.
// All recipient/variable/body data is sealed. No caller-selected template, kind,
// URL, arbitrary variables, attachment, transport or job function is accepted.
type SavingsExpired struct {
	ContractID, TermGeneration                                             string
	CanonicalEndDate, Date, ExpiresAt                                      time.Time
	Recipients                                                             []string
	FullName, SavingsPlanName, Currency, StartDate, MonthlyCommittedAmount string
	From, Subject, TextBody, HTMLBody                                      string
	Disposition                                                            string
}

func (SavingsExpired) String() string       { return "[savings expired redacted]" }
func (SavingsExpired) GoString() string     { return "[savings expired redacted]" }
func (SavingsExpired) LogValue() slog.Value { return slog.StringValue("[savings expired redacted]") }

// EnqueueSavingsExpiredTx joins only this store's already-open business TX.
func (s *Store) EnqueueSavingsExpiredTx(ctx context.Context, in SavingsExpired) (QueueResult, error) {
	if _, err := s.db.Tx(ctx); err != nil {
		return QueueResult{}, err
	}
	ps, err := prepareSavingsExpired(in)
	if err != nil {
		return QueueResult{}, err
	}
	return s.persistPrepared(ctx, ps)
}

// PrepareSavingsExpired is pure. Returned envelopes contain PII, not log data.
func PrepareSavingsExpired(in SavingsExpired) ([]Envelope, error) {
	ps, err := prepareSavingsExpired(in)
	if err != nil {
		return nil, err
	}
	out := make([]Envelope, 0, len(ps))
	for _, p := range ps {
		var v savingsExpiredPayload
		if json.Unmarshal(p.plaintext, &v) != nil {
			return nil, ErrInvalid
		}
		if len(v.Payload.Wire) != 0 {
			out = append(out, Envelope{from: v.Payload.From, to: v.Payload.To, wire: v.Payload.Wire})
		}
	}
	return out, nil
}

type savingsExpiredPayload struct {
	Kind           string         `json:"kind"`
	Version        int            `json:"version"`
	EventKey       string         `json:"eventKey"`
	Input          SavingsExpired `json:"input"`
	SnapshotDigest string         `json:"snapshotDigest"`
	Payload        frozen         `json:"payload"`
}

func prepareSavingsExpired(in SavingsExpired) ([]prepared, error) {
	end := in.CanonicalEndDate.UTC().Truncate(time.Microsecond)
	if !validInviteID(in.ContractID) || end.IsZero() || end.Year() < 1 || end.Year() > 9999 ||
		!in.CanonicalEndDate.Equal(end) || in.TermGeneration != hash(end.Format(time.RFC3339Nano)) ||
		in.Date.IsZero() || in.Date.Year() < 1 || in.ExpiresAt.Year() > 9999 ||
		!in.Date.Equal(in.Date.Truncate(time.Microsecond)) || !in.ExpiresAt.Equal(in.Date.Add(SavingsExpiredMaxAge)) || len(in.Recipients) > MaxRecipients {
		return nil, ErrInvalid
	}
	for _, v := range []string{in.FullName, in.SavingsPlanName, in.Currency, in.StartDate, in.MonthlyCommittedAmount, in.Subject} {
		if len(v) > 512 || !safeHeader(v) {
			return nil, ErrInvalid
		}
	}
	if !utf8.ValidString(in.TextBody) || !utf8.ValidString(in.HTMLBody) || len(in.TextBody)+len(in.HTMLBody) > MaxWireBytes {
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
	addresses := make([]string, 0, len(in.Recipients))
	seen := map[string]bool{}
	for _, raw := range in.Recipients {
		a, err := address(raw)
		if err != nil {
			return nil, ErrInvalid
		}
		key := strings.ToLower(a.Address)
		if !seen[key] {
			addresses = append(addresses, a.Address)
			seen[key] = true
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
	in.CanonicalEndDate, in.Date, in.ExpiresAt = end, in.Date.UTC(), in.ExpiresAt.UTC()
	var from *mail.Address
	var err error
	if in.From != "" || state == Pending {
		from, err = address(in.From)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	event := hash(SavingsExpiredKind + "\x00" + in.ContractID + "\x00" + in.TermGeneration)
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
		pt, err := json.Marshal(savingsExpiredPayload{Kind: SavingsExpiredKind, Version: Version, EventKey: event, Input: in, SnapshotDigest: digest, Payload: f})
		if err != nil || len(pt) > 1499900 {
			return nil, ErrInvalid
		}
		out = append(out, prepared{kind: SavingsExpiredKind, version: Version, event: event, recipient: hash(ordinal), state: state, expiry: in.ExpiresAt, plaintext: pt})
	}
	return out, nil
}
