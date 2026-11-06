package payment

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// banktransfer.go — the customer's "I have transferred" step on a PENDING manual bank transfer:
// stamps submittedAt (+ the optional transfer date, bank reference, note and proof file) on the
// bankTransfer doc and notifies the operator's finance mailbox (billing configuration
// company.financeEmail) with the bank_transfer_submitted template. The transfer stays PENDING —
// only the admin approve/reject settles it.

// BankTransferProofMaxBytes caps the decoded proof file (2 MiB).
const BankTransferProofMaxBytes = 2 << 20

// bankTransferProofTypes are the accepted proof content types.
var bankTransferProofTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true}

// BankTransferProof is the customer's proof-of-payment file (base64 in transit and at rest).
type BankTransferProof struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	DataBase64  string `json:"dataBase64"`
}

// BankTransferSubmitRequest is the body of POST .../bank-transfers/{id}/submit.
type BankTransferSubmitRequest struct {
	TransferredAt string             `json:"transferredAt"` // RFC3339, optional
	BankReference string             `json:"bankReference"`
	Note          string             `json:"note"`
	Proof         *BankTransferProof `json:"proof"`
}

// SetAdminURL wires the admin console base URL ({{adminUrl}} in bank_transfer_submitted).
func (s *AddFundsService) SetAdminURL(u string) { s.adminURL = strings.TrimRight(u, "/") }

// SubmitBankTransfer validates + stores the customer's submission on the PENDING transfer `id`
// owned by profile, then notifies the finance mailbox. Returns the public (proof-stripped) doc.
// 404 when the transfer is missing or belongs to another profile, 409 when not PENDING,
// 413 when the proof exceeds the cap, 400 on any other invalid field.
func (s *AddFundsService) SubmitBankTransfer(ctx context.Context, profile *billing.BillingProfile, id string, req BankTransferSubmitRequest) (pgdoc.M, error) {
	bt, err := s.billing.BankTransferByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if bt == nil || bt["billingProfileId"] != profile.ID {
		return nil, httpx.NotFound(fmt.Sprintf("Bank transfer %s not found ", id))
	}
	if bt["status"] != "PENDING" {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Bank transfer is no longer pending")
	}
	req.BankReference, req.Note = strings.TrimSpace(req.BankReference), strings.TrimSpace(req.Note)
	if len(req.BankReference) > 120 {
		return nil, httpx.BadRequest("Bank reference must be at most 120 characters")
	}
	if len(req.Note) > 500 {
		return nil, httpx.BadRequest("Note must be at most 500 characters")
	}
	set := pgdoc.M{"submittedAt": time.Now().UTC(), "bankReference": req.BankReference, "note": req.Note}
	if req.TransferredAt != "" {
		at, err := time.Parse(time.RFC3339, req.TransferredAt)
		if err != nil {
			return nil, httpx.BadRequest("transferredAt must be an RFC3339 timestamp")
		}
		set["transferredAt"] = at.UTC()
	}
	if p := req.Proof; p != nil {
		if !bankTransferProofTypes[p.ContentType] {
			return nil, httpx.BadRequest("Proof must be a PDF, PNG or JPEG file")
		}
		if strings.TrimSpace(p.FileName) == "" || len(p.FileName) > 200 {
			return nil, httpx.BadRequest("Proof file name is required (max 200 characters)")
		}
		// Size-check the encoded length first so an oversize upload is never decoded.
		if base64.StdEncoding.DecodedLen(len(p.DataBase64)) > BankTransferProofMaxBytes+3 {
			return nil, httpx.NewError(http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, "Proof file must be at most 2 MiB")
		}
		raw, err := base64.StdEncoding.DecodeString(p.DataBase64)
		if err != nil || len(raw) == 0 {
			return nil, httpx.BadRequest("Proof file must be base64 encoded")
		}
		if len(raw) > BankTransferProofMaxBytes {
			return nil, httpx.NewError(http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, "Proof file must be at most 2 MiB")
		}
		set["proof"] = pgdoc.M{"fileName": p.FileName, "contentType": p.ContentType, "dataBase64": p.DataBase64, "size": len(raw)}
	}
	if err := s.billing.UpdateBankTransfer(ctx, id, set); err != nil {
		return nil, err
	}
	for k, v := range set {
		bt[k] = v
	}
	s.notifyFinance(ctx, profile, bt)
	return billing.BankTransferPublic(bt), nil
}

