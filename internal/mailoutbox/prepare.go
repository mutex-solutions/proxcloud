package mailoutbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PrepareReminder freezes validated per-recipient envelopes for the fixed DTO.
// It performs no persistence or network I/O; EnqueueTx repeats validation and
// seals these internally. There is intentionally no HTTP route to this API.
func PrepareReminder(r Reminder) ([]Envelope, error) {
	ps, err := prepare(r)
	if err != nil {
		return nil, err
	}
	out := make([]Envelope, 0, len(ps))
	for _, p := range ps {
		var v struct {
			Payload frozen `json:"payload"`
		}
		if json.Unmarshal(p.plaintext, &v) != nil {
			return nil, ErrInvalid
		}
		if len(v.Payload.Wire) == 0 {
			continue
		}
		out = append(out, Envelope{from: v.Payload.From, to: v.Payload.To, wire: v.Payload.Wire})
	}
	return out, nil
}

var opaque = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var dotAtom = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*$")
var dnsDomain = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

type frozen struct {
	From        string    `json:"from"`
	To          string    `json:"to"`
	Wire        []byte    `json:"wire"`
	EndDate     time.Time `json:"endDate"`
	Disposition string    `json:"disposition"`
}
type prepared struct {
	kind                    string
	version                 int
	event, recipient, state string
	expiry                  time.Time
	plaintext               []byte
}

func encodeBody(b *strings.Builder, raw []byte) {
	v := base64.StdEncoding.EncodeToString(raw)
	for len(v) > 76 {
		b.WriteString(v[:76] + "\r\n")
		v = v[76:]
	}
	b.WriteString(v + "\r\n")
}
func hash(v string) string { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }
func aad(id string) []byte { return []byte(id + "\x00" + Kind + "\x00" + strconv.Itoa(Version)) }

