package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	stripe "github.com/stripe/stripe-go/v82"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// scanner.go is the PENDING-transaction reconciler (a distributed-locked scheduled job running
// every 20 minutes: scanAccountCreditTransactions +
// scanCollectTransactions). Without it, an async/redirect deposit whose callback never landed —
// or a collect the pod died mid-processing — stays PENDING forever and the credit never
// materializes. The scan window: status PENDING, createdAt in
// (now-24h, now-20min) — old enough to not race the live flow, young enough to still matter.
//
// A failed re-check says nothing about the payment, so it does not close the transaction: the money may
// have moved, and a FAILED deposit that did succeed takes a manual repair. Only the gateway saying it has
// no such intent is terminal. Everything else stays PENDING with the reason on the row, re-checked until
// it ages out of the window. That window is the bound: past it the scanner stops asking and the row stays
// PENDING, where a late gateway callback or an operator re-check can still settle it. A bill charge in that state keeps
// blocking new charges for the bill (chargeBusy): the money may have moved, so only an answer may lift the guard. A gateway
// answer of cancelled ends a charge by itself; one of declined or never confirmed lets an operator close it (CloseCollect).

// TransactionScanner re-drives stuck PENDING transactions through the same Process paths the
// callbacks use (ProcessAddFunds / processCollect — both idempotent; a still-pending gateway
// status leaves the txn PENDING for the next pass).
type TransactionScanner struct {
	billing  *billing.Repo
	addFunds *AddFundsService
	collect  *CollectService
	now      func() time.Time
	log      *slog.Logger
}

func NewTransactionScanner(b *billing.Repo, af *AddFundsService, c *CollectService, log *slog.Logger) *TransactionScanner {
	if log == nil {
		log = slog.Default()
	}
	return &TransactionScanner{
		billing: b, addFunds: af, collect: c,
		now: func() time.Time { return time.Now().UTC() }, log: log,
	}
}

func (s *TransactionScanner) WithNow(now func() time.Time) *TransactionScanner { s.now = now; return s }

const (
	scanMaxAge   = 24 * time.Hour
	scanMinAge   = 20 * time.Minute
	scanInterval = 20 * time.Minute
)

// intentGone reports the gateway answering that it has no such payment intent. Any other error (network,
// 5xx, rate limit, rejected credentials, a removed integration) leaves the outcome unknown.
func intentGone(err error) bool {
	var se *stripe.Error
	if errors.As(err, &se) {
		return se.HTTPStatusCode == 404
	}
	var ae *AirwallexError
	return errors.As(err, &ae) && ae.HTTPStatus == 404
}

// retryNote is the reason kept on a transaction whose re-check failed, and logs the failure: an error
// once the row is about to leave the window, since nothing retries it after that.
func (s *TransactionScanner) retryNote(id string, createdAt *time.Time, err error) string {
	if createdAt == nil {
		s.log.Warn("txn-scan: re-check failed", "txn", id, "err", err)
		return "Could not confirm with the payment gateway: " + err.Error()
	}
	stop := createdAt.Add(scanMaxAge)
	if s.now().After(stop) {
		s.log.Warn("txn-scan: operator re-check failed", "txn", id, "err", err)
		return fmt.Sprintf("Could not confirm with the payment gateway: %v. Automatic re-checks have ended; an operator can re-check it.", err)
	}
	if s.now().Add(2 * scanInterval).After(stop) {
		s.log.Error("txn-scan: re-checks are ending with the outcome unknown", "txn", id, "stop", stop, "err", err)
	} else {
		s.log.Warn("txn-scan: re-check failed", "txn", id, "err", err)
	}
	return fmt.Sprintf("Could not confirm with the payment gateway: %v. Re-checked automatically until %s.", err, stop.UTC().Format(time.RFC3339))
}

// Scan runs both halves. Returns the number of transactions
// examined (attempted), not necessarily advanced — a still-pending gateway status is a no-op.
func (s *TransactionScanner) Scan(ctx context.Context) (int, error) {
	now := s.now()
	from, to := now.Add(-scanMaxAge), now.Add(-scanMinAge)
	n1, err1 := s.scanAccountCredits(ctx, from, to)
	n2, err2 := s.scanCollects(ctx, from, to)
	n3 := s.scanRefunds(ctx, now.AddDate(0, 0, -30))
	if err1 != nil {
		return n1 + n2 + n3, err1
	}
	return n1 + n2 + n3, err2
}

