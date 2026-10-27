package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// SettleAccountCredits settles credits against an amount (1:1, no rounding). The
// ordered candidate list is an input (the datastore natural-order query is deferred).
// Each leg converts through the credit's invoice currency: gross↔invoice at `now`,
// base↔invoice at the credit's createdAt. The covered amount
// removed from the balance is the invoice→base re-conversion at createdAt — which is
// NOT assumed to be the reciprocal of base→invoice (the double-multiply preserves
// any rate asymmetry). Mutates each credit's Amount in place.
func SettleAccountCredits(credits []*AccountCredit, amount decimal.Decimal, currency string, x *Exchanger, now time.Time) ([]AccountCreditSettlement, error) {
	remaining := amount
	settlements := []AccountCreditSettlement{}
	for _, ac := range credits {
		if ac.Amount == nil || ac.Amount.IsZero() { // compareTo(ZERO) != 0 (nil guard is a safe superset)
			continue
		}
		t0 := time.Time{}
		if ac.CreatedAt != nil {
			t0 = *ac.CreatedAt
		}
		remInternal, err := x.Exchange(remaining, currency, ac.InvoiceCurrency, now)
		if err != nil {
			return nil, err
		}
		creditAmount, err := x.Exchange(*ac.Amount, ac.Currency, ac.InvoiceCurrency, t0)
		if err != nil {
			return nil, err
		}
		diff := creditAmount.Sub(remInternal)
		var settledAmount decimal.Decimal
		if diff.Cmp(decimal.Zero) >= 0 {
			settledAmount = remInternal
			remInternal = decimal.Zero
		} else {
			settledAmount = creditAmount
			remInternal = diff.Abs()
		}
		remaining, err = x.Exchange(remInternal, ac.InvoiceCurrency, currency, now)
		if err != nil {
			return nil, err
		}
		covered, err := x.Exchange(settledAmount, ac.InvoiceCurrency, ac.Currency, t0)
		if err != nil {
			return nil, err
		}
		newAmt := ac.Amount.Sub(covered)
		ac.Amount = &newAmt
		settlements = append(settlements, AccountCreditSettlement{AccountCredit: ac, Settled: covered})
		if remaining.IsZero() { // exact-zero break only
			break
		}
	}
	return settlements, nil
}

// TakePromotionalCredits takes promotional credits (1:1, no rounding). The
// ordered, non-expired, remaining>0 candidate list is an input (the re-query loop is
// deferred). All amount math is in base currency; the only FX is the metadata rate
// (base→profile at `now`) stored on the applied sub-doc. Mutates RemainingAmount.
func TakePromotionalCredits(credits []*PromotionalCredit, amount decimal.Decimal, baseCurrency, profileCurrency string, x *Exchanger, now time.Time) ([]AppliedPromotionalCredit, error) {
	remaining := amount
	applied := []AppliedPromotionalCredit{}
	for i := 0; i < len(credits) && remaining.Cmp(decimal.Zero) > 0; i++ {
		pc := credits[i]
		if pc.RemainingAmount == nil || pc.RemainingAmount.Cmp(decimal.Zero) <= 0 {
			continue
		}
		promo := *pc.RemainingAmount
		diff := promo.Sub(remaining)
		rate, err := x.GetExchangeRate(baseCurrency, profileCurrency, now)
		if err != nil {
			return nil, err
		}
		r := rate
		if diff.Cmp(decimal.Zero) >= 0 {
			newRem := diff
			pc.RemainingAmount = &newRem
			amt := remaining
			applied = append(applied, AppliedPromotionalCredit{Amount: &amt, PromotionalCreditID: pc.ID, Currency: baseCurrency, ExchangeRate: &r})
			remaining = decimal.Zero
		} else {
			remaining = diff.Abs()
			z := decimal.Zero
			pc.RemainingAmount = &z
			amt := promo
			applied = append(applied, AppliedPromotionalCredit{Amount: &amt, PromotionalCreditID: pc.ID, Currency: baseCurrency, ExchangeRate: &r})
		}
	}
	return applied, nil
}

// SettleBillAmount settles `amount` against the
// bill, PROMOTIONAL credits FIRST then ACCOUNT credits for the remainder. Appends
// the applied sub-docs to the bill and mutates the credits' balances in place;
// returns the applied results so the caller can persist them. Running totals are NOT
// rounded (only the stored sub-doc amounts + the unpaid total are).
func SettleBillAmount(bill *Bill, amount decimal.Decimal, promoCredits []*PromotionalCredit, accountCredits []*AccountCredit, taxRates []TaxRate, baseCurrency, profileCurrency string, x *Exchanger, now time.Time) ([]AppliedPromotionalCredit, []AccountCreditSettlement, error) {
	grossDiff := amount
	if grossDiff.IsZero() {
		return nil, nil, nil
	}
	remainingAmount := grossDiff
	promo, err := TakePromotionalCredits(promoCredits, grossDiff, baseCurrency, profileCurrency, x, now)
	if err != nil {
		return nil, nil, err
	}
	if len(promo) > 0 {
		bill.AppliedPromotionalCredits = append(bill.AppliedPromotionalCredits, promo...)
		sum := decimal.Zero
		for _, p := range promo {
			if p.Amount != nil {
				sum = sum.Add(*p.Amount)
			}
		}
		remainingAmount = grossDiff.Sub(sum)
	}
	var acct []AccountCreditSettlement
	if !remainingAmount.IsZero() {
		if _, acct, err = settleAccountCreditLeg(bill, accountCredits, remainingAmount, taxRates, baseCurrency, x, now); err != nil {
			return nil, nil, err
		}
	}
	return promo, acct, nil
}