func safeHeader(v string) bool {
	if !utf8.ValidString(v) {
		return false
	}
	for _, c := range v {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func address(v string) (*mail.Address, error) {
	if len(v) > 320 || !safeHeader(v) {
		return nil, ErrInvalid
	}
	a, err := mail.ParseAddress(strings.TrimSpace(v))
	if err != nil {
		return nil, ErrInvalid
	}
	// SMTPUTF8 is deliberately not part of the first envelope contract.
	for _, c := range a.Address {
		if c < 33 || c > 126 {
			return nil, ErrInvalid
		}
	}
	i := strings.LastIndex(a.Address, "@")
	if i <= 0 || i > 64 || i == len(a.Address)-1 || !dotAtom.MatchString(a.Address[:i]) || !dnsDomain.MatchString(a.Address[i+1:]) || strings.Contains(a.Address[i+1:], "..") {
		return nil, ErrInvalid
	}
	a.Address = a.Address[:i+1] + strings.ToLower(a.Address[i+1:])
	return a, nil
}

// prepare accepts only the fixed savings-reminder/v1 contract. Canonical content
// includes the recipient SET, so re-enqueue cannot silently add/remove recipients.
func prepare(r Reminder) ([]prepared, error) {
	if !opaque.MatchString(r.ContractID) || !opaque.MatchString(r.ExpiryGeneration) || r.WindowDays < 0 || r.WindowDays > 366 || r.Date.IsZero() || r.EndDate.IsZero() || len(r.Recipients) > MaxRecipients {
		return nil, ErrInvalid
	}
	switch r.Disposition {
	case "", Suppressed, Blocked, NoRecipient:
	default:
		return nil, ErrInvalid
	}
	if r.Disposition == NoRecipient && len(r.Recipients) != 0 {
		return nil, ErrInvalid
	}
	if len(r.TextBody)+len(r.HTMLBody) > MaxWireBytes || len(r.Subject) > 512 || !safeHeader(r.Subject) || !utf8.ValidString(r.TextBody) || !utf8.ValidString(r.HTMLBody) {
		return nil, ErrInvalid
	}
	r.Date = r.Date.UTC().Truncate(time.Second)
	r.EndDate = r.EndDate.UTC().Truncate(time.Microsecond)
	expiry := r.EndDate
	if ageEnd := r.Date.Add(MaxAge); ageEnd.Before(expiry) {
		expiry = ageEnd
	}
	event := hash(fmt.Sprintf("%s\x00%s\x00%s\x00%d", Kind, r.ContractID, r.ExpiryGeneration, r.WindowDays))
	state := r.Disposition
	if state == "" {
		state = Pending
	}
	recipients := append([]Recipient(nil), r.Recipients...)
	sort.Slice(recipients, func(i, j int) bool { return recipients[i].Key < recipients[j].Key })
	seen := map[string]bool{}
	for i := range recipients {
		if !opaque.MatchString(recipients[i].Key) || (i > 0 && recipients[i-1].Key == recipients[i].Key) {
			return nil, ErrInvalid
		}
		a, err := address(recipients[i].Address)
		if err != nil {
			return nil, ErrInvalid
		}
		if seen[strings.ToLower(a.Address)] {
			return nil, ErrInvalid
		}
		seen[strings.ToLower(a.Address)] = true
		recipients[i].Address = a.Address
	}
	if len(recipients) == 0 {
		state = NoRecipient
		recipients = []Recipient{{Key: "no-recipient"}}
	}
	var from *mail.Address
	if state == Pending || r.From != "" {
		var err error
		from, err = address(r.From)
		if err != nil {
			return nil, ErrInvalid
		}
	}
	// The recipient manifest is authenticated, sealed, and fingerprinted, never
	// stored in metadata. Its inclusion forbids changes to an immutable event.
	manifest, _ := json.Marshal(recipients)
	// Non-delivery rows may have no renderable From/recipient. Still fingerprint
	// every immutable draft input; changing a blocked/suppressed decision must
	// conflict rather than silently forgetting its different body/date/subject.
	draft, _ := json.Marshal(struct {
		From, Subject, Text, HTML string
		Date                      time.Time
	}{r.From, r.Subject, r.TextBody, r.HTMLBody, r.Date})
	draftDigest := hash(string(draft))
	out := make([]prepared, 0, len(recipients))
	for _, to := range recipients {
		f := frozen{To: to.Address, EndDate: r.EndDate, Disposition: state}
		if from != nil {
			f.From = from.Address
		}
		if state == Pending || (from != nil && to.Address != "") {
			id := hash(event + "\x00" + to.Key)
			var b strings.Builder
			fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@mail.proxcloud.invalid>\r\nMIME-Version: 1.0\r\n", from.String(), (&mail.Address{Address: to.Address}).String(), strings.ReplaceAll(mime.QEncoding.Encode("UTF-8", r.Subject), "?= =?", "?=\r\n =?"), r.Date.Format(time.RFC1123Z), id)
			write := func(contentType, body string) {
				fmt.Fprintf(&b, "Content-Type: %s; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", contentType)
				encodeBody(&b, []byte(body))
			}
			if r.TextBody != "" && r.HTMLBody != "" {
				boundary := "pc_" + id
				fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n--%s\r\n", boundary, boundary)
				write("text/plain", r.TextBody)
				fmt.Fprintf(&b, "--%s\r\n", boundary)
				write("text/html", r.HTMLBody)
				fmt.Fprintf(&b, "--%s--\r\n", boundary)
			} else if r.HTMLBody != "" {
				write("text/html", r.HTMLBody)
			} else {
				write("text/plain", r.TextBody)
			}
			f.Wire = []byte(b.String())
			if len(f.Wire) > MaxWireBytes {
				return nil, ErrInvalid
			}
		}
		canonical, _ := json.Marshal(struct {
			Payload     frozen          `json:"payload"`
			Manifest    json.RawMessage `json:"manifest"`
			DraftDigest string          `json:"draftDigest"`
		}{f, manifest, draftDigest})
		out = append(out, prepared{kind: Kind, version: Version, event: event, recipient: hash(to.Key), state: state, expiry: expiry, plaintext: canonical})
	}
	return out, nil
}
