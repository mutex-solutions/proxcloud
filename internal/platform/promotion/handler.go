// Package promotion serves the client promotion endpoints:
// GET /api/v1/promotion/deposit (deposit-promotion config) and
// POST /api/v1/promotion/{billingProfileId}/code (redeem a promo code → PromotionalCredit).
package promotion

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// BillingProfileResolver resolves the org owning a billing profile WHILE enforcing that the caller
// (userSub) is a member of it — the membership gate applied before redeem() runs. It returns the
// resolved org id + billing-profile id, or an error (a non-member yields the membership 404 from
// the underlying org lookup). Injected as a closure (main.go) so promotion doesn't import org.
type BillingProfileResolver func(ctx context.Context, billingProfileID, userSub string) (orgID, resolvedBPID string, err error)

type Handler struct {
	billing *billing.Repo
	resolve BillingProfileResolver
}

func NewHandler(b *billing.Repo, resolve BillingProfileResolver) *Handler {
	return &Handler{billing: b, resolve: resolve}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/promotion/deposit", h.deposit)
	r.Post("/promotion/{billingProfileId}/code", h.redeem)
}

// depositPromotion is the deposit-promotion response: {enabled} always, plus
// {ratio, daysValid} only when enabled (omit when absent).
type depositPromotion struct {
	Enabled   bool     `json:"enabled"`
	Ratio     *float64 `json:"ratio,omitempty"`
	DaysValid *int     `json:"daysValid,omitempty"`
}

// deposit returns the deposit-promotion config. Disabled by default (the unconfigured state).
func (h *Handler) deposit(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, depositPromotion{Enabled: false})
}

// redeem redeems a promo code:
// membership-resolve the bp + its org (the authz gate) → promo-codes-enabled guard →
// getRedeemableByCode (case-insensitive + ACTIVE/not-DISABLED + in-window + org-targeted) →
// record the redemption (refused when the org has one) and mint its PromotionalCredit, in one transaction →
// single(credit).
func (h *Handler) redeem(w http.ResponseWriter, r *http.Request) {
	bpID := chi.URLParam(r, "billingProfileId")
	code := strings.TrimSpace(r.URL.Query().Get("code"))

	// Authz: resolve the bp's org WITH a membership check on the caller (a non-member must NOT be
	// able to mint a credit on another org's profile). The resolver returns the membership-404 when
	// the caller is not a member.
	orgID, resolvedBPID, err := h.resolve(r.Context(), bpID, httpx.RC(r.Context()).Sub)
	if httpx.WriteError(w, err) {
		return
	}

	// promotionCodesEnabled (billingConfiguration): null/true → enabled, false → disabled.
	_, enabled, _, err := h.billing.Configuration(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	if enabled != nil && !*enabled {
		httpx.WriteError(w, httpx.BadRequest("Promotion codes are disabled on this platform"))
		return
	}

	if orgID == "" {
		httpx.WriteError(w, httpx.BadRequest("Billing profile is not attached to an organization"))
		return
	}

	pc, err := h.billing.FindPromotionCodeByCode(r.Context(), code)
	if httpx.WriteError(w, err) {
		return
	}
	if herr := validateRedeemable(code, pc, orgID); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	codeIDHex, _ := pc["_id"].(string) // pgdoc injects _id as a plain string

	now := time.Now().UTC()
	amount := promoAmount(pc)
	credit := &pricing.PromotionalCredit{
		BillingProfileID: resolvedBPID,
		Code:             code,
		InitialAmount:    &amount,
		RemainingAmount:  &amount,
		CreatedAt:        &now,
		UpdatedAt:        &now,
		ExpirationDate:   promoExpiry(pc, now),
	}
	saved, err := h.billing.RedeemPromotion(r.Context(), credit, codeIDHex, orgID, "USER_REDEEM", now)
	if errors.Is(err, billing.ErrPromotionRedeemed) {
		err = httpx.BadRequest("This promotion code has already been redeemed by your organization")
	}
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, saved)
}

