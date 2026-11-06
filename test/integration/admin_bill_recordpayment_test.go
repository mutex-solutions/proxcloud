//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// recordingHooks captures the PayService side-effects (bill_is_paid mail + suspension review).
type recordingHooks struct {
	mails    []string
	reviewed []string
}

func (h *recordingHooks) SendTemplate(_ context.Context, key string, _ []string, _ map[string]any) error {
	h.mails = append(h.mails, key)
	return nil
}

func (h *recordingHooks) ReviewBillingProfile(_ context.Context, p *billing.BillingProfile) error {
	h.reviewed = append(h.reviewed, p.ID)
	return nil
}

// newAdminRouter builds the admin router over db under REMOTE_OIDC (iss/cid), with the
// PayService (+ hooks) wired for record-payment.
func newAdminRouter(db *pgdoc.DB, iss, cid string, hooks *recordingHooks) http.Handler {
	billingRepo, pricingRepo := billing.NewRepo(db), pricing.NewRepo(db)
	pay := billing.NewPayService(billingRepo, pricingRepo)
	if hooks != nil {
		pay.SetNotifier(hooks)
		pay.SetReviewer(hooks)
	}
	h := admin.NewHandler(admin.NewRepo(db), nil, user.NewRepo(db), nil, pricingRepo, billingRepo, nil, nil, nil, nil, "", iss, cid)
	h.SetPayService(pay)
	r := chi.NewRouter()
	h.Routes(r)
	return r
}

// doAdmin performs one admin request as the given request context; body is raw JSON (or "").
func doAdmin(h http.Handler, rc *httpx.RequestContext, method, path, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(httpx.WithRC(req.Context(), rc))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func dataOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body %s: %v", w.Body, err)
	}
	return env.Data
}