// notifyFinance emails bank_transfer_submitted to company.financeEmail (no-op when unset).
func (s *AddFundsService) notifyFinance(ctx context.Context, profile *billing.BillingProfile, bt pgdoc.M) {
	to, _ := s.billing.FinanceEmail(ctx)
	if to == "" {
		return
	}
	str := func(k string) string {
		if bt[k] == nil {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(bt[k]))
	}
	amount := str("grossAmount")
	if d, err := decimal.NewFromString(amount); err == nil {
		amount = d.StringFixed(2)
	}
	vars := map[string]any{
		"fullName": fullName(profile), "companyName": profile.CompanyName,
		"amount": amount, "currency": str("currency"), "referenceNumber": bt["referenceNumber"],
		"bankReference": str("bankReference"), "invoiceNumber": str("invoiceNumber"), "billId": str("billId"),
		"transferredAt": "", "adminUrl": s.adminURL + "/clients/bank-transfers",
	}
	if t, ok := bt["transferredAt"].(time.Time); ok {
		vars["transferredAt"] = t.Format("2006-01-02")
	}
	// The finance mailbox is an operator address, not a billing profile — send to it directly.
	if s.notifier != nil && to != "" {
		_ = s.notifier.SendTemplate(ctx, "bank_transfer_submitted", []string{to}, vars)
	}
}

// WriteBankTransferProof streams a transfer's proof as an attachment (404 envelope when none).
// Shared by the client and admin proof routes.
func WriteBankTransferProof(w http.ResponseWriter, bt pgdoc.M) {
	ct, name, data, ok := BankTransferProofBytes(bt)
	if !ok {
		httpx.WriteError(w, httpx.NotFound("No proof file on this bank transfer"))
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_, _ = w.Write(data)
}

// BankTransferProofBytes decodes the stored proof of a transfer doc: content type, file name, bytes.
// ok=false when the transfer carries no (decodable) proof.
func BankTransferProofBytes(bt pgdoc.M) (contentType, fileName string, data []byte, ok bool) {
	p, _ := bt["proof"].(map[string]any)
	if p == nil {
		return "", "", nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(fmt.Sprint(p["dataBase64"]))
	if err != nil || len(raw) == 0 {
		return "", "", nil, false
	}
	return fmt.Sprint(p["contentType"]), fmt.Sprint(p["fileName"]), raw, true
}

// CancelBankTransfer withdraws a manual transfer the customer created but never made — the
// "I clicked bank transfer by mistake" case. Only a PENDING transfer that has not been reported
// ("I have transferred") can be cancelled: once reported, money may be in flight and only the
// operator can resolve it (reject). The linked deposit transaction is CANCELLED with it so the
// billing history and the finance queue both drop it.
func (s *AddFundsService) CancelBankTransfer(ctx context.Context, profile *billing.BillingProfile, id string) (pgdoc.M, error) {
	bt, err := s.billing.BankTransferByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if bt == nil || bt["billingProfileId"] != profile.ID {
		return nil, httpx.NotFound(fmt.Sprintf("Bank transfer %s not found ", id))
	}
	if bt["status"] != "PENDING" {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Bank transfer is no longer pending")
	}
	if at, _ := bt["submittedAt"]; at != nil {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict,
			"Bank transfer already reported as made — contact us to withdraw it")
	}
	now := time.Now().UTC()
	set := pgdoc.M{"status": "CANCELLED", "cancelledAt": now}
	if err := s.billing.UpdateBankTransfer(ctx, id, set); err != nil {
		return nil, err
	}
	if txnID, _ := bt["accountCreditTransactionId"].(string); txnID != "" {
		txn, err := s.billing.AccountCreditTransactionByID(ctx, txnID)
		if err != nil {
			return nil, err
		}
		if txn != nil && txn.Status == "PENDING" {
			txn.Status = "CANCELLED"
			txn.GatewayMessage = "Cancelled by the customer before the transfer was made"
			if _, err := s.billing.SaveAccountCreditTransaction(ctx, txn); err != nil {
				return nil, err
			}
		}
	}
	for k, v := range set {
		bt[k] = v
	}
	return billing.BankTransferPublic(bt), nil
}
