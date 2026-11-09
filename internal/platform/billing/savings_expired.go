package billing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// SavingsExpiredOutbox is an enqueue-only DB adapter, never a Notifier. The
// request contains only the existing expired template's fixed variable set.
type SavingsExpiredOutbox interface {
	EnqueueSavingsExpiredTx(context.Context, SavingsExpiredRequest) (ReminderQueueResult, error)
}
type SavingsExpiredRequest struct {
	ContractID, TermGeneration                                             string
	CanonicalEndDate, Date, ExpiresAt                                      time.Time
	Recipients                                                             []string
	FullName, SavingsPlanName, Currency, StartDate, MonthlyCommittedAmount string
}

func (SavingsExpiredRequest) String() string   { return "[savings expired request redacted]" }
func (SavingsExpiredRequest) GoString() string { return "[savings expired request redacted]" }
func (SavingsExpiredRequest) LogValue() slog.Value {
	return slog.StringValue("[savings expired request redacted]")
}

func (r *Repo) SetSavingsExpiredOutbox(o SavingsExpiredOutbox) { r.savingsExpiredOutbox = o }

// Always wired, even producer OFF and with no payload keys.
func (r *Repo) SetSavingsExpiredCanceler(c ReminderCanceler) { r.savingsExpiredCanceler = c }

// This private, non-document sidecar has no public DTO/admin history surface and
// no FK to the deletable/full-replaced contract. Selection is immutable forever;
// cancellation can only add a timestamp. Same-EndDate reactivation cannot adopt
// or resend any durable/legacy/no-mail/cancelled term. No historical backfill.
const savingsExpiredSchema = `
CREATE TABLE IF NOT EXISTS savings_expired_term (
 contract_id text NOT NULL,
 term_generation text NOT NULL CHECK(term_generation ~ '^[0-9a-f]{64}$'),
 source_end timestamptz NOT NULL,
 profile_id text NOT NULL,
 selection text NOT NULL CHECK(selection IN ('durable','legacy','no_mail')),
 decision_date timestamptz NOT NULL,
 expires_at timestamptz NOT NULL CHECK(expires_at=decision_date+interval '24 hours'),
 outbox_ids text[] NOT NULL CHECK(cardinality(outbox_ids)<=20),
 queue_state text NOT NULL,
 cancelled_at timestamptz,
 PRIMARY KEY(contract_id,term_generation)
);
CREATE OR REPLACE FUNCTION savings_expired_term_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND OLD.cancelled_at IS NULL AND NEW.cancelled_at IS NOT NULL
  AND (to_jsonb(NEW)-'cancelled_at') IS NOT DISTINCT FROM (to_jsonb(OLD)-'cancelled_at') THEN RETURN NEW; END IF;
 RAISE EXCEPTION 'savings expired term immutable selection';
END $$;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='savings_expired_term_immutable_selection' AND tgrelid='savings_expired_term'::regclass) THEN
  CREATE TRIGGER savings_expired_term_immutable_selection BEFORE UPDATE OR DELETE ON savings_expired_term FOR EACH ROW EXECUTE FUNCTION savings_expired_term_immutable();
 END IF;
END $$;
`

// EnsureSavingsExpired runs outside ANY caller TX, including startup while OFF.
// It neither adopts contracts nor reads mail keys nor performs external effects.
func (r *Repo) EnsureSavingsExpired(ctx context.Context) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	db := r.savingCtrs.DB()
	return db.WithTx(ctx, func(tc context.Context) error {
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
		if err = db.LockXact(tc, "savings-expired-schema-v1"); err != nil {
			return err
		}
		_, err = tx.Exec(tc, savingsExpiredSchema)
		return err
	})
}

// Observing a pre-upgrade EXPIRED term is not permission to adopt/backfill it.
// Before a cooperative admin writer can reactivate/delete/replace that term,
// retain a no-mail tombstone. Failed callbacks roll this observation back too.
func (r *Repo) retainHistoricalExpiredTx(ctx context.Context, c *SavingsContract) error {
	if c.Status != SavingsStatusExpired || c.EndDate == nil || c.EndDate.IsZero() || c.EndDate.Year() < 1 || c.EndDate.Year() > 9999 {
		return nil
	}
	tx, err := r.savingCtrs.DB().Tx(ctx)
	if err != nil {
		return err
	}
	end := c.EndDate.UTC().Truncate(time.Microsecond)
	_, err = tx.Exec(ctx, `INSERT INTO savings_expired_term(contract_id,term_generation,source_end,profile_id,selection,decision_date,expires_at,outbox_ids,queue_state)
	 SELECT $1,$2,$3,$4,'no_mail',d,d+interval '24 hours',ARRAY[]::text[],'' FROM (SELECT clock_timestamp() AS d) stamp ON CONFLICT DO NOTHING`, c.ID, reminderGeneration(end), end, c.BillingProfileID)
	return err
}

