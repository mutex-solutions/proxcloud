package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// Repo backs the billingProfile collection (+ reads billingConfiguration for the
// base currency, bill for the restricted check, identityValidation for /validation,
// and the billing-domain list collections for the client list endpoints).
type Repo struct {
	profiles               *pgdoc.Store
	configs                *pgdoc.Store
	bills                  *pgdoc.Store
	validations            *pgdoc.Store
	credits                *pgdoc.Store         // accountCreditTransaction (the txn log)
	acctCredits            *pgdoc.Store         // accountCredit (the spendable balance docs)
	promoCredit            *pgdoc.Store         // promotionalCredit
	collects               *pgdoc.Store         // collectTransaction
	savingPlans            *pgdoc.Store         // savingsPlan
	savingCtrs             *pgdoc.Store         // savingsContract
	priceAdjust            *pgdoc.Store         // priceAdjustmentRule
	suspensions            *pgdoc.Store         // suspension (SuspensionProcess)
	gateways               *pgdoc.Store         // thirdPartyIntegration (payment gateways live here too)
	bankXfers              *pgdoc.Store         // bankTransfer (manual bank-transfer deposits)
	cards                  *pgdoc.Store         // creditCard (saved cards)
	cardTxns               *pgdoc.Store         // creditCardTransaction
	promoCodes             *pgdoc.Store         // promotionCode (admin-created codes)
	promoRedeem            *pgdoc.Store         // promotionCodeRedemption (per-org redemption record)
	reminders              *pgdoc.Store         // reminderNotification (savings-contract expiry reminders)
	counters               *pgdoc.Store         // counters (invoice-number sequences, one doc per year)
	enc                    *textcrypt.Encryptor // OPTIONAL, nil-safe: decrypts payment-gateway secrets on read
	reminderOutbox         ReminderOutbox       // opt-in atomic savings reminder producer, never SMTP
	reminderCanceler       ReminderCanceler     // remains available even when producer is OFF
	savingsExpiredOutbox   SavingsExpiredOutbox // fixed expired-notice selection, default OFF
	savingsExpiredCanceler ReminderCanceler     // keyless, independent of producer
}

// SetEncryptor wires the at-rest secret encryptor used to DECRYPT payment-gateway secrets
// (GetGateway). Optional and nil-safe: when unset (e.g. in tests) GetGateway returns the secret
// as-stored, and textcrypt is fail-open so a legacy plaintext value survives either way.
func (r *Repo) SetEncryptor(enc *textcrypt.Encryptor) { r.enc = enc }

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{
		profiles:    db.C("billingProfile"),
		configs:     db.C("billingConfiguration"),
		bills:       db.C("bill"),
		validations: db.C("identityValidation"),
		credits:     db.C("accountCreditTransaction"),
		acctCredits: db.C("accountCredit"),
		promoCredit: db.C("promotionalCredit"),
		collects:    db.C("collectTransaction"),
		savingPlans: db.C("savingsPlan"),
		savingCtrs:  db.C("savingsContract"),
		suspensions: db.C("suspension"),
		gateways:    db.C("thirdPartyIntegration"),
		cards:       db.C("creditCard"),
		cardTxns:    db.C("creditCardTransaction"),
		promoCodes:  db.C("promotionCode"),
		promoRedeem: db.C("promotionCodeRedemption"),
		priceAdjust: db.C("priceAdjustmentRule"),
		reminders:   db.C("reminderNotification"),
		bankXfers:   db.C("bankTransfer"),
		counters:    db.C("counters"),
	}
}