// validateRedeemable checks a redeemable promo code, with these status codes + strings:
//   - blank code / code-not-found → 400 "Invalid code. " (the INVALID_CODE translation, trailing space)
//   - status DISABLED → 400 "This promotion code is disabled" (ONLY DISABLED is rejected — an
//     ACTIVE/absent status passes; other values are NOT rejected as "not active")
//   - before validFrom → 400 "This promotion code is not yet active"
//   - after validUntil → 400 "This promotion code has expired"
//   - targetOrganizationIds non-empty and not containing the org → 400 "This promotion code cannot be
//     redeemed by your organization"
//
// (A blank/missing code 400s; the not-found is the SAME 400 INVALID_CODE, NOT a 404.)
func validateRedeemable(code string, pc pgdoc.M, organizationID string) *httpx.HTTPError {
	if strings.TrimSpace(code) == "" || pc == nil {
		return httpx.BadRequest("Invalid code. ")
	}
	if status, _ := pc["status"].(string); strings.EqualFold(status, "DISABLED") {
		return httpx.BadRequest("This promotion code is disabled")
	}
	now := time.Now().UTC()
	if from := promoTime(pc["validFrom"]); from != nil && now.Before(*from) {
		return httpx.BadRequest("This promotion code is not yet active")
	}
	if until := promoTime(pc["validUntil"]); until != nil && now.After(*until) {
		return httpx.BadRequest("This promotion code has expired")
	}
	if targets := promoStrings(pc["targetOrganizationIds"]); len(targets) > 0 {
		found := false
		for _, t := range targets {
			if t == organizationID {
				found = true
				break
			}
		}
		if !found {
			return httpx.BadRequest("This promotion code cannot be redeemed by your organization")
		}
	}
	return nil
}

// promoAmount reads the code's amount (a decimal string in jsonb) as a decimal.Decimal (0 on absence).
func promoAmount(pc pgdoc.M) decimal.Decimal {
	switch v := pc["amount"].(type) {
	case decimal.Decimal:
		d, err := decimal.NewFromString(v.String())
		if err == nil {
			return d
		}
	case float64:
		return decimal.NewFromFloat(v)
	}
	return decimal.Zero
}

// noExpiration is a far-future sentinel so a never-expires credit still satisfies the
// `expirationDate > now` availability filter (a null expiry is never stored).
var noExpiration = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// promoExpiry computes the credit's expiry from the code's creditValidityDuration:
// `duration == null ? NO_EXPIRATION : now.plus(duration)`.
//
// creditValidityDuration is a duration persisted as an ISO-8601 STRING ("PT720H", "P30D", …) —
// NOT a {value,unit} object. We therefore parse the ISO-8601 string first; absent/unparseable/
// non-positive → the NO_EXPIRATION sentinel (9999-01-01, never nil — so the credit still passes
// the `expirationDate > now` availability filter). The legacy {value,unit} map and a
// bare-number-of-days are kept as defensive fallbacks for any oddly-shaped stored doc.
func promoExpiry(pc pgdoc.M, now time.Time) *time.Time {
	switch v := pc["creditValidityDuration"].(type) {
	case string:
		if d, ok := parseISO8601Duration(v); ok && d > 0 {
			exp := now.Add(d)
			return &exp
		}
	case map[string]any:
		if exp, ok := legacyValueUnitExpiry(v, now); ok {
			return exp
		}
	case int32:
		if v > 0 {
			exp := now.AddDate(0, 0, int(v))
			return &exp
		}
	case int64:
		if v > 0 {
			exp := now.AddDate(0, 0, int(v))
			return &exp
		}
	case float64:
		if v > 0 {
			exp := now.AddDate(0, 0, int(v))
			return &exp
		}
	}
	return &noExpiration
}

