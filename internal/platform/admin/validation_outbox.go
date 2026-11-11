package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const validationDeliveryMode = "validation-approved-outbox/v1"

// ValidationApprovalRequest is the fixed preparation seam: no document/KYC data,
// arbitrary variables, template selection or activation/sender capability.
type ValidationApprovalRequest struct {
	ValidationID, ApprovalGeneration string
	Date, ExpiresAt                  time.Time
	Recipients                       []string
	FullName                         string
}

func (ValidationApprovalRequest) String() string   { return "[validation preparation redacted]" }
func (ValidationApprovalRequest) GoString() string { return "[validation preparation redacted]" }
func (ValidationApprovalRequest) LogValue() slog.Value {
	return slog.StringValue("[validation preparation redacted]")
}

// Narrow private test seam; SetActivation wires the existing production service.
// The decision helper never receives this capability.
type validationActivationTail interface {
	Activate(context.Context, *billing.BillingProfile, string) error
	NotifyValidation(context.Context, *billing.BillingProfile)
}

type ValidationApprovalOutbox interface {
	EnqueueValidationTx(context.Context, ValidationApprovalRequest) (mailoutbox.QueueResult, error)
}
type validationCanceler interface {
	CancelPendingTx(context.Context, []string) error
}

func (h *Handler) SetValidationOutbox(q ValidationApprovalOutbox) { h.validationOutbox = q }

// Always wire, including producer-OFF/keyless startup. Cancellation uses IDs only.
func (h *Handler) SetValidationCanceler(q validationCanceler) { h.validationCanceler = q }

// The marker is sticky across revocation, OFF approvals and reapproval. Old
// generation events/receipts remain immutable in the outbox; FirstGeneration and
// PriorGenerations preserve history even when the current snapshot is superseded.
type validationDelivery struct {
	Mode             string    `json:"mode"`
	Generation       string    `json:"generation"`
	FirstGeneration  string    `json:"firstGeneration"`
	PriorGenerations int       `json:"priorGenerations"`
	Date             time.Time `json:"date"`
	ExpiresAt        time.Time `json:"expiresAt"`
	IDs              []string  `json:"outboxIds"`
	Revoked          bool      `json:"revoked"`
}

func readValidationDelivery(doc pgdoc.M) (*validationDelivery, error) {
	value, ok := doc["approvalDelivery"]
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("validation delivery decision invalid")
	}
	var d validationDelivery
	if json.Unmarshal(raw, &d) != nil || d.Mode != validationDeliveryMode || d.Date.IsZero() || !d.ExpiresAt.Equal(d.Date.Add(mailoutbox.ValidationApprovalMaxAge)) || len(d.IDs) < 1 || len(d.IDs) > mailoutbox.MaxRecipients || d.PriorGenerations < 0 {
		return nil, errors.New("validation delivery decision invalid")
	}
	for _, v := range append([]string{d.Generation, d.FirstGeneration}, d.IDs...) {
		u, e := uuid.Parse(v)
		if e != nil || u == uuid.Nil || u.String() != v {
			return nil, errors.New("validation delivery decision invalid")
		}
	}
	return &d, nil
}

type validationStatusDecision struct {
	doc            pgdoc.M
	profileID      string
	suppressLegacy bool
}

