package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type mailStatusFake struct{ calls int }

func (f *mailStatusFake) Overview(context.Context) (mailoutbox.Overview, error) {
	f.calls++
	return mailoutbox.Overview{Source: "primary", Counts: map[string]int64{mailoutbox.Unknown: 1}}, nil
}
func (f *mailStatusFake) GetStatus(context.Context, string) (*mailoutbox.Status, error) {
	f.calls++
	return &mailoutbox.Status{ID: "12d5034d-ce98-4456-9f67-48057277cddd", State: mailoutbox.Unknown, Reason: string(mailoutbox.ClassUncertain)}, nil
}
func TestMailOutboxReadOnlyRBACAndOpaqueStatus(t *testing.T) {
	f := &mailStatusFake{}
	h := &Handler{adminIssuer: "operator-realm", mailOutbox: f}
	router := chi.NewRouter()
	h.Routes(router)
	for _, path := range []string{"/admin/mail-outbox", "/admin/mail-outbox/12d5034d-ce98-4456-9f67-48057277cddd"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}))
		router.ServeHTTP(w, req)
		if w.Code != 403 || f.calls != 0 {
			t.Fatal("unauthorized status read")
		}
		w = httptest.NewRecorder()
		req = httptest.NewRequest("GET", path, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"}))
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, pii := range []string{"subject", "recipient", "password", "sealed", "fingerprint", "attemptId"} {
			if strings.Contains(w.Body.String(), pii) {
				t.Fatal("status exposure", pii)
			}
		}
		f.calls = 0
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if router.Match(chi.NewRouteContext(), method, "/admin/mail-outbox/12d5034d-ce98-4456-9f67-48057277cddd/resend") {
			t.Fatal("resend API registered")
		}
	}
}