// legacyValueUnitExpiry handles a legacy {value,unit} creditValidityDuration shape (defensive only).
// Returns (nil,false) when the value is non-positive → caller uses NO_EXPIRATION.
func legacyValueUnitExpiry(d map[string]any, now time.Time) (*time.Time, bool) {
	val := 0
	switch v := d["value"].(type) {
	case int32:
		val = int(v)
	case int64:
		val = int(v)
	case float64:
		val = int(v)
	}
	if val <= 0 {
		return nil, false
	}
	var exp time.Time
	switch strings.ToUpper(toStr(d["unit"])) {
	case "MONTHS", "MONTH":
		exp = now.AddDate(0, val, 0)
	case "YEARS", "YEAR":
		exp = now.AddDate(val, 0, 0)
	default: // DAYS
		exp = now.AddDate(0, 0, val)
	}
	return &exp, true
}

func toStr(v any) string { s, _ := v.(string); return s }

// parseISO8601Duration parses the ISO-8601 duration form `PnDTnHnMnS` (time-based
// only: days/hours/minutes/seconds — no years/months/weeks) into a time.Duration. Returns
// ok=false on any malformed input or an empty (no-component) duration. Case-insensitive; fractional
// second components are allowed.
func parseISO8601Duration(s string) (time.Duration, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "P") {
		return 0, false
	}
	s = s[1:]
	datePart, timePart := s, ""
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		datePart, timePart = s[:i], s[i+1:]
	}
	var total time.Duration
	matched := false
	if datePart != "" {
		if !strings.HasSuffix(datePart, "D") {
			return 0, false
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(datePart, "D"), 64)
		if err != nil {
			return 0, false
		}
		total += time.Duration(n * 24 * float64(time.Hour))
		matched = true
	}
	for timePart != "" {
		j := 0
		for j < len(timePart) && (timePart[j] == '.' || timePart[j] == '-' || (timePart[j] >= '0' && timePart[j] <= '9')) {
			j++
		}
		if j == 0 || j >= len(timePart) {
			return 0, false
		}
		n, err := strconv.ParseFloat(timePart[:j], 64)
		if err != nil {
			return 0, false
		}
		switch timePart[j] {
		case 'H':
			total += time.Duration(n * float64(time.Hour))
		case 'M':
			total += time.Duration(n * float64(time.Minute))
		case 'S':
			total += time.Duration(n * float64(time.Second))
		default:
			return 0, false
		}
		matched = true
		timePart = timePart[j+1:]
	}
	return total, matched
}

// promoTime coerces a validFrom/validUntil bound to a time. The window guard was silently skipped
// when the bound was stored as an ISO-8601 STRING or an epoch number (promoTime returned nil → no
// bound → redeemable outside its window); parse those shapes too so the window is enforced.
func promoTime(v any) *time.Time {
	switch t := v.(type) {
	case time.Time:
		tt := t.UTC()
		return &tt
	case string:
		return parsePromoTimeString(t)
	case int64:
		return epochToTime(t)
	case int32:
		return epochToTime(int64(t))
	case float64:
		return epochToTime(int64(t))
	}
	return nil
}

// parsePromoTimeString parses a bound stored as a string — an RFC3339 / date-only timestamp, or an
// epoch (seconds or millis) rendered as digits. Nil when blank/unparseable (treated as "no bound",
// the same as the pre-existing nil behaviour for an unrecognised shape).
func parsePromoTimeString(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if tt, err := time.Parse(layout, s); err == nil {
			tt = tt.UTC()
			return &tt
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return epochToTime(n)
	}
	return nil
}

// epochToTime interprets n as epoch millis when it is large enough to be a millisecond timestamp
// (|n| ≥ 1e12, i.e. beyond ~2001 when read as seconds), else as epoch seconds.
func epochToTime(n int64) *time.Time {
	var tt time.Time
	if n >= 1_000_000_000_000 || n <= -1_000_000_000_000 {
		tt = time.UnixMilli(n).UTC()
	} else {
		tt = time.Unix(n, 0).UTC()
	}
	return &tt
}

func promoStrings(v any) []string {
	var arr []any
	switch a := v.(type) {
	case []any:
		arr = a
	default:
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