// settleAccountCreditLeg builds one
// AppliedAccountCredit per settlement (invoiceAmount = scaleHalfUp(invoiceExchangeRate
// × settled); grossAmount = tax(invoiceAmount); amount = scaleHalfUp(settled)) and
// decrements the running gross by the RAW settled. Returns the leftover gross.
// A nil InvoiceExchangeRate defaults to 1: some stored credits lack the field, and a settleable
// credit already passed the same-currency identity exchange above, where the rate is exactly 1.
func settleAccountCreditLeg(bill *Bill, accountCredits []*AccountCredit, grossDiff decimal.Decimal, taxRates []TaxRate, baseCurrency string, x *Exchanger, now time.Time) (decimal.Decimal, []AccountCreditSettlement, error) {
	settlements, err := SettleAccountCredits(accountCredits, grossDiff, baseCurrency, x, now)
	if err != nil {
		return decimal.Zero, nil, err
	}
	for _, s := range settlements {
		invRate := dOne
		if s.AccountCredit.InvoiceExchangeRate != nil {
			invRate = *s.AccountCredit.InvoiceExchangeRate
		}
		invoiceAmount := scaleHalfUp(invRate.Mul(s.Settled))
		gross := CalculateGrossAmount(invoiceAmount, taxRates)
		amt := scaleHalfUp(s.Settled)
		rate := invRate
		bill.AppliedAccountCredits = append(bill.AppliedAccountCredits, AppliedAccountCredit{
			AccountCreditID: s.AccountCredit.ID,
			Currency:        s.AccountCredit.Currency,
			ExchangeRate:    &rate,
			Amount:          &amt,
			InvoiceCurrency: s.AccountCredit.InvoiceCurrency,
			GrossAmount:     &gross,
		})
		grossDiff = grossDiff.Sub(s.Settled)
	}
	return grossDiff, settlements, nil
}

// ApplyPaidCollectOnBill appends one
// AppliedCollectedCredit for a completed payment, then mark the bill PAID if nothing
// is left unpaid. The collected amount uses the txn's OWN frozen exchange rate
// (NOT a live lookup) and is NOT re-rounded; grossAmount/exchangeRate are copied off
// the txn.
func ApplyPaidCollectOnBill(bill *Bill, ct *CollectTransaction, baseCurrency string) *Bill {
	if bill.CollectedAmounts == nil {
		bill.CollectedAmounts = []AppliedCollectedCredit{}
	}
	amt := collectAmountToProduct(ct, baseCurrency)
	bill.CollectedAmounts = append(bill.CollectedAmounts, AppliedCollectedCredit{
		CollectTransactionID: ct.ID,
		Amount:               &amt,
		Currency:             baseCurrency,
		ExchangeRate:         ct.ExchangeRate,
		GrossAmount:          ct.GrossAmount,
		InvoiceCurrency:      ct.Currency,
	})
	if GetUnpaidAmountBillProductCurrency(bill).Cmp(decimal.Zero) <= 0 {
		bill.Status = BillStatusPaid
	}
	return bill
}

// collectAmountToProduct converts the collect amount to the product currency:
// identity when the txn currency is the base, else amount ÷ the
// txn's OWN stored exchangeRate (HALF_UP at the amount's scale). No live rate lookup.
func collectAmountToProduct(ct *CollectTransaction, baseCurrency string) decimal.Decimal {
	amt := decimal.Zero
	if ct.Amount != nil {
		amt = *ct.Amount
	}
	if baseCurrency == ct.Currency {
		return amt
	}
	rate := decimal.Zero
	if ct.ExchangeRate != nil {
		rate = *ct.ExchangeRate
	}
	return divHalfUpScale(amt, rate)
}

// GetNetAmountBillProductCurrencyWithAdjustments returns Σ item nets + Σ adjustment amounts.
func GetNetAmountBillProductCurrencyWithAdjustments(bill *Bill) decimal.Decimal {
	total := decimal.Zero
	for i := range bill.Items {
		total = total.Add(bill.Items[i].NetAmount)
	}
	for _, a := range bill.Adjustments {
		if a.Amount != nil {
			total = total.Add(*a.Amount)
		}
	}
	return total
}

// GetUnpaidAmountBillProductCurrency returns
// net(+adjustments) minus every applied bucket's product-currency Amount, scaleHalfUp (2dp).
func GetUnpaidAmountBillProductCurrency(bill *Bill) decimal.Decimal {
	total := GetNetAmountBillProductCurrencyWithAdjustments(bill)
	applied := decimal.Zero
	for _, a := range bill.AppliedAccountCredits {
		if a.Amount != nil {
			applied = applied.Add(*a.Amount)
		}
	}
	for _, p := range bill.AppliedPromotionalCredits {
		if p.Amount != nil {
			applied = applied.Add(*p.Amount)
		}
	}
	for _, c := range bill.CollectedAmounts {
		if c.Amount != nil {
			applied = applied.Add(*c.Amount)
		}
	}
	return scaleHalfUp(total.Sub(applied))
}
