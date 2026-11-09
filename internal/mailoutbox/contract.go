// Package mailoutbox is the dormant, primary-PostgreSQL reminder-mail substrate.
// There are deliberately no business callers or resend APIs in PR A.
package mailoutbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	Kind          = "savings-reminder/v1"
	Version       = 1
	MaxRecipients = 20
	MaxWireBytes  = 1 << 20
	MaxAttempts   = 8
	MaxAge        = 30 * 24 * time.Hour
	Pending       = "pending"
	Claimed       = "claimed"
	Dispatching   = "DISPATCHING"
	Accepted      = "accepted"
	Unknown       = "UNKNOWN"
	Dead          = "dead"
	Blocked       = "blocked_config"
	Suppressed    = "suppressed"
	NoRecipient   = "no_recipient"
	Expired       = "expired"
)

var (
	ErrInvalid   = errors.New("mail outbox: invalid reminder contract")
	ErrConflict  = errors.New("mail outbox: immutable event conflict")
	ErrLeaseLost = errors.New("mail outbox: lease lost")
	ErrReceipt   = errors.New("mail outbox: invalid or superseded receipt")
	ErrPayload   = errors.New("mail outbox: sealed payload unavailable")
	ErrDisabled  = errors.New("mail outbox: dispatch disabled")
)

// Reminder is a future billing-owned preparation DTO, NOT an HTTP/job payload.
// Caller renders in memory then passes this within the SAME owned pgdoc.WithTx.
// EndDate is the frozen contract expiry; Generation changes on an expiry change.
// Date is a decision timestamp frozen by the caller, never a per-retry wall clock.
// No arbitrary kind, transport, attachments, job functions or SMTP credentials.
type Reminder struct {
	ContractID       string
	ExpiryGeneration string
	WindowDays       int
	EndDate          time.Time
	Date             time.Time
	Recipients       []Recipient
	From             string
	Subject          string
	TextBody         string
	HTMLBody         string
	// Empty means pending. Only explicit non-delivery decisions below are allowed.
	Disposition string
}
type Recipient struct{ Key, Address string }

func (Reminder) LogValue() slog.Value { return slog.StringValue("[reminder redacted]") }
func (Reminder) String() string       { return "[reminder redacted]" }

// Envelope is immutable outside this package; Wire returns a copy. One envelope
// has exactly one RCPT. Stable Message-ID is NOT an SMTP exactly-once guarantee.
type Envelope struct {
	from, to string
	wire     []byte
}

func (e Envelope) From() string       { return e.from }
func (e Envelope) To() string         { return e.to }
func (e Envelope) Wire() []byte       { return append([]byte(nil), e.wire...) }
func (Envelope) LogValue() slog.Value { return slog.StringValue("[mail envelope redacted]") }
func (Envelope) String() string       { return "[mail envelope redacted]" }

type Delivery string

const (
	RelayAccepted Delivery = "accepted"
	NotAccepted   Delivery = "known_not_accepted"
	Indeterminate Delivery = "unknown"
)

type Stage string

const (
	StageConfig   Stage = "config"
	StageConnect  Stage = "connect"
	StageTLS      Stage = "tls"
	StageAuth     Stage = "auth"
	StageEnvelope Stage = "envelope"
	StageData     Stage = "data"
	StageBody     Stage = "body"
	StageEndData  Stage = "end_data"
	StageReply    Stage = "reply"
	StageLegacy   Stage = "legacy"
)

type SafeClass string

const (
	ClassOK        SafeClass = "relay_accepted"
	ClassNetwork   SafeClass = "network"
	ClassTemporary SafeClass = "smtp_temporary"
	ClassPermanent SafeClass = "smtp_permanent"
	ClassConfig    SafeClass = "configuration"
	ClassTLS       SafeClass = "tls_required"
	ClassCancelled SafeClass = "cancelled"
	ClassUncertain SafeClass = "receipt_lost"
	ClassPayload   SafeClass = "sealed_payload"
	ClassLease     SafeClass = "dispatch_lease_expired"
	ClassLegacy    SafeClass = "legacy_untyped"
)

// Receipt NEVER includes raw SMTP replies or errors (they may echo PII/tokens).
// Accepted means relay acceptance, not inbox delivery. Retry only known-unsent.
type Receipt struct {
	Delivery  Delivery  `json:"delivery"`
	Stage     Stage     `json:"stage"`
	Class     SafeClass `json:"class"`
	Permanent bool      `json:"permanent"`
}

func (r Receipt) valid() bool {
	switch r.Stage {
	case StageConfig, StageConnect, StageTLS, StageAuth, StageEnvelope, StageData, StageBody, StageEndData, StageReply, StageLegacy:
	default:
		return false
	}
	switch r.Class {
	case ClassOK, ClassNetwork, ClassTemporary, ClassPermanent, ClassConfig, ClassTLS, ClassCancelled, ClassUncertain, ClassPayload, ClassLease, ClassLegacy:
	default:
		return false
	}
	switch r.Delivery {
	case RelayAccepted:
		return r.Stage == StageReply && r.Class == ClassOK && !r.Permanent
	case NotAccepted:
		if r.Class == ClassOK || r.Class == ClassUncertain || r.Class == ClassLease || r.Class == ClassLegacy {
			return false
		}
		// End-of-DATA I/O failure is ALWAYS unknown; only an explicit 4xx/5xx
		// negative final reply proves non-acceptance at that boundary.
		if r.Stage == StageEndData || (r.Stage == StageReply && r.Class != ClassTemporary && r.Class != ClassPermanent) {
			return false
		}
		return true
	case Indeterminate:
		return !r.Permanent && (r.Class == ClassUncertain || r.Class == ClassLease || r.Class == ClassLegacy)
	}
	return false
}

// Transport is internal trusted infrastructure, not a selectable job function.
// Implementations must enforce hard context-aware socket deadlines.
type Transport interface {
	Deliver(context.Context, Envelope) Receipt
}

type QueueResult struct {
	IDs   []string
	State string
}

// Claim contains opaque ownership metadata only, safe for logging/serialization.
type Claim struct {
	ID        string `json:"id"`
	Owner     string `json:"-"`
	Epoch     int64  `json:"epoch"`
	AttemptID string `json:"attemptId,omitempty"`
	Attempts  int    `json:"attempts"`
}
type Status struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}