// BankTransferByTxnID looks up a bank transfer by its account-credit-transaction id
// (nil,nil when absent — the caller maps to a 404).
func (r *Repo) BankTransferByTxnID(ctx context.Context, txnID string) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.bankXfers.FindOne(ctx, pgdoc.M{"accountCreditTransactionId": txnID}, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// CreateBankTransfer inserts a PENDING manual bank-transfer record
// and returns its id.
func (r *Repo) CreateBankTransfer(ctx context.Context, doc pgdoc.M) (string, error) {
	return r.bankXfers.InsertOne(ctx, doc)
}

// BankTransferByID loads a bank transfer by id (raw doc, `_id` injected); nil,nil when absent.
func (r *Repo) BankTransferByID(ctx context.Context, id string) (pgdoc.M, error) {
	var doc pgdoc.M
	found, err := r.bankXfers.Get(ctx, id, &doc)
	if err != nil || !found {
		return nil, err
	}
	return doc, nil
}

// ListBankTransfers returns a billing profile's bank transfers, newest first (never nil).
func (r *Repo) ListBankTransfers(ctx context.Context, bpID string) ([]pgdoc.M, error) {
	return r.listRaw(ctx, r.bankXfers, pgdoc.M{"billingProfileId": bpID}, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
}

// UpdateBankTransfer merges set into the transfer doc.
// ResolveBankTransfer moves a PENDING transfer on, once: of an approve, a reject and a customer cancel
// racing each other only one wins. unset names fields to drop. Reports whether this caller won.
func (r *Repo) ResolveBankTransfer(ctx context.Context, id string, set pgdoc.M, unset ...string) (bool, error) {
	return r.bankXfers.SetFieldsOne(ctx, pgdoc.M{"_id": id, "status": "PENDING"}, set, unset)
}

func (r *Repo) UpdateBankTransfer(ctx context.Context, id string, set pgdoc.M) error {
	_, err := r.bankXfers.SetByID(ctx, id, set, nil)
	return err
}

// BankTransferPublic shapes a stored bank transfer for the API: `_id` → `id`, `_class` dropped, and the customer's
// proof file is reduced to its metadata (the bytes stream from the dedicated /proof routes).
// Returns a shallow copy; nil-safe.
func BankTransferPublic(doc pgdoc.M) pgdoc.M {
	if doc == nil {
		return nil
	}
	out := make(pgdoc.M, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	if v, ok := out["_id"]; ok {
		out["id"] = v
		delete(out, "_id")
	}
	delete(out, "_class") // legacy discriminator, not part of the API shape
	if p, ok := out["proof"].(map[string]any); ok {
		out["proof"] = pgdoc.M{"fileName": p["fileName"], "contentType": p["contentType"], "size": p["size"]}
	}
	return out
}

// listRaw returns the matching docs as raw maps (never nil). NOTE: this handles the
// EMPTY-STATE case — under the default seed these collections are empty, so the
// client list endpoints return []. The per-element DTO mapping (BillDto gross/tax/FX,
// credit/transaction/savings shapes) is deferred to later when these get populated; a
// non-empty raw passthrough would NOT match the canonical DTOs and would fail loudly,
// signalling that the DTO work is due.
func (r *Repo) listRaw(ctx context.Context, col *pgdoc.Store, filter pgdoc.M, opts ...pgdoc.FindOpt) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := col.Find(ctx, filter, &out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}

// findTyped decodes the matching docs into a typed slice (money → decimal.Decimal
// via the pgdoc codec). Never nil.
func findTyped[T any](ctx context.Context, col *pgdoc.Store, filter pgdoc.M, opts ...pgdoc.FindOpt) ([]T, error) {
	out := []T{}
	if err := col.Find(ctx, filter, &out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}

// BillsByBillingProfile — lists a profile's bills (→ BillDto via ToBillDto).
func (r *Repo) BillsByBillingProfile(ctx context.Context, bpID string) ([]pricing.Bill, error) {
	return findTyped[pricing.Bill](ctx, r.bills, pgdoc.M{"billingProfileId": bpID})
}

// BillsPage is the keyset-paged variant of BillsByBillingProfile (cursor on _id, newest-first).
func (r *Repo) BillsPage(ctx context.Context, bpID string, p paging.Params) ([]pricing.Bill, *string, *string, error) {
	return paging.Keyset(ctx, r.bills, pgdoc.M{"billingProfileId": bpID}, p, func(b pricing.Bill) string { return b.ID })
}

// SentBills returns a profile's bills in status SENT — the
// collect cron's per-bill work-list.
func (r *Repo) SentBills(ctx context.Context, bpID string) ([]pricing.Bill, error) {
	return findTyped[pricing.Bill](ctx, r.bills, pgdoc.M{"billingProfileId": bpID, "status": string(pricing.BillStatusSent)})
}

// PriceAdjustmentRulesByPricePlanIDs returns the ENABLED price-adjustment rules for the given price
// plans (the rules attached to those plans, filtered to enabled).
func (r *Repo) PriceAdjustmentRulesByPricePlanIDs(ctx context.Context, pricePlanIDs []string) ([]pricing.PriceAdjustmentRule, error) {
	if len(pricePlanIDs) == 0 {
		return nil, nil
	}
	return findTyped[pricing.PriceAdjustmentRule](ctx, r.priceAdjust, pgdoc.M{"pricePlanId": pgdoc.M{"$in": pricePlanIDs}, "enabled": true})
}

// DeleteBill removes a bill by id (the delete-if-zero path when sending a bill). No-op if absent.
func (r *Repo) DeleteBill(ctx context.Context, id string) error {
	_, err := r.bills.DeleteByID(ctx, id)
	return err
}

// AllBills — every bill (platform-wide), for the admin dashboard's current-month
// cost insight across all profiles.
func (r *Repo) AllBills(ctx context.Context) ([]pricing.Bill, error) {
	return findTyped[pricing.Bill](ctx, r.bills, pgdoc.M{})
}

// AllBillingProfiles — every profile. The collect cron fans out over
// every profile (NOT only ACTIVE — the collect job skips SUSPENDED).
func (r *Repo) AllBillingProfiles(ctx context.Context) ([]BillingProfile, error) {
	bps, err := findTyped[BillingProfile](ctx, r.profiles, pgdoc.M{})
	r.fillCurrency(ctx, bps)
	return bps, err
}

// PromotionalCreditsByBillingProfile — all of a profile's promotional credits (the
// remainingAmount>0.01 filter is applied in PromotionalCreditsToDtos).
func (r *Repo) PromotionalCreditsByBillingProfile(ctx context.Context, bpID string) ([]pricing.PromotionalCredit, error) {
	return findTyped[pricing.PromotionalCredit](ctx, r.promoCredit, pgdoc.M{"billingProfileId": bpID})
}

// CollectTransactionsByBillingProfile returns a profile's SUCCESS/FAILED collect transactions,
// newest first. Only the terminal-outcome transactions are returned (PENDING/CANCELLED are
// excluded).
func (r *Repo) CollectTransactionsByBillingProfile(ctx context.Context, bpID string) ([]pricing.CollectTransaction, error) {
	filter := pgdoc.M{
		"billingProfileId": bpID,
		"status":           pgdoc.M{"$in": []string{string(pricing.CollectTransactionStatusSuccess), string(pricing.CollectTransactionStatusFailed)}},
	}
	return findTyped[pricing.CollectTransaction](ctx, r.collects, filter,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
}

// CollectTransactionsPage is the keyset-paged variant of CollectTransactionsByBillingProfile
// (same status filter, cursor on _id / newest-first) → page + next/prev markers.
func (r *Repo) CollectTransactionsPage(ctx context.Context, bpID string, p paging.Params) ([]pricing.CollectTransaction, *string, *string, error) {
	filter := pgdoc.M{
		"billingProfileId": bpID,
		"status":           pgdoc.M{"$in": []string{string(pricing.CollectTransactionStatusSuccess), string(pricing.CollectTransactionStatusFailed)}},
	}
	return paging.Keyset(ctx, r.collects, filter, p, func(t pricing.CollectTransaction) string { return t.ID })
}

// CollectTransactionByID loads one collect transaction by _id (admin by-id read). nil if absent.
func (r *Repo) CollectTransactionByID(ctx context.Context, id string) (*pricing.CollectTransaction, error) {
	var t pricing.CollectTransaction
	found, err := r.collects.Get(ctx, id, &t)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

// SavingsContractsByBillingProfile — a profile's savings contracts.
func (r *Repo) SavingsContractsByBillingProfile(ctx context.Context, bpID string) ([]SavingsContract, error) {
	return findTyped[SavingsContract](ctx, r.savingCtrs, pgdoc.M{"billingProfileId": bpID})
}

// AllSavingsContracts — every savings contract; the expiration cron
// loads them all then filters ACTIVE + endDate<now in the service.
func (r *Repo) AllSavingsContracts(ctx context.Context) ([]SavingsContract, error) {
	return findTyped[SavingsContract](ctx, r.savingCtrs, pgdoc.M{})
}

// SaveSavingsContract persists a mutated contract:
// bumps updatedAt and full-replaces by id (the id column is preserved; the codec
// strips _id from the stored body).
func (r *Repo) SaveSavingsContract(ctx context.Context, c *SavingsContract) error {
	now := time.Now().UTC()
	c.UpdatedAt = &now
	_, err := r.savingCtrs.Replace(ctx, c.ID, c)
	return err
}

// AccountCreditTotal sums the profile's spendable account credits
// (Σ accountCredit.amount). Zero when none.
func (r *Repo) AccountCreditTotal(ctx context.Context, bpID string) (decimal.Decimal, error) {
	credits, err := findTyped[pricing.AccountCredit](ctx, r.acctCredits, pgdoc.M{"billingProfileId": bpID})
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for i := range credits {
		if credits[i].Amount != nil {
			total = total.Add(*credits[i].Amount)
		}
	}
	return total, nil
}

// AvailablePromotionalTotal sums the profile's non-expired promotional credits
// (Σ remainingAmount where expirationDate > now).
func (r *Repo) AvailablePromotionalTotal(ctx context.Context, bpID string, now time.Time) (decimal.Decimal, error) {
	credits, err := findTyped[pricing.PromotionalCredit](ctx, r.promoCredit, pgdoc.M{
		"billingProfileId": bpID,
		"expirationDate":   pgdoc.M{"$gt": now},
	})
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for i := range credits {
		if credits[i].RemainingAmount != nil {
			total = total.Add(*credits[i].RemainingAmount)
		}
	}
	return total, nil
}

// SuspensionConfiguration reads the global billingConfiguration.suspensionConfiguration;
// nil when absent.
func (r *Repo) SuspensionConfiguration(ctx context.Context) (*pricing.BillingAutomaticSuspensionConfig, error) {
	var cfg struct {
		SuspensionConfiguration *pricing.BillingAutomaticSuspensionConfig `json:"suspensionConfiguration"`
	}
	found, err := r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	if err != nil || !found {
		return nil, err
	}
	return cfg.SuspensionConfiguration, nil
}

// AvailableSavingsPlans — the savings plans available to a billing profile.
// The full access-mode SELECTION (public + scoped-to-profile) is a refinement; {available:true}
// matches the seed reality (empty). Returns the typed plans for SavingsPlanToDto.
func (r *Repo) AvailableSavingsPlans(ctx context.Context) ([]SavingsPlan, error) {
	return findTyped[SavingsPlan](ctx, r.savingPlans, pgdoc.M{"available": true})
}

// SavingsContractByID loads one savings contract by id → 404. nil if absent / bad id.
func (r *Repo) SavingsContractByID(ctx context.Context, id string) (*SavingsContract, error) {
	var c SavingsContract
	found, err := r.savingCtrs.Get(ctx, id, &c)
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// ExistsActiveSavingsContract reports whether the profile already holds a contract for the plan, ACTIVE
// or awaiting its payment: paying two of them would stack their discounts.
func (r *Repo) ExistsActiveSavingsContract(ctx context.Context, planID, bpID string) (bool, error) {
	return r.savingCtrs.Exists(ctx, pgdoc.M{
		"savingsPlanId": planID, "billingProfileId": bpID,
		"status": pgdoc.M{"$in": []string{SavingsStatusActive, SavingsStatusPendingPayment}},
	})
}

// AvailableSavingsPlanByID loads a savings plan by id, only when it's marked available.
// nil if absent or not available (the handler maps to 404).
func (r *Repo) AvailableSavingsPlanByID(ctx context.Context, id string) (*SavingsPlan, error) {
	var p SavingsPlan
	found, err := r.savingPlans.FindOne(ctx, pgdoc.M{"_id": id, "available": true}, &p)
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

// AccountCreditTransactionByID loads one account-credit transaction by id.
// Returns nil when missing (handler maps to the interpolated 404) or when the id is
// unknown. Typed (money → decimal); nested invoiceDetails/accountCredit kept raw.
func (r *Repo) AccountCreditTransactionByID(ctx context.Context, id string) (*AccountCreditTransaction, error) {
	var doc AccountCreditTransaction
	found, err := r.credits.Get(ctx, id, &doc)
	if err != nil || !found {
		return nil, err
	}
	return &doc, nil
}

// ListAccountCreditTransactions returns a profile's SUCCESS/FAILED account-credit transactions,
// newest first (same status filter as collect: PENDING/CANCELLED excluded). Typed
// (money → decimal codec).
func (r *Repo) ListAccountCreditTransactions(ctx context.Context, bpID string) ([]AccountCreditTransaction, error) {
	filter := pgdoc.M{
		"billingProfileId": bpID,
		"status":           pgdoc.M{"$in": []string{string(pricing.CollectTransactionStatusSuccess), string(pricing.CollectTransactionStatusFailed)}},
	}
	return findTyped[AccountCreditTransaction](ctx, r.credits, filter,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
}

// ListAccountCreditTransactionsPage is the keyset-paged variant of ListAccountCreditTransactions
// (same status filter, cursor on _id / newest-first).
func (r *Repo) ListAccountCreditTransactionsPage(ctx context.Context, bpID string, p paging.Params) ([]AccountCreditTransaction, *string, *string, error) {
	filter := pgdoc.M{
		"billingProfileId": bpID,
		"status":           pgdoc.M{"$in": []string{string(pricing.CollectTransactionStatusSuccess), string(pricing.CollectTransactionStatusFailed)}},
	}
	return paging.Keyset(ctx, r.credits, filter, p, func(t AccountCreditTransaction) string { return t.ID })
}

// AllAccountCreditTransactionsByProfile — a profile's account-credit transactions, newest first
// (ALL statuses, unlike the client list). Empty for a no-txn profile.
func (r *Repo) AllAccountCreditTransactionsByProfile(ctx context.Context, bpID string) ([]AccountCreditTransaction, error) {
	return findTyped[AccountCreditTransaction](ctx, r.credits, pgdoc.M{"billingProfileId": bpID},
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
}

// AllAccountCreditTransactions — every account-credit transaction (platform-wide), for the admin
// Dashboard MRR payments insight.
func (r *Repo) AllAccountCreditTransactions(ctx context.Context) ([]AccountCreditTransaction, error) {
	return findTyped[AccountCreditTransaction](ctx, r.credits, pgdoc.M{})
}

// AllCollectTransactions — every collect transaction (platform-wide), for the admin Dashboard
// current-month payments insight.
func (r *Repo) AllCollectTransactions(ctx context.Context) ([]pricing.CollectTransaction, error) {
	return findTyped[pricing.CollectTransaction](ctx, r.collects, pgdoc.M{})
}

// AllCollectTransactionsOffset / AllAccountCreditTransactionsOffset — offset-paged platform-wide
// ledgers (newest-first, + total) for the admin global transaction tables (numbered pages, like the
// other admin lists).
func (r *Repo) AllCollectTransactionsOffset(ctx context.Context, filter pgdoc.M, p paging.Params) ([]pricing.CollectTransaction, int64, error) {
	return paging.Offset[pricing.CollectTransaction](ctx, r.collects, filter, nil, p)
}

func (r *Repo) AllAccountCreditTransactionsOffset(ctx context.Context, filter pgdoc.M, p paging.Params) ([]AccountCreditTransaction, int64, error) {
	return paging.Offset[AccountCreditTransaction](ctx, r.credits, filter, nil, p)
}

// AllCollectTransactionsByProfile — all statuses, createdAt DESC.
func (r *Repo) AllCollectTransactionsByProfile(ctx context.Context, bpID string) ([]pricing.CollectTransaction, error) {
	return findTyped[pricing.CollectTransaction](ctx, r.collects, pgdoc.M{"billingProfileId": bpID},
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
}

// CreditCardTransactionsByProfile — createdAt DESC.
func (r *Repo) CreditCardTransactionsByProfile(ctx context.Context, bpID string) ([]CreditCardTransaction, error) {
	return findTyped[CreditCardTransaction](ctx, r.cardTxns, pgdoc.M{"billingProfileId": bpID},
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
}

// CountSentBills counts a profile's bills in status SENT
// (for ProjectStats.unpaidBills). 0 for a fresh profile.
func (r *Repo) CountSentBills(ctx context.Context, bpID string) (int64, error) {
	return r.bills.Count(ctx, pgdoc.M{"billingProfileId": bpID, "status": "SENT"})
}

// HasOverdueBills reports whether the profile has a SENT bill past its due date (dueAt < now) —
// the same date the invoice and the bill mail print.
func (r *Repo) HasOverdueBills(ctx context.Context, bpID string) (bool, error) {
	return r.bills.Exists(ctx, pgdoc.M{
		"billingProfileId": bpID,
		"status":           "SENT",
		"dueAt":            pgdoc.M{"$lt": time.Now().UTC()},
	})
}

// GetIdentityValidationByID loads an identity validation by id.
// Returns (nil,nil) when absent so the caller can
// emit the 404 "The account validation with id %s was not found. ".
func (r *Repo) GetIdentityValidationByID(ctx context.Context, id string) (*IdentityValidation, error) {
	var v IdentityValidation
	found, err := r.validations.Get(ctx, id, &v)
	if err != nil || !found {
		return nil, err
	}
	return &v, nil
}

// CreateForOrganization creates a fresh BillingProfile for an org:
// status NEW, owner email/name, a
// single contact (full name), empty customInfo/verifications, a default
// pricePlanConfig, and the base currency. Returns the new profile id.
func (r *Repo) CreateForOrganization(ctx context.Context, orgID string, owner Owner) (string, error) {
	now := time.Now().UTC()
	currency, _ := r.BaseCurrency(ctx) // null/"" when unconfigured
	bp := &BillingProfile{
		OrganizationID:  orgID,
		Sub:             owner.Sub,
		Status:          StatusNew,
		Email:           owner.Email,
		FirstName:       owner.FirstName,
		LastName:        owner.LastName,
		Currency:        currency,
		CustomInfo:      map[string]any{},
		Verifications:   []any{},
		PricePlanConfig: &PricePlanConfiguration{PricePlanIDs: []string{}, IncludePublicPricePlans: true},
		Contacts:        []Contact{{Name: owner.FullName, Email: owner.Email}},
		CreatedAt:       &now,
		UpdatedAt:       &now,
	}
	return r.profiles.InsertOne(ctx, bp)
}

// Delete removes a billing profile by id. No-op if absent.
func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.profiles.DeleteByID(ctx, id)
	return err
}

// Update persists a mutated profile: bumps
// updatedAt and full-replaces the document by id (the codec strips _id from the
// stored body; the id column is preserved).
func (r *Repo) Update(ctx context.Context, bp *BillingProfile) (*BillingProfile, error) {
	now := time.Now().UTC()
	bp.UpdatedAt = &now
	if _, err := r.profiles.Replace(ctx, bp.ID, bp); err != nil {
		return nil, err
	}
	return bp, nil
}

// autoActivationFlow reads billingConfiguration.autoActivationFlow;
// nil when the subdoc is absent
// (→ auto-activation is not configured).
func (r *Repo) autoActivationFlow(ctx context.Context) (*AutoActivationFlow, error) {
	var cfg struct {
		AutoActivationFlow *AutoActivationFlow `json:"autoActivationFlow"`
	}
	found, err := r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	if err != nil || !found {
		return nil, err
	}
	return cfg.AutoActivationFlow, nil
}

// ProvisioningPromotional is a provisioning promotional-credit config entry ({amount, daysValidity}).
type ProvisioningPromotional struct {
	Amount       decimal.Decimal `json:"amount"`
	DaysValidity int             `json:"daysValidity"`
}

// ProvisioningPromotionals reads billingConfiguration.provisioningSettings.promotionals — the
// promotional credits minted for every newly-activated profile. Empty when unconfigured.
func (r *Repo) ProvisioningPromotionals(ctx context.Context) ([]ProvisioningPromotional, error) {
	var cfg struct {
		ProvisioningSettings struct {
			Promotionals []ProvisioningPromotional `json:"promotionals"`
		} `json:"provisioningSettings"`
	}
	found, err := r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	if err != nil || !found {
		return nil, err
	}
	return cfg.ProvisioningSettings.Promotionals, nil
}

func (r *Repo) FindByID(ctx context.Context, id string) (*BillingProfile, error) {
	var bp BillingProfile
	found, err := r.profiles.Get(ctx, id, &bp)
	if err != nil || !found {
		return nil, err
	}
	one := []BillingProfile{bp}
	r.fillCurrency(ctx, one)
	return &one[0], nil
}

// FindByStatus returns every billing profile in the given status —
// the charge cron loads the ACTIVE profiles.
func (r *Repo) FindByStatus(ctx context.Context, status string) ([]BillingProfile, error) {
	bps, err := findTyped[BillingProfile](ctx, r.profiles, pgdoc.M{"status": status})
	r.fillCurrency(ctx, bps)
	return bps, err
}

// fillCurrency backfills an ACTIVE profile with no currency: an operator could activate a profile
// before it had billing details (activation now sets it), and every bill read of such a profile
// failed on the blank currency. It bills in the base currency, as saving its details would have
// set, filled in memory and written once where the stored currency is still blank. A failed write
// only leaves the fill to the next read.
func (r *Repo) fillCurrency(ctx context.Context, bps []BillingProfile) {
	base := ""
	for i := range bps {
		bp := &bps[i]
		if bp.Status != StatusActive || bp.Currency != "" {
			continue
		}
		if base == "" {
			b, err := r.BaseCurrency(ctx)
			if err != nil || b == "" {
				return
			}
			base = b
		}
		bp.Currency = base
		if _, err := r.profiles.SetFieldsOne(ctx, pgdoc.M{"_id": bp.ID, "status": StatusActive,
			"$or": []pgdoc.M{{"currency": pgdoc.M{"$exists": false}}, {"currency": ""}}},
			pgdoc.M{"currency": base}, nil); err != nil {
			slog.Warn("billing profile currency backfill", "billingProfile", bp.ID, "err", err)
		}
	}
}

// Exists reports whether a billing profile with the given _id exists
// (used by the affiliate cfy check). A malformed
// id is treated as "not found" (no error).
func (r *Repo) Exists(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	return r.profiles.Exists(ctx, pgdoc.M{"_id": id})
}

// IsActive reports whether a billing profile is ACTIVE (used by project create
// to decide initial project status).
func (r *Repo) IsActive(ctx context.Context, id string) (bool, error) {
	bp, err := r.FindByID(ctx, id)
	if err != nil || bp == nil {
		return false, err
	}
	return bp.Status == StatusActive, nil
}

// BaseCurrency reads billingConfiguration.baseCurrency,
// or "" when no config exists.
func (r *Repo) BaseCurrency(ctx context.Context) (string, error) {
	bc, _, _, err := r.Configuration(ctx)
	return bc, err
}

// TimeUnitLimits reads billingConfiguration.settings.timeUnitLimits —
// the per-time-unit "size in month" overrides the pricing
// preview's monthly-price conversion applies. Nil/empty when unset, in
// which case the per-unit defaults (minute=43200, hour=720, month=1) apply.
func (r *Repo) TimeUnitLimits(ctx context.Context) (map[string]int, error) {
	var cfg struct {
		Settings struct {
			TimeUnitLimits map[string]int `json:"timeUnitLimits"`
		} `json:"settings"`
	}
	found, err := r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	if err != nil || !found {
		return nil, err
	}
	return cfg.Settings.TimeUnitLimits, nil
}

// BillingConfigurationAdminDto is the raw BillingConfiguration domain as the admin endpoints
// serialize it (the whole document). The typed fields are the ones handler code
// reads; every other stored field (suspensionConfiguration, settings, savingsContractNotification
// Config, name, invoiceGatewayId, …) rides along via the inline Extra map so the admin UI can
// round-trip the FULL shape — its update replaces the whole doc, so a partial read would wipe
// the unexposed fields on save.
type BillingConfigurationAdminDto struct {
	ID                    string         `json:"id"`
	BaseCurrency          string         `json:"baseCurrency,omitempty"`
	DefaultConfiguration  bool           `json:"defaultConfiguration"`
	PromotionCodesEnabled bool           `json:"promotionCodesEnabled"`
	Extra                 map[string]any `json:"-"`
}

// MarshalJSON merges the typed fields with the inline extras (typed fields win) so the wire
// shape is the full document with `_id` renamed to `id` and `_class` never emitted.
func (d BillingConfigurationAdminDto) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	for k, v := range d.Extra {
		if k == "_id" || k == "_class" || k == "id" {
			continue
		}
		out[k] = v
	}
	out["id"] = d.ID
	if d.BaseCurrency != "" {
		out["baseCurrency"] = d.BaseCurrency
	}
	out["defaultConfiguration"] = d.DefaultConfiguration
	out["promotionCodesEnabled"] = d.PromotionCodesEnabled
	return json.Marshal(out)
}

// UnmarshalJSON decodes the stored document: the typed fields are pulled out and every OTHER key
// is collected into Extra, so a read→modify→save round-trips the unexposed fields (the storage
// codec is plain JSON, which — unlike the old inline decode — does not populate a catch-all map).
func (d *BillingConfigurationAdminDto) UnmarshalJSON(b []byte) error {
	var all map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&all); err != nil {
		return err
	}
	if v, ok := all["id"].(string); ok {
		d.ID = v
	}
	if v, ok := all["baseCurrency"].(string); ok {
		d.BaseCurrency = v
	}
	if v, ok := all["defaultConfiguration"].(bool); ok {
		d.DefaultConfiguration = v
	}
	if v, ok := all["promotionCodesEnabled"].(bool); ok {
		d.PromotionCodesEnabled = v
	}
	delete(all, "baseCurrency")
	delete(all, "defaultConfiguration")
	delete(all, "promotionCodesEnabled")
	d.Extra = all
	return nil
}

// AllBillingConfigurations lists every billing configuration.
func (r *Repo) AllBillingConfigurations(ctx context.Context) ([]BillingConfigurationAdminDto, error) {
	return findTyped[BillingConfigurationAdminDto](ctx, r.configs, pgdoc.M{})
}

// CurrentBillingConfiguration returns the
// default config (else the first), or (nil,nil) when none.
func (r *Repo) CurrentBillingConfiguration(ctx context.Context) (*BillingConfigurationAdminDto, error) {
	var cfg BillingConfigurationAdminDto
	found, err := r.configs.FindOne(ctx, pgdoc.M{"defaultConfiguration": true}, &cfg)
	if err != nil {
		return nil, err
	}
	if !found {
		found, err = r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
		if err != nil || !found {
			return nil, err
		}
	}
	return &cfg, nil
}

// BillingConfigurationByID loads one billing configuration by id; (nil,nil) when absent (the
// caller maps that to the 400 "Billing configuration not found ").
func (r *Repo) BillingConfigurationByID(ctx context.Context, id string) (*BillingConfigurationAdminDto, error) {
	var cfg BillingConfigurationAdminDto
	found, err := r.configs.Get(ctx, id, &cfg)
	if err != nil || !found {
		return nil, err
	}
	return &cfg, nil
}

// Configuration reads the single billingConfiguration doc.
// found=false when none
// exists (the caller maps that to the 400 "billing not configured").
func (r *Repo) Configuration(ctx context.Context) (baseCurrency string, promotionCodesEnabled *bool, found bool, err error) {
	var cfg struct {
		BaseCurrency          string `json:"baseCurrency"`
		PromotionCodesEnabled *bool  `json:"promotionCodesEnabled"`
	}
	found, err = r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	if err != nil || !found {
		return "", nil, false, err
	}
	return cfg.BaseCurrency, cfg.PromotionCodesEnabled, true, nil
}
