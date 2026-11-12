//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// TestProjectInitSaysWhy (#216): POST /project/{id}/init answered 200 {} whether or not it brought the project up. Now an
// open that leaves the project as it is answers 409 with errors.details {status, reason[, billingProfileStatus]}, one
// reason per cause, and the status stays; an open that works keeps its 200 {}.
func TestProjectInitSaysWhy(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	mustInsert(t, f.db, "organization", pgdoc.M{"_id": "o-nobp", "name": "no profile"})
	mustInsert(t, f.db, "organization_members", pgdoc.M{"organizationId": "o-nobp", "sub": suspendedSub, "roles": []string{"OWNER"}})

	type details struct {
		Status               string `json:"status"`
		Reason               string `json:"reason"`
		BillingProfileStatus string `json:"billingProfileStatus"`
	}
	cases := []struct {
		name          string
		doc           pgdoc.M
		reason        string // "" = 200
		profileStatus string
		want          string // stored status after the open
	}{
		{"enabled: provisioned, nothing to say", pgdoc.M{"status": project.StatusEnabled}, "", "", project.StatusEnabled},
		{"disabled with nothing holding it: comes up", pgdoc.M{"status": project.StatusDisabled}, "", "", project.StatusEnabled},
		{"new customer: profile NEW", pgdoc.M{"status": project.StatusDisabled, "organizationId": "o2", "services": []any{}}, project.ReasonBillingProfileInactive, billing.StatusNew, project.StatusDisabled},
		{"own profile NEW", pgdoc.M{"status": project.StatusDisabled, "billingProfileId": "bp-new-o1"}, project.ReasonBillingProfileInactive, billing.StatusNew, project.StatusDisabled},
		{"profile SUSPENDED", pgdoc.M{"status": project.StatusDisabled, "organizationId": "o3"}, project.ReasonBillingProfileSuspended, billing.StatusSuspended, project.StatusDisabled},
		{"no paying profile", pgdoc.M{"status": project.StatusDisabled, "organizationId": "o-nobp", "services": []any{}}, project.ReasonBillingProfileMissing, "", project.StatusDisabled},
		{"operator's disable under an ACTIVE profile", pgdoc.M{"status": project.StatusDisabled, "disabledByOperator": true}, project.ReasonDisabledByOperator, "", project.StatusDisabled},
		{"scheduled for deletion", pgdoc.M{"status": project.StatusScheduledForDeletion}, project.ReasonScheduledForDeletion, "", project.StatusScheduledForDeletion},
		{"deletion in progress", pgdoc.M{"status": project.StatusDeleteInProgress}, project.ReasonDeletionInProgress, "", project.StatusDeleteInProgress},
		{"deleted", pgdoc.M{"status": project.StatusDeleted}, project.ReasonDeleted, "", project.StatusDeleted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.seed(t, c.doc)
			code, body := f.do(http.MethodPost, "/project/p1/init", "")
			if c.reason == "" {
				if code != http.StatusOK || body != "{}\n" {
					t.Fatalf("%d %q, want 200 {}", code, body)
				}
			} else {
				var env struct {
					Errors struct {
						Code    int     `json:"code"`
						Message string  `json:"message"`
						Details details `json:"details"`
					} `json:"errors"`
				}
				if err := json.Unmarshal([]byte(body), &env); err != nil {
					t.Fatalf("%s: %v", body, err)
				}
				want := details{Status: c.want, Reason: c.reason, BillingProfileStatus: c.profileStatus}
				if code != http.StatusConflict || env.Errors.Code != http.StatusConflict || env.Errors.Message == "" || env.Errors.Details != want {
					t.Fatalf("%d %s, want 409 with %+v", code, body, want)
				}
			}
			if got := f.stored(t).Status; got != c.want {
				t.Fatalf("stored %s, want %s", got, c.want)
			}
		})
	}

	// Billing not configured: nothing on the billing side holds a project, so a DISABLED one comes up even under a NEW
	// profile (as before).
	t.Run("billing not configured", func(t *testing.T) {
		if _, err := f.db.C("billingConfiguration").DeleteMany(ctx, pgdoc.M{}); err != nil {
			t.Fatal(err)
		}
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "organizationId": "o2"})
		if code, body := f.do(http.MethodPost, "/project/p1/init", ""); code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		if got := f.stored(t).Status; got != project.StatusEnabled {
			t.Fatalf("stored %s", got)
		}
	})
}