type savingsExpiredTerm struct {
	generation string
	ids        []string
}

func (r *Repo) expiredTermsTx(ctx context.Context, id string) ([]savingsExpiredTerm, error) {
	tx, err := r.savingCtrs.DB().Tx(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT term_generation,outbox_ids FROM savings_expired_term WHERE contract_id=$1 AND cancelled_at IS NULL ORDER BY term_generation FOR UPDATE`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []savingsExpiredTerm
	for rows.Next() {
		var v savingsExpiredTerm
		if err := rows.Scan(&v.generation, &v.ids); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repo) cancelSavingsExpiredTx(ctx context.Context, id string) error {
	rows, err := r.expiredTermsTx(ctx, id)
	if err != nil {
		return err
	}
	tx, err := r.savingCtrs.DB().Tx(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if len(row.ids) > 0 {
			if r.savingsExpiredCanceler == nil {
				return errors.New("savings expired cancellation unavailable")
			}
			for start := 0; start < len(row.ids); start += 20 {
				end := start + 20
				if end > len(row.ids) {
					end = len(row.ids)
				}
				if err := r.savingsExpiredCanceler.CancelPendingTx(ctx, row.ids[start:end]); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE savings_expired_term SET cancelled_at=clock_timestamp() WHERE contract_id=$1 AND term_generation=$2 AND cancelled_at IS NULL`, id, row.generation); err != nil {
			return err
		}
	}
	return nil
}

// expireSavingsTermTx is private and DB-only. Caller has the shared advisory,
// current contract, reminder rows and term rows locked in that order. Expiration
// closes OLD generations before selecting the new one, never cancelling itself.
func (s *SavingsService) expireSavingsTermTx(ctx context.Context, c *SavingsContract) (legacy bool, err error) {
	r := s.repo
	tx, err := r.savingCtrs.DB().Tx(ctx)
	if err != nil {
		return false, err
	}
	var date time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&date); err != nil {
		return false, err
	}
	date = date.UTC().Truncate(time.Microsecond)
	end := c.EndDate.UTC().Truncate(time.Microsecond)
	generation := reminderGeneration(end)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM savings_expired_term WHERE contract_id=$1 AND term_generation=$2)`, c.ID, generation).Scan(&exists); err != nil {
		return false, err
	}
	if err = r.cancelReminderRowsTx(ctx, c.ID, c.EndDate, date); err != nil {
		return false, err
	}
	if err = r.cancelSavingsExpiredTx(ctx, c.ID); err != nil {
		return false, err
	}
	c.Status = SavingsStatusExpired
	if err = r.SaveSavingsContract(ctx, c); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	mode, state := "no_mail", ""
	ids := []string{}
	expiry := date.Add(24 * time.Hour)
	if r.savingsExpiredOutbox != nil {
		var profile BillingProfile
		found, err := r.profiles.GetForUpdate(ctx, c.BillingProfileID, &profile)
		if err != nil {
			return false, err
		}
		if !found {
			return false, errors.New("savings expired profile unavailable")
		}
		in := SavingsExpiredRequest{ContractID: c.ID, TermGeneration: generation, CanonicalEndDate: end, Date: date, ExpiresAt: expiry,
			Recipients: Recipients(&profile), FullName: profileFullName(&profile), SavingsPlanName: c.SavingsPlanName, Currency: profile.Currency}
		if c.StartDate != nil {
			in.StartDate = c.StartDate.Format("2006-01-02")
		}
		if c.MonthlyCommittedAmount != nil {
			in.MonthlyCommittedAmount = c.MonthlyCommittedAmount.StringFixed(2)
		}
		q, err := r.savingsExpiredOutbox.EnqueueSavingsExpiredTx(ctx, in)
		if err != nil {
			return false, err
		}
		if len(q.IDs) == 0 || len(q.IDs) > 20 || q.State == "" {
			return false, errors.New("invalid savings expired queue result")
		}
		mode, state, ids = "durable", q.State, append([]string(nil), q.IDs...)
	} else if s.notifier != nil {
		mode, legacy = "legacy", true
	}
	_, err = tx.Exec(ctx, `INSERT INTO savings_expired_term(contract_id,term_generation,source_end,profile_id,selection,decision_date,expires_at,outbox_ids,queue_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, c.ID, generation, end, c.BillingProfileID, mode, date, expiry, ids, state)
	return legacy && err == nil, err
}
