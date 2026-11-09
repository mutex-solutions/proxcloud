package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPrepareTemplateNeverResolvesOrSends(t *testing.T) {
	vars := map[string]any{"name": "fixture"}
	s := NewService(func(_ context.Context, _ string, v map[string]any) (string, string, bool, error) {
		if v["businessName"] != "Fixture Ltd" {
			t.Fatal("missing branding")
		}
		return "Reminder", "<p>fixture</p>", true, nil
	}, func(context.Context) (Mailer, string) {
		t.Fatal("preparation resolved SMTP")
		return nil, ""
	}, "Fixture Ltd", nil)
	m, ok, err := s.PrepareTemplate(t.Context(), "savings_contract_expiry_reminder", vars)
	if err != nil || !ok || m.Subject != "Reminder" || !strings.Contains(m.HTMLBody, "<p>fixture</p>") {
		t.Fatalf("draft=%+v ok=%v err=%v", m, ok, err)
	}
	if _, ok := vars["businessName"]; ok {
		t.Fatal("preparation mutated caller vars")
	}
	s.render = func(context.Context, string, map[string]any) (string, string, bool, error) {
		return "", "", false, errors.New("private-token-canary")
	}
	if _, _, err := s.PrepareTemplate(t.Context(), "fixed", nil); err != ErrPreparation || strings.Contains(err.Error(), "canary") {
		t.Fatal("unsafe preparation error")
	}
}
