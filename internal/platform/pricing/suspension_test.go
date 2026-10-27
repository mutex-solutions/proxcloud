package pricing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var suspNow = time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)

func limit(balance string, days int) SuspensionLimit {
	if balance == "" {
		return SuspensionLimit{Days: days}
	}
	return SuspensionLimit{Balance: dp(balance), Days: days}
}

func daysAgo(n int) time.Time { return suspNow.Add(-time.Duration(n) * 24 * time.Hour) }

func TestStartAtSuspensionLimit(t *testing.T) {
	susp := limit("-100", 30)
	t.Run("nil_notifications_uses_suspendedAt", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Type: SuspensionTypeBalance, SuspendedAt: &susp}
		if got := c.StartAtSuspensionLimit(); got == nil || !got.Balance.Equal(mustDec("-100")) {
			t.Errorf("got %+v, want suspendedAt", got)
		}
	})
	t.Run("balance_picks_max_balance", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Type: SuspensionTypeBalance, SuspendedAt: &susp,
			Notifications: []SuspensionLimit{limit("-10", 5), limit("0", 1), limit("-50", 10)}}
		if got := c.StartAtSuspensionLimit(); !got.Balance.Equal(mustDec("0")) { // max balance = 0
			t.Errorf("got %v, want 0", got.Balance)
		}
	})
	t.Run("due_date_picks_min_days", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Type: SuspensionTypeDueDate, SuspendedAt: &susp,
			Notifications: []SuspensionLimit{limit("", 7), limit("", 3), limit("", 14)}}
		if got := c.StartAtSuspensionLimit(); got.Days != 3 {
			t.Errorf("got %d, want 3", got.Days)
		}
	})
}

func TestIsEligibleForSuspension(t *testing.T) {
	t.Run("disabled_never_eligible", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Enabled: false, Type: SuspensionTypeBalance}
		if IsEligibleForSuspension(c, limit("0", 0), mustDec("-100"), nil, suspNow) {
			t.Error("disabled config must not be eligible")
		}
	})
	t.Run("balance_at_or_below_limit", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Enabled: true, Type: SuspensionTypeBalance}
		lim := limit("-50", 0)
		if !IsEligibleForSuspension(c, lim, mustDec("-50"), nil, suspNow) { // == limit → eligible (<=)
			t.Error("balance == limit must be eligible")
		}
		if !IsEligibleForSuspension(c, lim, mustDec("-60"), nil, suspNow) {
			t.Error("balance below limit must be eligible")
		}
		if IsEligibleForSuspension(c, lim, mustDec("-40"), nil, suspNow) {
			t.Error("balance above limit must NOT be eligible")
		}
	})
	t.Run("due_date_threshold", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Enabled: true, Type: SuspensionTypeDueDate}
		lim := limit("", 7)
		if !IsEligibleForSuspension(c, lim, decimal.Zero, []time.Time{daysAgo(8)}, suspNow) { // 8 >= 7
			t.Error("a bill 8 days overdue must be eligible (limit 7)")
		}
		if IsEligibleForSuspension(c, lim, decimal.Zero, []time.Time{daysAgo(3)}, suspNow) {
			t.Error("a bill 3 days overdue must NOT be eligible (limit 7)")
		}
		if IsEligibleForSuspension(c, lim, decimal.Zero, nil, suspNow) {
			t.Error("no due bills must NOT be eligible")
		}
	})
}

func TestListNotificationsToSend(t *testing.T) {
	sent := suspNow.Add(-time.Hour)
	t.Run("balance_filters_unsent_crossed", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Type: SuspensionTypeBalance}
		notifs := []SuspensionNotification{
			{SuspensionLimit: limit("0", 0)},                // 0 > -10 → send
			{SuspensionLimit: limit("-50", 0)},              // -50 > -10? no → skip
			{SuspensionLimit: limit("0", 0), SentAt: &sent}, // already sent → skip
		}
		got := ListNotificationsToSend(c, notifs, nil, mustDec("-10"), suspNow)
		if len(got) != 1 || !got[0].SuspensionLimit.Balance.Equal(mustDec("0")) {
			t.Errorf("got %d notifications, want 1 (the 0-limit unsent)", len(got))
		}
	})
	t.Run("due_date_filters_by_max_overdue", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Type: SuspensionTypeDueDate}
		notifs := []SuspensionNotification{
			{SuspensionLimit: limit("", 3)},                // 3 < 10 → send
			{SuspensionLimit: limit("", 10)},               // 10 < 10? no → skip
			{SuspensionLimit: limit("", 3), SentAt: &sent}, // sent → skip
		}
		got := ListNotificationsToSend(c, notifs, []time.Time{daysAgo(10), daysAgo(2)}, decimal.Zero, suspNow) // max overdue = 10
		if len(got) != 1 || got[0].SuspensionLimit.Days != 3 {
			t.Errorf("got %d notifications, want 1 (days=3)", len(got))
		}
	})
	t.Run("due_date_no_due_bills_empty", func(t *testing.T) {
		c := &BillingAutomaticSuspensionConfig{Type: SuspensionTypeDueDate}
		notifs := []SuspensionNotification{{SuspensionLimit: limit("", 1)}}
		if got := ListNotificationsToSend(c, notifs, nil, decimal.Zero, suspNow); len(got) != 0 {
			t.Errorf("no due bills must yield no notifications, got %d", len(got))
		}
	})
}
