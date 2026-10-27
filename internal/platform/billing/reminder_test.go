package billing

import (
	"testing"
	"time"
)

func detail(days int, sent bool) ReminderNotificationDetail {
	d := ReminderNotificationDetail{DaysBefore: days}
	if sent {
		t := time.Unix(0, 0).UTC()
		d.SentAt = &t
	}
	return d
}

func TestReminderDecision(t *testing.T) {
	// details are stored DESC by daysBefore (scheduleReminders sorts reverse).
	t.Run("picks_tightest_open_window_skips_larger", func(t *testing.T) {
		details := []ReminderNotificationDetail{detail(30, false), detail(7, false), detail(1, false)}
		send, skip := reminderDecision(details, 6) // 6<=30, 6<=7, 6>1 → open: idx0,idx1; pick last=idx1(7); skip idx0(30)
		if send != 1 {
			t.Fatalf("sendIdx = %d, want 1 (the 7-day, tightest open)", send)
		}
		if len(skip) != 1 || skip[0] != 0 {
			t.Fatalf("skipIdxs = %v, want [0] (the 30-day, superseded)", skip)
		}
	})
	t.Run("single_open_when_close_to_expiry", func(t *testing.T) {
		details := []ReminderNotificationDetail{detail(30, true), detail(7, true), detail(1, false)}
		send, skip := reminderDecision(details, 0) // only idx2(1) unsent + open (0<=1)
		if send != 2 || len(skip) != 0 {
			t.Fatalf("send=%d skip=%v, want send=2 skip=[]", send, skip)
		}
	})
	t.Run("none_open_when_far_from_expiry", func(t *testing.T) {
		details := []ReminderNotificationDetail{detail(7, false), detail(1, false)}
		send, _ := reminderDecision(details, 20) // 20>7, 20>1 → none open
		if send != -1 {
			t.Fatalf("sendIdx = %d, want -1 (nothing due)", send)
		}
	})
	t.Run("past_due_still_sends", func(t *testing.T) {
		details := []ReminderNotificationDetail{detail(7, false)}
		send, _ := reminderDecision(details, -2) // -2<=7 → open
		if send != 0 {
			t.Fatalf("sendIdx = %d, want 0 (past-due still reminds)", send)
		}
	})
	t.Run("all_sent_none_open", func(t *testing.T) {
		details := []ReminderNotificationDetail{detail(30, true), detail(7, true)}
		send, _ := reminderDecision(details, 3)
		if send != -1 {
			t.Fatalf("sendIdx = %d, want -1 (all already sent)", send)
		}
	})
}

func TestDaysUntil(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		end  time.Time
		want int64
	}{
		{base.Add(6 * 24 * time.Hour), 6},
		{base.Add(6*24*time.Hour + 23*time.Hour), 6}, // 6.95 days truncates → 6
		{base, 0},
		{base.Add(-2*24*time.Hour - time.Hour), -2}, // -2.04 days truncates toward zero → -2
	}
	for _, c := range cases {
		if got := daysUntil(base, c.end); got != c.want {
			t.Errorf("daysUntil(%v) = %d, want %d", c.end.Sub(base), got, c.want)
		}
	}
}

func TestAllDetailsSent(t *testing.T) {
	if allDetailsSent([]ReminderNotificationDetail{detail(7, true), detail(1, false)}) {
		t.Error("one unsent → not all sent")
	}
	if !allDetailsSent([]ReminderNotificationDetail{detail(7, true), detail(1, true)}) {
		t.Error("all sent → true")
	}
}