// decideValidationStatus owns the one short PRIMARY RC transaction. Lock order:
// billingProfileValidation -> referenced billingProfile -> outbox event/rows.
// Neither Activate, NotifyValidation, SMTP nor cloud callbacks are reachable here.
func (h *Handler) decideValidationStatus(ctx context.Context, id, status string) (validationStatusDecision, error) {
	var result validationStatusDecision
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return result, err
	}
	if !isValidValidationStatus(status) {
		return result, httpx.BadRequest(fmt.Sprintf("Invalid validation status %s", status))
	}
	db := h.repo.db
	// Missing optional stores must never abort a decision transaction that would
	// otherwise record an explicit non-delivery. ON also prepares these at startup.
	for _, name := range []string{billingProfileValidationCollection, billingProfileCollection} {
		if err := db.C(name).Ensure(ctx); err != nil {
			return result, errors.New("validation decision store unavailable")
		}
	}
	err := db.WithTx(ctx, func(tc context.Context) error {
		tx, err := db.Tx(tc)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(tc, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			return err
		}
		if err = pgdoc.RequirePrimary(tc, tx); err != nil {
			return err
		}
		var raw []byte
		if err = tx.QueryRow(tc, `SELECT doc FROM "billingProfileValidation" WHERE id=$1 FOR UPDATE`, id).Scan(&raw); errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("Billing profile validation not found.")
		} else if err != nil {
			return err
		}
		var doc pgdoc.M
		if pgdoc.Unmarshal(raw, id, &doc) != nil {
			return errors.New("validation document invalid")
		}
		oldStatus, _ := doc["status"].(string)
		bpID, _ := doc["billingProfileId"].(string)
		d, err := readValidationDelivery(doc)
		if err != nil {
			return err
		}
		// Decode only the locked current profile. No pre-lock profile/map can
		// select recipients or decide which business document the mail concerns.
		var bp billing.BillingProfile
		profileFound := true
		err = tx.QueryRow(tc, `SELECT doc FROM "billingProfile" WHERE id=$1 FOR UPDATE`, bpID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			profileFound = false
		} else if err != nil {
			return err
		} else if pgdoc.Unmarshal(raw, bpID, &bp) != nil {
			return errors.New("validation billing profile invalid")
		}
		if status == "APPROVED" && oldStatus != "APPROVED" && h.validationOutbox != nil && !profileFound {
			return httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", bpID))
		}
		if status != "APPROVED" && d != nil {
			if h.validationCanceler == nil {
				return errors.New("validation cancellation unavailable")
			}
			for i := 0; i < len(d.IDs); i += mailoutbox.MaxRecipients {
				if err = h.validationCanceler.CancelPendingTx(tc, d.IDs[i:min(i+mailoutbox.MaxRecipients, len(d.IDs))]); err != nil {
					return errors.New("validation cancellation failed")
				}
			}
			d.Revoked = true
			doc["approvalDelivery"] = d
		}
		// Already APPROVED is NEVER adopted/backfilled. A durable marker returns
		// its original generation without render/enqueue; OFF-history never falls
		// back to legacy mail, nor manufactures an unencrypted mail decision.
		if status == "APPROVED" && oldStatus != "APPROVED" && h.validationOutbox != nil {
			var now time.Time
			if err = tx.QueryRow(tc, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				return err
			}
			generation := uuid.NewString()
			expiry := now.Add(mailoutbox.ValidationApprovalMaxAge)
			q, err := h.validationOutbox.EnqueueValidationTx(tc, ValidationApprovalRequest{ValidationID: id, ApprovalGeneration: generation, Date: now, ExpiresAt: expiry, Recipients: billing.Recipients(&bp), FullName: validationProfileName(&bp)})
			if err != nil {
				return errors.New("validation approval preparation failed")
			}
			if len(q.IDs) < 1 || len(q.IDs) > mailoutbox.MaxRecipients {
				return errors.New("validation approval queue result invalid")
			}
			next := &validationDelivery{Mode: validationDeliveryMode, Generation: generation, FirstGeneration: generation, Date: now, ExpiresAt: expiry, IDs: q.IDs}
			if d != nil {
				next.FirstGeneration = d.FirstGeneration
				next.PriorGenerations = d.PriorGenerations + 1
			}
			d = next
			doc["approvalDelivery"] = d
			if _, err = readValidationDelivery(doc); err != nil {
				return err
			}
		}
		doc["status"] = status
		body, _, err := pgdoc.Marshal(doc)
		if err != nil {
			return errors.New("validation decision encoding failed")
		}
		tag, err := tx.Exec(tc, `UPDATE "billingProfileValidation" SET doc=$2::jsonb WHERE id=$1`, id, body)
		if err != nil {
			return errors.New("validation decision persistence failed")
		}
		if tag.RowsAffected() != 1 {
			return errors.New("validation decision disappeared")
		}
		result = validationStatusDecision{doc: doc, profileID: bpID, suppressLegacy: d != nil || h.validationOutbox != nil}
		return nil
	})
	if err != nil {
		return validationStatusDecision{}, err
	}
	return result, nil
}

func validationProfileName(bp *billing.BillingProfile) string {
	return strings.TrimSpace(bp.FirstName + " " + bp.LastName)
}