// TestAdminRecordBillPayment covers POST /admin/bill/{id}/record-payment: full → PAID (mail +
// review fired, SUCCESS collect txn recorded), partial → stays SENT with the collected amount,
// OPEN → 400, PAID → 409, unknown → 404, bad method → 400, BankTransfer gateway id stamped.
func TestAdminRecordBillPayment(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "bp-1", "currency": "USD", "email": "acct@example.com", "firstName": "Ada", "lastName": "Lovelace", "status": "ACTIVE"})
	// 10% tax → a 100 net bill is 110 gross; the accountant enters gross figures.
	mustInsert(t, db, "taxRate", pgdoc.M{"name": "VAT", "level": "ALL", "rateLevels": []any{pgdoc.M{"level": 1, "percentage": 10}}})
	gwID := mustInsertID(t, db, "thirdPartyIntegration", pgdoc.M{"name": "Wire", "thirdParty": "BankTransfer", "category": "Payment"})

	bill := func(status, net string) string {
		return mustInsertID(t, db, "bill", pgdoc.M{
			"billingProfileId": "bp-1", "status": status, "invoiceCurrency": "USD",
			"billingCycle": pgdoc.M{"startDate": "2026-08-01T00:00:00Z", "endDate": "2026-08-31T23:59:59Z"},
			"items":        []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, net)}},
		})
	}
	const iss, cid = "test-iss", "test-cid"
	hooks := &recordingHooks{}
	h := newAdminRouter(db, iss, cid, hooks)
	admin := &httpx.RequestContext{Sub: "op-1", Email: "op@example.com", Issuer: iss, Azp: cid}
	post := func(id, body string) *httptest.ResponseRecorder {
		return doAdmin(h, admin, http.MethodPost, "/admin/bill/"+id+"/record-payment", body)
	}

	t.Run("full payment → PAID + mail + review + SUCCESS txn", func(t *testing.T) {
		id := bill("SENT", "100")
		w := post(id, `{"method":"BANK_TRANSFER","reference":"TRX-42"}`)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		d := dataOf(t, w)
		if d["status"] != "PAID" {
			t.Fatalf("overview status = %v, want PAID (%s)", d["status"], w.Body)
		}
		if ca, _ := d["collectedAmounts"].([]any); len(ca) != 1 {
			t.Fatalf("collectedAmounts = %v, want 1 entry", d["collectedAmounts"])
		}
		var persisted pricing.Bill
		if _, err := db.C("bill").Get(ctx, id, &persisted); err != nil || persisted.Status != pricing.BillStatusPaid {
			t.Fatalf("persisted bill status = %s err=%v, want PAID", persisted.Status, err)
		}
		if len(persisted.CollectedAmounts) != 1 || persisted.CollectedAmounts[0].Amount.String() != "100" {
			t.Fatalf("collected net should be 100 (110 gross ÷ 1.10): %+v", persisted.CollectedAmounts)
		}
		var txn pricing.CollectTransaction
		if _, err := db.C("collectTransaction").FindOne(ctx, pgdoc.M{"billId": id}, &txn); err != nil {
			t.Fatal(err)
		}
		if txn.Status != pricing.CollectTransactionStatusSuccess || txn.PaymentGatewayID != gwID || txn.GrossAmount.String() != "110" {
			t.Fatalf("txn = %+v, want SUCCESS / gateway %s / gross 110", txn, gwID)
		}
		if txn.Metadata["method"] != "BANK_TRANSFER" || txn.Metadata["reference"] != "TRX-42" || txn.Metadata["recordedBy"] != "op-1" {
			t.Fatalf("txn metadata = %v", txn.Metadata)
		}
		if len(hooks.mails) != 1 || hooks.mails[0] != "bill_is_paid" || len(hooks.reviewed) != 1 || hooks.reviewed[0] != "bp-1" {
			t.Fatalf("hooks: mails=%v reviewed=%v", hooks.mails, hooks.reviewed)
		}
		// a second payment on the now-PAID bill → 409
		if w := post(id, `{"method":"OTHER"}`); w.Code != http.StatusConflict {
			t.Fatalf("PAID bill: status=%d, want 409", w.Code)
		}
	})

	t.Run("partial payment stays SENT with the collected amount", func(t *testing.T) {
		hooks.mails, hooks.reviewed = nil, nil
		id := bill("SENT", "100")
		w := post(id, `{"amount":"55","method":"OTHER","reference":"cash","paidAt":"2026-09-01T10:00:00Z"}`)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		d := dataOf(t, w)
		if d["status"] != "SENT" {
			t.Fatalf("status = %v, want SENT", d["status"])
		}
		// 55 gross of 110 → 50 net collected → 50 net unpaid → 55 gross unpaid
		if unpaid, _ := d["unpaidAmount"].(json.Number); string(unpaid) != "55" && d["unpaidAmount"] != 55.0 {
			t.Fatalf("unpaidAmount = %v (%T), want 55", d["unpaidAmount"], d["unpaidAmount"])
		}
		if len(hooks.mails) != 0 || len(hooks.reviewed) != 0 {
			t.Fatalf("partial payment must not fire the paid hooks: %v %v", hooks.mails, hooks.reviewed)
		}
		var txn pricing.CollectTransaction
		_, _ = db.C("collectTransaction").FindOne(ctx, pgdoc.M{"billId": id}, &txn)
		if txn.CreatedAt == nil || txn.CreatedAt.Format("2006-01-02") != "2026-09-01" {
			t.Fatalf("txn createdAt should be the paidAt date: %v", txn.CreatedAt)
		}
		// the remainder settles it
		if w := post(id, `{"method":"BANK_TRANSFER"}`); w.Code != 200 || dataOf(t, w)["status"] != "PAID" {
			t.Fatalf("remainder: status=%d body=%s", w.Code, w.Body)
		}
	})

	t.Run("guards", func(t *testing.T) {
		if w := post(bill("OPEN", "10"), `{"method":"BANK_TRANSFER"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "not issued") {
			t.Fatalf("OPEN: status=%d body=%s, want 400 not issued", w.Code, w.Body)
		}
		if w := post(bill("PAID", "10"), `{"method":"BANK_TRANSFER"}`); w.Code != 409 {
			t.Fatalf("PAID: status=%d, want 409", w.Code)
		}
		if w := post("000000000000000000000000", `{"method":"BANK_TRANSFER"}`); w.Code != 404 {
			t.Fatalf("missing: status=%d, want 404", w.Code)
		}
		sent := bill("SENT", "10")
		if w := post(sent, `{"method":"CARD"}`); w.Code != 400 {
			t.Fatalf("bad method: status=%d, want 400", w.Code)
		}
		if w := post(sent, `{"method":"OTHER","amount":"0"}`); w.Code != 400 {
			t.Fatalf("zero amount: status=%d, want 400", w.Code)
		}
		if w := post(sent, `{"method":"OTHER","paidAt":"yesterday"}`); w.Code != 400 {
			t.Fatalf("bad paidAt: status=%d, want 400", w.Code)
		}
		// PUT status validation (Part 1d): only OPEN/SENT/PAID
		if w := doAdmin(h, admin, http.MethodPut, "/admin/bill/"+sent, `{"status":"INVOICED"}`); w.Code != 400 {
			t.Fatalf("PUT bad status: status=%d body=%s, want 400", w.Code, w.Body)
		}
		if w := doAdmin(h, admin, http.MethodPut, "/admin/bill/"+sent, `{"status":"OPEN"}`); w.Code != 200 {
			t.Fatalf("PUT valid status: status=%d body=%s, want 200", w.Code, w.Body)
		}
	})
}
