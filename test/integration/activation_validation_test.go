//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// The validation-APPROVED tail (admin POST /billing-profile/validations/{id}/status/APPROVED):
// Activate(SourceValidation) flips a NEW profile ACTIVE with the VALIDATION constraint stamped
// (flow==nil ⇒ the filling-billing-details constraint passes), and NotifyValidation sends
// billing_profile_validated with the loginUrl var.
func TestActivation_ValidationApproved(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	bpID := pgdoc.NewID()
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "status": "NEW", "email": "v@demo", "currency": "USD",
	}); err != nil {
		t.Fatalf("seed bp: %v", err)
	}
	svc := billing.NewActivationService(repo, nil, nil)
	notes := &captureNotifier{}
	svc.SetNotifier(notes)
	svc.SetLoginURL("https://ui.example")

	bp, _ := repo.FindByID(ctx, bpID)
	if err := svc.Activate(ctx, bp, billing.SourceValidation); err != nil {
		t.Fatalf("activate: %v", err)
	}
	svc.NotifyValidation(ctx, bp)

	got, _ := repo.FindByID(ctx, bpID)
	if got.Status != billing.StatusActive {
		t.Fatalf("status: want ACTIVE, got %s", got.Status)
	}
	if len(got.ActivationConstraints) != 1 || got.ActivationConstraints[0].Source != billing.SourceValidation {
		t.Fatalf("constraint: want VALIDATION, got %+v", got.ActivationConstraints)
	}
	if len(notes.calls) != 1 || notes.calls[0].key != "billing_profile_validated" {
		t.Fatalf("mail: want billing_profile_validated, got %+v", notes.calls)
	}
	if notes.calls[0].vars["loginUrl"] != "https://ui.example" {
		t.Fatalf("loginUrl var missing: %v", notes.calls[0].vars)
	}
}
