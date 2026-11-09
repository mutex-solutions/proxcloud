//go:build integration

package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"github.com/mutex-solutions/proxcloud/internal/platform/message"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type validationTemplateCounter struct {
	repo  *message.Repo
	calls atomic.Int64
}

func (c *validationTemplateCounter) ByKey(ctx context.Context, key string) (*message.MessageTemplate, error) {
	if key != "billing_profile_validation_approved" {
		panic("wrong template")
	}
	c.calls.Add(1)
	return c.repo.ByKey(ctx, key)
}
func validationCompositionHandler(db *pgdoc.DB) *admin.Handler {
	return admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, billing.NewRepo(db), nil, nil, nil, nil, "", "fixture", "")
}
func validationCompositionRoute(t *testing.T, h *admin.Handler, id, status string) int {
	t.Helper()
	r := chi.NewRouter()
	h.Routes(r)
	req := httptest.NewRequest("POST", "/admin/billing-profile/validations/"+id+"/status/"+status, nil)
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{SigV4KeyID: "fixture"}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}
func TestValidationCompositionActualOneTemplateConcurrentAndExplicitDecisions(t *testing.T) {
	for _, mode := range []string{"pending", "suppressed", "blocked", "no-recipient", "missing-template", "render-error", "malformed-recipient", "missing-profile"} {
		t.Run(mode, func(t *testing.T) {
			// The shared helper makes a new disposable database for every body. Both
			// independent pools below use only our explicitly provided loopback fixture.
			db := inviteCompositionDB(t)
			peer := invitePeer(t, db, true)
			ctx := t.Context()
			if e := prepareValidationDependencies(ctx, db); e != nil {
				t.Fatal(e)
			}
			s := mailoutbox.New(db, inviteTestKeys(t))
			if e := s.Ensure(ctx); e != nil {
				t.Fatal(e)
			}
			msg := message.NewRepo(db)
			tmpl := message.MessageTemplate{Key: "billing_profile_validation_approved", MessageTitle: "Validation approved", MessageBody: "<p>{{fullName}} validation approved only. {{loginUrl}}</p>"}
			if mode == "suppressed" {
				tmpl.Disabled = true
			}
			if mode != "missing-template" {
				if e := msg.Create(ctx, &tmpl); e != nil {
					t.Fatal(e)
				}
			}
			counter := &validationTemplateCounter{repo: msg}
			profile, id := pgdoc.NewID(), pgdoc.NewID()
			email := "MixedCaseLocal@EXAMPLE.invalid"
			contacts := []pgdoc.M{{"email": "other@example.invalid"}}
			if mode == "no-recipient" {
				email = ""
				contacts = nil
			}
			if mode == "malformed-recipient" {
				contacts = append(contacts, pgdoc.M{"email": "malformed"})
			}
			if mode != "missing-profile" {
				if _, e := db.C("billingProfile").InsertOne(ctx, pgdoc.M{"_id": profile, "status": "NEW", "email": email, "contacts": contacts, "firstName": "PRIVATE_NAME"}); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := db.C("billingProfileValidation").InsertOne(ctx, pgdoc.M{"_id": id, "status": "PENDING", "billingProfileId": profile, "document": "PRIVATE_KYC_NOT_A_VAR"}); e != nil {
				t.Fatal(e)
			}
			service := mail.NewService(func(context.Context, string, map[string]any) (string, string, bool, error) {
				panic("second lookup forbidden")
			}, func(context.Context) (mail.Mailer, string) { panic("SMTP forbidden") }, "Fixture", nil)
			config := func(context.Context) (mail.Config, error) { return mail.Config{From: "sender@example.invalid"}, nil }
			if mode == "blocked" {
				config = func(context.Context) (mail.Config, error) { return mail.Config{}, errors.New("PRIVATE_CONFIGURATION") }
			}
			if mode == "render-error" {
				service = nil
			}
			h := validationCompositionHandler(db)
			a := validationOutboxAdapter{store: s, service: service, message: counter, config: config, loginURL: "https://console.example.invalid/login"}
			h.SetValidationOutbox(a)
			h.SetValidationCanceler(s)
			fail := mode == "missing-template" || mode == "render-error" || mode == "malformed-recipient" || mode == "missing-profile"
			if fail {
				code := validationCompositionRoute(t, h, id, "APPROVED")
				if code != 500 && code != 404 {
					t.Fatal("failure not propagated", code)
				}
				var status string
				var n int
				if e := db.Pool.QueryRow(ctx, `SELECT doc->>'status',(SELECT count(*) FROM mail_outbox) FROM "billingProfileValidation" WHERE id=$1`, id).Scan(&status, &n); e != nil || status != "PENDING" || n != 0 {
					t.Fatal("failed actual adapter committed", e)
				}
				return
			}
			// Unwired activation answers 501 AFTER committing approval. No real billing
			// service callback is called by these composition tests (the admin test uses
			// its private pure-fake seam to prove failed/attempt-only activation behavior).
			h2 := validationCompositionHandler(peer)
			a2 := a
			a2.store = mailoutbox.New(peer, inviteTestKeys(t))
			h2.SetValidationOutbox(a2)
			h2.SetValidationCanceler(mailoutbox.New(peer, nil))
			var wg sync.WaitGroup
			codes := make(chan int, 2)
			for _, handler := range []*admin.Handler{h, h2} {
				wg.Go(func() { codes <- validationCompositionRoute(t, handler, id, "APPROVED") })
			}
			wg.Wait()
			close(codes)
			for code := range codes {
				if code != 501 {
					t.Fatal("tail result changed", code)
				}
			}
			if counter.calls.Load() != 1 {
				t.Fatal("not exactly one fixed snapshot", counter.calls.Load())
			}
			if code := validationCompositionRoute(t, h, id, "APPROVED"); code != 501 || counter.calls.Load() != 1 {
				t.Fatal("duplicate reread template")
			}
			want, n := mailoutbox.Pending, 2
			if mode == "suppressed" {
				want = mailoutbox.Suppressed
			}
			if mode == "blocked" {
				want = mailoutbox.Blocked
			}
			if mode == "no-recipient" {
				want = mailoutbox.NoRecipient
				n = 1
			}
			var count int
			var state string
			if e := db.Pool.QueryRow(ctx, `SELECT count(*),min(state) FROM mail_outbox`).Scan(&count, &state); e != nil || count != n || state != want {
				t.Fatal("actual disposition", count, state, e)
			}
			if mode == "pending" {
				cl, e := s.Claim(ctx, time.Minute)
				if e != nil || cl == nil {
					t.Fatal(e)
				}
				env, e := s.BeginDispatch(ctx, cl)
				if e != nil {
					t.Fatal(e)
				}
				if strings.Contains(string(env.Wire()), "PRIVATE_KYC_NOT_A_VAR") {
					t.Fatal("KYC escaped")
				}
			}
		})
	}
}
