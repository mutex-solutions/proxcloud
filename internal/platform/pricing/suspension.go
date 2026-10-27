package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// Dunning/suspension decision logic. This slice implements the PURE decision
// predicates — eligibility, the notification filter, and the start-limit pick — as
// golden-testable functions. The orchestration (SuspensionProcess repo, mail, audit,
// the activation suspend/resume side-effects, executeBillingProfile/reviewBillingProfile
// flow) is deferred to the persistence/cron slice. The suspension-config SELECTION
// (profile.suspensionConfiguration when overwriteSuspension, else the global one) also
// lives with the billing-profile wiring — the resolved config is an input here.

// SuspensionType (BillingAutomaticSuspensionConfig.SuspensionType).
const (
	SuspensionTypeBalance = "BALANCE"
	SuspensionTypeDueDate = "DUE_DATE"
)

// SuspensionLimit: a balance and/or day threshold.
type SuspensionLimit struct {
	Balance *decimal.Decimal `json:"balance,omitempty"`
	Days    int              `json:"days"`
}

// BillingAutomaticSuspensionConfig: the automatic-suspension policy.
type BillingAutomaticSuspensionConfig struct {
	Enabled       bool              `json:"enabled"`
	Type          string            `json:"type,omitempty"`
	SuspendedAt   *SuspensionLimit  `json:"suspendedAt,omitempty"`
	Notifications []SuspensionLimit `json:"notifications,omitempty"`
}

// SuspensionNotification: a per-limit dunning notification slot on a
// SuspensionProcess; SentAt nil means "not yet sent".
type SuspensionNotification struct {
	SuspensionLimit SuspensionLimit  `json:"suspensionLimit"`
	Balance         *decimal.Decimal `json:"balance,omitempty"`
	Currency        string           `json:"currency,omitempty"`
	SentAt          *time.Time       `json:"sentAt,omitempty"`
	CreatedAt       *time.Time       `json:"createdAt,omitempty"`
}

// StartAtSuspensionLimit picks the starting suspension limit:
// nil notifications → suspendedAt; else BALANCE picks the max-balance notification (or
// suspendedAt if empty), DUE_DATE picks the min-days notification (or suspendedAt).
func (c *BillingAutomaticSuspensionConfig) StartAtSuspensionLimit() *SuspensionLimit {
	if c.Notifications == nil {
		return c.SuspendedAt
	}
	switch c.Type {
	case SuspensionTypeBalance:
		// pick max-by-balance; a null balance would break the comparison, so skip nil-balance
		// limits rather than return one (which would later nil-deref
		// in IsEligibleForSuspension).
		var best *SuspensionLimit
		for i := range c.Notifications {
			n := &c.Notifications[i]
			if n.Balance == nil {
				continue
			}
			if best == nil || n.Balance.Cmp(*best.Balance) > 0 {
				best = n
			}
		}
		if best == nil {
			return c.SuspendedAt
		}
		return best
	case SuspensionTypeDueDate:
		var best *SuspensionLimit
		for i := range c.Notifications {
			n := &c.Notifications[i]
			if best == nil || n.Days < best.Days {
				best = n
			}
		}
		if best == nil {
			return c.SuspendedAt
		}
		return best
	default:
		return c.SuspendedAt
	}
}

// IsEligibleForSuspension reports whether the profile is eligible for suspension: when the
// config is enabled, BALANCE checks balance ≤ limit.balance; DUE_DATE checks whether
// any due bill is at least limit.days past its dueAt. `dueAts` are the due bills'
// dueAt instants (the repo query is deferred).
func IsEligibleForSuspension(config *BillingAutomaticSuspensionConfig, limit SuspensionLimit, balance decimal.Decimal, dueAts []time.Time, now time.Time) bool {
	if config == nil || !config.Enabled {
		return false
	}
	switch config.Type {
	case SuspensionTypeBalance:
		if limit.Balance == nil {
			return false // a BALANCE limit with no balance threshold can't be crossed
		}
		return balance.Cmp(*limit.Balance) <= 0
	case SuspensionTypeDueDate:
		for _, due := range dueAts {
			if daysBetween(due, now) >= int64(limit.Days) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// ListNotificationsToSend returns the
// not-yet-sent notifications whose limit has been crossed — BALANCE: limit.balance >
// currentBalance; DUE_DATE: limit.days < the maximum days-overdue across due bills
// (none due → empty).
func ListNotificationsToSend(config *BillingAutomaticSuspensionConfig, notifications []SuspensionNotification, dueAts []time.Time, currentBalance decimal.Decimal, now time.Time) []SuspensionNotification {
	out := []SuspensionNotification{}
	switch config.Type {
	case SuspensionTypeBalance:
		for _, n := range notifications {
			if n.SentAt == nil && n.SuspensionLimit.Balance != nil && n.SuspensionLimit.Balance.Cmp(currentBalance) > 0 {
				out = append(out, n)
			}
		}
	case SuspensionTypeDueDate:
		if len(dueAts) == 0 {
			return out
		}
		maxDays := int64(-1 << 62)
		for _, due := range dueAts {
			if d := daysBetween(due, now); d > maxDays {
				maxDays = d
			}
		}
		for _, n := range notifications {
			if n.SentAt == nil && int64(n.SuspensionLimit.Days) < maxDays {
				out = append(out, n)
			}
		}
	}
	return out
}

// MarkNotificationsToSend sets SentAt=now + Balance=currentBalance on every not-yet-sent notification
// whose limit has been crossed (same predicate as ListNotificationsToSend, but mutates the slice in
// place so the SuspensionProcess can be persisted (setSentAt/
// setBalance on each). Returns the number marked.
func MarkNotificationsToSend(config *BillingAutomaticSuspensionConfig, notifications []SuspensionNotification, dueAts []time.Time, currentBalance decimal.Decimal, now time.Time) int {
	count := 0
	mark := func(i int) {
		t, b := now, currentBalance
		notifications[i].SentAt = &t
		notifications[i].Balance = &b
		count++
	}
	switch config.Type {
	case SuspensionTypeBalance:
		for i := range notifications {
			n := &notifications[i]
			if n.SentAt == nil && n.SuspensionLimit.Balance != nil && n.SuspensionLimit.Balance.Cmp(currentBalance) > 0 {
				mark(i)
			}
		}
	case SuspensionTypeDueDate:
		if len(dueAts) == 0 {
			return 0
		}
		maxDays := int64(-1 << 62)
		for _, due := range dueAts {
			if d := daysBetween(due, now); d > maxDays {
				maxDays = d
			}
		}
		for i := range notifications {
			n := &notifications[i]
			if n.SentAt == nil && int64(n.SuspensionLimit.Days) < maxDays {
				mark(i)
			}
		}
	}
	return count
}

// daysBetween returns the whole days between from and to, truncated
// toward zero.
func daysBetween(from, to time.Time) int64 {
	return int64(to.Sub(from) / (24 * time.Hour))
}
