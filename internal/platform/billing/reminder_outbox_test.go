package billing

import (
	"testing"
	"time"
)

func TestReminderGenerationCanonicalExpiryAndProcessedMarkers(t *testing.T) {
	end := time.Date(2027, 1, 2, 0, 0, 0, 123456000, time.UTC)
	if reminderGeneration(end) != reminderGeneration(end.In(time.FixedZone("fixture", 7*3600))) {
		t.Fatal("same instant changed generation")
	}
	if reminderGeneration(end) == reminderGeneration(end.AddDate(0, 1, 0)) {
		t.Fatal("extension reused generation")
	}
	details := []ReminderNotificationDetail{{DaysBefore: 30, SentAt: &end}, {DaysBefore: 7, DeliveryQueuedAt: &end}, {DaysBefore: 1, SkippedAt: &end}}
	if i, _ := reminderDecision(details, 0); i != -1 || !allDetailsSent(details) {
		t.Fatal("processed/queued/skipped window reselected")
	}
	if details[1].SentAt != nil || details[2].SentAt != nil {
		t.Fatal("queue/skip invented SMTP acknowledgement")
	}
}