// scanRefunds polls the refunds that were still processing at the gateway when the operator
// requested them (REFUNDED deposits with metadata.refund.status PENDING, up to 30 days back) —
// see AddFundsService.ReconcileRefund for what a settled / failed refund does.
func (s *TransactionScanner) scanRefunds(ctx context.Context, since time.Time) int {
	txns, err := s.billing.RefundedAccountCreditTransactions(ctx, since)
	if err != nil {
		s.log.Error("txn-scan: list refunded txns", "err", err)
		return 0
	}
	n := 0
	for i := range txns {
		t := &txns[i]
		if st := refundState(t); st == nil || st["status"] != "PENDING" {
			continue
		}
		n++
		if err := s.addFunds.ReconcileRefund(ctx, t); err != nil {
			s.log.Error("txn-scan: reconcile refund", "txn", t.ID, "err", err)
		}
	}
	return n
}

// scanAccountCredits re-drives each stuck PENDING deposit with an externalId through ProcessAddFunds. A
// processing error never aborts the batch: see the file comment for what it does to the row.
func (s *TransactionScanner) scanAccountCredits(ctx context.Context, from, to time.Time) (int, error) {
	txns, err := s.billing.PendingAccountCreditTransactions(ctx, from, to)
	if err != nil {
		return 0, err
	}
	// A settle that died after its claim: put it back and re-drive it like any other. The settle is
	// idempotent, so even a claim still in flight here only risks a second thank-you mail.
	stuck, err := s.billing.StuckAccountCreditTransactions(ctx, to)
	if err != nil {
		return 0, err
	}
	for i := range stuck {
		if err := s.billing.ReleaseAccountCreditTransaction(ctx, stuck[i].ID); err != nil {
			s.log.Error("txn-scan: release stuck txn", "txn", stuck[i].ID, "err", err)
			continue
		}
		stuck[i].Status = "PENDING"
		txns = append(txns, stuck[i])
	}
	n := 0
	for i := range txns {
		t := &txns[i]
		if t.ExternalID == "" {
			continue // blank externalId — nothing to poll at the gateway
		}
		n++
		if _, err := s.addFunds.ProcessAddFunds(ctx, t.ID); err != nil {
			status, msg := "PENDING", ""
			if intentGone(err) {
				status, msg = "FAILED", err.Error()
				s.log.Error("txn-scan: deposit has no intent at the gateway", "txn", t.ID, "err", err)
			} else {
				msg = s.retryNote(t.ID, t.CreatedAt, err)
			}
			if serr := s.billing.UpdatePendingAccountCreditTransaction(ctx, t.ID, status, msg); serr != nil {
				s.log.Error("txn-scan: save account-credit txn", "txn", t.ID, "err", serr)
			}
		}
	}
	return n, nil
}

// scanCollects: bill already PAID → the charge becomes credit if it succeeded, else CANCELLED (a late
// duplicate collect must not double-settle); a missing bill → FAILED; else re-drive processCollect. A
// processing error never aborts the batch: see the file comment for what it does to the row.
func (s *TransactionScanner) scanCollects(ctx context.Context, from, to time.Time) (int, error) {
	txns, err := s.billing.PendingCollectTransactions(ctx, from, to)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range txns {
		t := &txns[i]
		if t.ExternalID == "" {
			continue
		}
		n++
		s.scanOneCollect(ctx, t)
	}
	return n, nil
}

// RecheckCollect is the operator's re-check of one charge, past the scan window too: it asks the gateway the way a scan
// pass does and returns the row as it then stands, with the gateway's answer on it. A charge with no gateway reference
// cannot be asked about (see CloseCollect). Nil when there is no such charge.
func (s *TransactionScanner) RecheckCollect(ctx context.Context, id string) (*pricing.CollectTransaction, error) {
	t, err := s.billing.CollectTransactionByID(ctx, id)
	if err != nil || t == nil || t.Status != pricing.CollectTransactionStatusPending {
		return t, err
	}
	if t.ExternalID == "" {
		return nil, httpx.NewError(409, 409, fmt.Sprintf("This charge has no gateway reference, so the gateway cannot be asked about it. Look for reference %s at the gateway. If nothing was charged, close it.", id))
	}
	s.scanOneCollect(ctx, t)
	return s.billing.CollectTransactionByID(ctx, id)
}

