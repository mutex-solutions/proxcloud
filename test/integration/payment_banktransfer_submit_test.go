//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/payment"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// newClientBillingRouter builds the tenant billing router (org.BillingHandler) over db with the
// AddFundsService (+ capture notifier) wired — enough for the bank-transfer routes.
func newClientBillingRouter(t *testing.T, db *pgdoc.DB, cap *captureNotifier) http.Handler {
	t.Helper()
	ctx := context.Background()
	orgRepo := org.NewRepo(db)
	if err := orgRepo.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure org indexes: %v", err)
	}
	billingRepo, pricingRepo := billing.NewRepo(db), pricing.NewRepo(db)
	addFunds := payment.NewAddFundsService(billingRepo, pricingRepo, nil)
	addFunds.SetNotifier(cap)
	addFunds.SetAdminURL("https://admin.example.com/")
	h := org.NewBillingHandler(org.NewService(orgRepo, billingRepo, nil), org.NewPolicy(orgRepo), billingRepo, pricingRepo,
		nil, addFunds, nil, nil, "", user.NewRepo(db), nil)
	r := chi.NewRouter()
	h.Routes(r)
	return r
}

// TestBankTransferSubmit covers the customer's "I have transferred" step + the proof downloads:
// happy path stores the fields and notifies company.financeEmail; another profile's transfer → 404;
// a settled transfer → 409; an oversize proof → 413; a wrong content type → 400; the list strips
// the proof bytes; the client and admin proof routes stream the file.
func TestBankTransferSubmit(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	cap := &captureNotifier{}
	h := newClientBillingRouter(t, db, cap)

	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD", "defaultConfiguration": true,
		"company": pgdoc.M{"businessName": "Example", "financeEmail": "finance@example.com"}})
	const bpID, otherBP, gwID = "bp-1", "bp-2", "gw-bank"
	orgRepo := org.NewRepo(db)
	o, err := orgRepo.Insert(ctx, &org.Organization{Name: "Acme", BillingProfileID: bpID, CustomInfo: map[string]any{}})
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": bpID, "organizationId": o.ID, "currency": "USD", "email": "bea@example.com",
		"firstName": "Bea", "lastName": "Ng", "companyName": "Bea Ltd"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": otherBP, "currency": "USD", "email": "x@example.com"})
	if _, err := orgRepo.AddMember(ctx, o.ID, "cust-1", rbac.RoleOwner); err != nil {
		t.Fatalf("add member: %v", err)
	}
	now := time.Now().UTC()
	transfer := func(bp, status string) string {
		return mustInsertID(t, db, "bankTransfer", pgdoc.M{
			"status": status, "paymentGatewayId": gwID, "referenceNumber": 482913, "billingProfileId": bp,
			"accountCreditTransactionId": pgdoc.NewID(), "fullName": "Bea Ng", "grossAmount": decimalOf(t, "10.90"),
			"currency": "USD", "createdAt": now, "billId": "bill-1", "invoiceNumber": "INV-2026-000007",
		})
	}
	pending := transfer(bpID, "PENDING")
	foreign := transfer(otherBP, "PENDING")
	settled := transfer(bpID, "APPROVED")

	cust := &httpx.RequestContext{Sub: "cust-1", Email: "cust@example.com"}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(httpx.WithRC(req.Context(), cust))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	submitPath := func(bp, id string) string { return "/payment/" + bp + "/bank-transfers/" + id + "/submit" }
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	pngB64 := base64.StdEncoding.EncodeToString(png)
	good := `{"transferredAt":"2026-09-03T10:00:00Z","bankReference":"TRX-42","note":"sent from DBS",` +
		`"proof":{"fileName":"slip.png","contentType":"image/png","dataBase64":"` + pngB64 + `"}}`

	t.Run("happy path stores fields + notifies finance", func(t *testing.T) {
		w := do(http.MethodPost, submitPath(bpID, pending), good)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		d := dataOf(t, w)
		if d["status"] != "PENDING" || d["submittedAt"] == nil || d["bankReference"] != "TRX-42" || d["note"] != "sent from DBS" {
			t.Fatalf("response: %v", d)
		}
		if p, _ := d["proof"].(map[string]any); p == nil || p["fileName"] != "slip.png" || p["dataBase64"] != nil || p["size"] != float64(len(png)) {
			t.Fatalf("proof meta in response: %v", d["proof"])
		}
		var stored pgdoc.M
		if ok, _ := db.C("bankTransfer").Get(ctx, pending, &stored); !ok {
			t.Fatal("transfer vanished")
		}
		if stored["status"] != "PENDING" || stored["transferredAt"] != "2026-09-03T10:00:00Z" {
			t.Fatalf("stored: %v", stored)
		}
		if p, _ := stored["proof"].(map[string]any); p == nil || p["dataBase64"] != pngB64 || p["contentType"] != "image/png" {
			t.Fatalf("stored proof: %v", stored["proof"])
		}
		if len(cap.calls) != 1 {
			t.Fatalf("notifier calls = %d, want 1", len(cap.calls))
		}
		c := cap.calls[0]
		if c.key != "bank_transfer_submitted" || len(c.to) != 1 || c.to[0] != "finance@example.com" {
			t.Fatalf("notification: key=%s to=%v", c.key, c.to)
		}
		want := map[string]any{"fullName": "Bea Ng", "companyName": "Bea Ltd", "amount": "10.90", "currency": "USD",
			"bankReference": "TRX-42", "transferredAt": "2026-09-03", "invoiceNumber": "INV-2026-000007", "billId": "bill-1",
			"adminUrl": "https://admin.example.com/clients/bank-transfers"}
		for k, v := range want {
			if c.vars[k] != v {
				t.Errorf("vars[%s] = %v, want %v", k, c.vars[k], v)
			}
		}
		if c.vars["referenceNumber"] != float64(482913) {
			t.Errorf("vars[referenceNumber] = %v", c.vars["referenceNumber"])
		}
	})

	t.Run("another profile's transfer → 404", func(t *testing.T) {
		if w := do(http.MethodPost, submitPath(bpID, foreign), good); w.Code != 404 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("settled transfer → 409", func(t *testing.T) {
		if w := do(http.MethodPost, submitPath(bpID, settled), good); w.Code != 409 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("oversize proof → 413", func(t *testing.T) {
		big := base64.StdEncoding.EncodeToString(make([]byte, payment.BankTransferProofMaxBytes+1))
		body := `{"proof":{"fileName":"big.pdf","contentType":"application/pdf","dataBase64":"` + big + `"}}`
		if w := do(http.MethodPost, submitPath(bpID, pending), body); w.Code != 413 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("wrong content type → 400", func(t *testing.T) {
		body := `{"proof":{"fileName":"x.exe","contentType":"application/octet-stream","dataBase64":"` + pngB64 + `"}}`
		if w := do(http.MethodPost, submitPath(bpID, pending), body); w.Code != 400 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("no add_funds permission → 403", func(t *testing.T) {
		if _, err := orgRepo.AddMember(ctx, o.ID, "viewer-1", rbac.RoleMember); err != nil {
			t.Fatalf("add viewer: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, submitPath(bpID, pending), strings.NewReader(good))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "viewer-1"}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})

	t.Run("list is newest-first without proof bytes", func(t *testing.T) {
		w := do(http.MethodGet, "/payment/"+bpID+"/bank-transfers", "")
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var env struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if len(env.Data) != 2 { // bp-1 owns pending + settled; bp-2's is excluded
			t.Fatalf("list = %d items (%s)", len(env.Data), w.Body)
		}
		for _, d := range env.Data {
			if d["id"] == nil || d["billingProfileId"] != bpID {
				t.Fatalf("row: %v", d)
			}
			if p, ok := d["proof"].(map[string]any); ok && p["dataBase64"] != nil {
				t.Fatalf("list leaked proof bytes: %v", d)
			}
		}
	})

	t.Run("client proof download", func(t *testing.T) {
		w := do(http.MethodGet, "/payment/"+bpID+"/bank-transfers/"+pending+"/proof", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != string(png) {
			t.Fatalf("status=%d ct=%s len=%d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
		if !strings.Contains(w.Header().Get("Content-Disposition"), `"slip.png"`) {
			t.Fatalf("disposition=%s", w.Header().Get("Content-Disposition"))
		}
		if w := do(http.MethodGet, "/payment/"+bpID+"/bank-transfers/"+foreign+"/proof", ""); w.Code != 404 {
			t.Fatalf("foreign proof status=%d", w.Code)
		}
		if w := do(http.MethodGet, "/payment/"+bpID+"/bank-transfers/"+settled+"/proof", ""); w.Code != 404 {
			t.Fatalf("no-proof status=%d", w.Code)
		}
	})

	t.Run("admin proof download + list strips bytes", func(t *testing.T) {
		const iss, cid = "test-iss", "test-cid"
		ah := newAdminRouter(db, iss, cid, nil)
		op := &httpx.RequestContext{Sub: "op-1", Email: "op@example.com", Issuer: iss, Azp: cid}
		w := doAdmin(ah, op, http.MethodGet, "/admin/bank-transfer/"+pending+"/proof", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != string(png) {
			t.Fatalf("status=%d ct=%s body=%s", w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		if w := doAdmin(ah, op, http.MethodGet, "/admin/bank-transfer/nope/proof", ""); w.Code != 404 {
			t.Fatalf("unknown status=%d", w.Code)
		}
		w = doAdmin(ah, op, http.MethodGet, "/admin/bank-transfer?integrationId="+gwID, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), pngB64) {
			t.Fatalf("admin list status=%d leaked=%v", w.Code, strings.Contains(w.Body.String(), pngB64))
		}
		if !strings.Contains(w.Body.String(), `"bankReference":"TRX-42"`) || !strings.Contains(w.Body.String(), `"fileName":"slip.png"`) {
			t.Fatalf("admin list lacks submit fields: %s", w.Body)
		}
	})
}

// TestBankTransferCancel: a customer withdraws a transfer they never made. Cancelling a PENDING,
// unreported transfer flips it and its deposit transaction to CANCELLED; a second cancel, a
// reported ("I have transferred") transfer and a settled one answer 409; another profile's → 404.
func TestBankTransferCancel(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	h := newClientBillingRouter(t, db, &captureNotifier{})

	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD", "defaultConfiguration": true})
	const bpID, otherBP, gwID = "bp-1", "bp-2", "gw-bank"
	orgRepo := org.NewRepo(db)
	o, err := orgRepo.Insert(ctx, &org.Organization{Name: "Acme", BillingProfileID: bpID, CustomInfo: map[string]any{}})
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": bpID, "organizationId": o.ID, "currency": "USD", "email": "bea@example.com"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": otherBP, "currency": "USD", "email": "x@example.com"})
	if _, err := orgRepo.AddMember(ctx, o.ID, "cust-1", rbac.RoleOwner); err != nil {
		t.Fatalf("add member: %v", err)
	}
	now := time.Now().UTC()
	transfer := func(bp, status string, extra pgdoc.M) (btID, txnID string) {
		txnID = mustInsertID(t, db, "accountCreditTransaction", pgdoc.M{
			"billingProfileId": bp, "status": "PENDING", "paymentGatewayId": gwID, "amount": decimalOf(t, "10"),
			"grossAmount": decimalOf(t, "10.90"), "currency": "USD", "createdAt": now, "metadata": pgdoc.M{},
		})
		doc := pgdoc.M{
			"status": status, "paymentGatewayId": gwID, "referenceNumber": 482913, "billingProfileId": bp,
			"accountCreditTransactionId": txnID, "fullName": "Bea Ng", "grossAmount": decimalOf(t, "10.90"),
			"currency": "USD", "createdAt": now,
		}
		for k, v := range extra {
			doc[k] = v
		}
		return mustInsertID(t, db, "bankTransfer", doc), txnID
	}
	pending, pendingTxn := transfer(bpID, "PENDING", nil)
	reported, reportedTxn := transfer(bpID, "PENDING", pgdoc.M{"submittedAt": now, "bankReference": "TRX-1"})
	settled, _ := transfer(bpID, "APPROVED", nil)
	foreign, _ := transfer(otherBP, "PENDING", nil)

	cust := &httpx.RequestContext{Sub: "cust-1", Email: "cust@example.com"}
	cancel := func(bp, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/payment/"+bp+"/bank-transfers/"+id+"/cancel", nil)
		req = req.WithContext(httpx.WithRC(req.Context(), cust))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	txnStatus := func(id string) string {
		var doc pgdoc.M
		if ok, _ := db.C("accountCreditTransaction").Get(ctx, id, &doc); !ok {
			t.Fatalf("transaction %s vanished", id)
		}
		s, _ := doc["status"].(string)
		return s
	}

	t.Run("pending + unreported → CANCELLED with its transaction", func(t *testing.T) {
		w := cancel(bpID, pending)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		if d := dataOf(t, w); d["status"] != "CANCELLED" || d["cancelledAt"] == nil || d["id"] != pending {
			t.Fatalf("response: %v", d)
		}
		var stored pgdoc.M
		if ok, _ := db.C("bankTransfer").Get(ctx, pending, &stored); !ok || stored["status"] != "CANCELLED" {
			t.Fatalf("stored: %v", stored)
		}
		if got := txnStatus(pendingTxn); got != "CANCELLED" {
			t.Fatalf("deposit transaction status = %q, want CANCELLED", got)
		}
		if w := cancel(bpID, pending); w.Code != 409 {
			t.Fatalf("second cancel: status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("already reported → 409, transaction untouched", func(t *testing.T) {
		if w := cancel(bpID, reported); w.Code != 409 || !strings.Contains(w.Body.String(), "already reported") {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		if got := txnStatus(reportedTxn); got != "PENDING" {
			t.Fatalf("reported transaction status = %q, want PENDING", got)
		}
	})
	t.Run("settled → 409", func(t *testing.T) {
		if w := cancel(bpID, settled); w.Code != 409 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})
	t.Run("another profile's transfer → 404", func(t *testing.T) {
		if w := cancel(bpID, foreign); w.Code != 404 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	})
}
