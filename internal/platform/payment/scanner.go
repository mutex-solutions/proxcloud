package payment

import (
	"context"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// scanner.go is the PENDING-transaction reconciler (a distributed-locked scheduled job running
// every 20 minutes: scanAccountCreditTransactions +
// scanCollectTransactions). Without it, an async/redirect deposit whose callback never landed —
// or a collect the pod died mid-processing — stays PENDING forever and the credit never
// materializes. The scan window: status PENDING, createdAt in
// (now-24h, now-20min) — old enough to not race the live flow, young enough to still matter.

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

// Scan runs both halves. Returns the number of transactions
// examined (attempted), not necessarily advanced — a still-pending gateway status is a no-op.
func (s *TransactionScanner) Scan(ctx context.Context) (int, error) {
	now := s.now()
	from, to := now.Add(-24*time.Hour), now.Add(-20*time.Minute)
	n1, err1 := s.scanAccountCredits(ctx, from, to)
	n2, err2 := s.scanCollects(ctx, from, to)
	if err1 != nil {
		return n1 + n2, err1
	}
	return n1 + n2, err2
}

// scanAccountCredits re-drives each stuck PENDING deposit with an
// externalId through ProcessAddFunds; a processing error marks it FAILED with the
// gateway message, and never aborts the batch.
func (s *TransactionScanner) scanAccountCredits(ctx context.Context, from, to time.Time) (int, error) {
	txns, err := s.billing.PendingAccountCreditTransactions(ctx, from, to)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range txns {
		t := &txns[i]
		if t.ExternalID == "" {
			continue // blank externalId — nothing to poll at the gateway
		}
		n++
		if _, err := s.addFunds.ProcessAddFunds(ctx, t.ID); err != nil {
			t.Status = "FAILED"
			t.GatewayMessage = err.Error()
			if _, serr := s.billing.SaveAccountCreditTransaction(ctx, t); serr != nil {
				s.log.Error("txn-scan: save failed account-credit txn", "txn", t.ID, "err", serr)
			}
			s.log.Error("txn-scan: account-credit txn failed", "txn", t.ID, "err", err)
		}
	}
	return n, nil
}

// scanCollects: bill already PAID → txn CANCELLED (a late duplicate
// collect must not double-settle); a missing/blank bill errors → FAILED;
// else re-drive processCollect. Errors mark the txn FAILED and continue the batch.
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

func (s *TransactionScanner) scanOneCollect(ctx context.Context, t *pricing.CollectTransaction) {
	fail := func(msg string) {
		t.Status = pricing.CollectTransactionStatusFailed
		t.ErrorMessage = msg
		if _, err := s.billing.SaveCollectTransaction(ctx, t); err != nil {
			s.log.Error("txn-scan: save failed collect txn", "txn", t.ID, "err", err)
		}
	}
	// a null/absent bill → FAILED. A
	// PENDING collect this old with no resolvable bill is an anomaly either way.
	if t.BillID == "" {
		fail("collect transaction has no bill")
		return
	}
	bill, err := s.billing.BillByID(ctx, t.BillID)
	if err != nil || bill == nil {
		fail("Bill " + t.BillID + " not found")
		return
	}
	if bill.Status == pricing.BillStatusPaid {
		// already-paid bill → the stuck collect is CANCELLED (never double-settle).
		t.Status = pricing.CollectTransactionStatusCancelled
		if _, err := s.billing.SaveCollectTransaction(ctx, t); err != nil {
			s.log.Error("txn-scan: save cancelled collect txn", "txn", t.ID, "err", err)
		}
		return
	}
	if _, err := s.collect.processCollect(ctx, t); err != nil {
		fail(err.Error())
	}
}