// CloseCollect is the operator's way to end a PENDING charge that blocks its bill. With a gateway reference the gateway is
// asked again here, and only an answer that the charge did not succeed closes it, so nothing that may still capture is
// closed by hand: cancelling it at the gateway and re-checking ends those. With no reference nothing can ask the gateway
// (the process stopped between the row and its answer), so the operator who found no charge there vouches for it. Refused
// while the live flow could still be running.
func (s *TransactionScanner) CloseCollect(ctx context.Context, id string) (*pricing.CollectTransaction, error) {
	t, err := s.billing.CollectTransactionByID(ctx, id)
	if err != nil || t == nil {
		return nil, err
	}
	if t.Status != pricing.CollectTransactionStatusPending {
		return nil, httpx.NewError(409, 409, "Only a PENDING charge can be closed.")
	}
	if t.CreatedAt != nil && s.now().Sub(*t.CreatedAt) < scanMinAge {
		return nil, httpx.NewError(409, 409, "This charge is too recent: it may still be running. Try again later.")
	}
	msg := "Closed by an operator: no charge found at the gateway"
	if t.ExternalID != "" {
		pi, err := s.collect.retrieveCharge(ctx, t)
		if err != nil {
			return nil, httpx.NewError(409, 409, fmt.Sprintf("The gateway could not be asked, so the charge was not closed: %v", err))
		}
		if !chargeDidNotSucceed(pi.Status) {
			return nil, httpx.NewError(409, 409, fmt.Sprintf("The gateway reports %s, so the charge was not closed. Re-check records a final answer; one that may still complete has to be cancelled at the gateway first.", pi.Status))
		}
		msg = "Closed by an operator. " + chargeNote(pi)
	}
	if err := s.billing.UpdatePendingCollectTransaction(ctx, id, pricing.CollectTransactionStatusCancelled, msg); err != nil {
		return nil, err
	}
	return s.billing.CollectTransactionByID(ctx, id)
}

func (s *TransactionScanner) scanOneCollect(ctx context.Context, t *pricing.CollectTransaction) {
	save := func(status pricing.CollectTransactionStatus, msg string) {
		if err := s.billing.UpdatePendingCollectTransaction(ctx, t.ID, status, msg); err != nil {
			s.log.Error("txn-scan: save collect txn", "txn", t.ID, "err", err)
		}
	}
	failed := func(err error) {
		if intentGone(err) {
			s.log.Error("txn-scan: charge has no intent at the gateway", "txn", t.ID, "err", err)
			save(pricing.CollectTransactionStatusFailed, err.Error())
			return
		}
		save(pricing.CollectTransactionStatusPending, s.retryNote(t.ID, t.CreatedAt, err))
	}
	// A deposit-by-card (no bill) with an unknown outcome: ask the gateway — a SUCCESS mints the
	// credit the customer paid for, a decline fails it. Never fail it blind.
	process := func() {
		done, err := s.collect.processCollect(ctx, t)
		switch {
		case err != nil:
			failed(err)
		case done.Status == pricing.CollectTransactionStatusPending:
			save(pricing.CollectTransactionStatusPending, done.ErrorMessage)
		}
	}
	if t.BillID == "" {
		process()
		return
	}
	bill, err := s.billing.BillByID(ctx, t.BillID)
	if err != nil {
		failed(err)
		return
	}
	if bill == nil {
		save(pricing.CollectTransactionStatusFailed, "Bill "+t.BillID+" not found")
		return
	}
	if bill.Status == pricing.BillStatusPaid && !billHolds(bill, t.ID) {
		// The bill was settled by something else meanwhile (credits, a bank transfer, another
		// charge). Ask the gateway before closing the stuck charge: money that DID move must not
		// vanish from the books; a charge that did not succeed is CANCELLED.
		settled, err := s.collect.settleSurplus(ctx, t)
		switch {
		case settled:
		case err == nil, intentGone(err):
			save(pricing.CollectTransactionStatusCancelled, "")
		default:
			failed(err)
		}
		return
	}
	process()
}
