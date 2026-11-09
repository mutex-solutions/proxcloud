package projectinvite

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

const durableDeliveryMode = "project-invite-outbox/v1"
const inviteDecisionLock = "project-invite-decisions-v1"

// InviteRequest is only for the fixed template adapter, not arbitrary template
// variables or user-supplied job functions. JoinURL contains a credential.
type InviteRequest struct {
	InviteID, Email, ProjectName, JoinURL string
	Date, ExpiresAt                       time.Time
}

func (InviteRequest) String() string       { return "[invite preparation redacted]" }
func (InviteRequest) GoString() string     { return "[invite preparation redacted]" }
func (InviteRequest) LogValue() slog.Value { return slog.StringValue("[invite preparation redacted]") }

type InviteOutbox interface {
	EnqueueInviteTx(context.Context, InviteRequest) (mailoutbox.QueueResult, error)
}

type inviteCanceler interface {
	CancelPendingTx(context.Context, []string) error
}

// Wiring is done once before serving. Cancellation remains wired when the
// producer is OFF or keys are absent; it only needs persisted opaque row IDs.
func (r *Repo) SetInviteCanceler(c inviteCanceler) { r.canceler = c }
func (h *Handler) SetInviteOutbox(q InviteOutbox)  { h.outbox = q }

func (h *Handler) createDurableInvite(ctx context.Context, email string, proj *project.Project) (string, error) {
	id, token := pgdoc.NewID(), uuid.NewString()
	err := h.repo.invites.DB().WithTx(ctx, func(tc context.Context) error {
		db := h.repo.invites.DB()
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
		if err = db.LockXact(tc, inviteDecisionLock); err != nil {
			return err
		}
		var now time.Time
		if err = tx.QueryRow(tc, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		expiry := now.Add(24 * time.Hour)
		doc := pgdoc.M{"_id": id, "email": email, "projectId": proj.ID, "token": token, "date": now, "expiresAt": expiry}
		if _, err = h.repo.invites.InsertOne(tc, doc); err != nil {
			return errors.New("project invite persistence failed")
		}
		q, err := h.outbox.EnqueueInviteTx(tc, InviteRequest{InviteID: id, Email: email, ProjectName: proj.Name,
			JoinURL: h.uiBaseURL + "/join-project?invite-token=" + token, Date: now, ExpiresAt: expiry})
		if err != nil {
			return errors.New("project invite preparation failed")
		}
		if len(q.IDs) != 1 {
			return errors.New("project invite queue result invalid")
		}
		doc["deliveryMode"], doc["outboxIds"] = durableDeliveryMode, q.IDs
		found, err := h.repo.invites.Replace(tc, id, doc)
		if err != nil || !found {
			return errors.New("project invite decision persistence failed")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// deleteInvites owns the delete+cancel transaction. A single bounded decision
// advisory lock also covers inserts (including legacy ones), so ALL matching
// rows at this decision are removed, not an arbitrary capped first page. Core
// workers lock outbox only and never call back into invites: invite -> outbox.
func (r *Repo) deleteInvites(ctx context.Context, email, projectID string, expiredOnly bool) error {
	if err := pgdoc.RejectInheritedTransaction(ctx); err != nil {
		return err
	}
	return r.invites.DB().WithTx(ctx, func(tc context.Context) error {
		db := r.invites.DB()
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
		if err = db.LockXact(tc, inviteDecisionLock); err != nil {
			return err
		}
		after := ""
		for {
			rows, err := tx.Query(tc, `SELECT id,doc FROM "projectInvite" WHERE id>$1
			 AND (($4 AND pgdoc_ts(doc->>'expiresAt')<=clock_timestamp()) OR
			 (NOT $4 AND doc->>'email'=$2 AND doc->>'projectId'=$3)) ORDER BY id LIMIT 128 FOR UPDATE`, after, email, projectID, expiredOnly)
			if err != nil {
				return errors.New("project invite cleanup unavailable")
			}
			var ids, outboxIDs []string
			for rows.Next() {
				var id string
				var raw []byte
				if err = rows.Scan(&id, &raw); err != nil {
					rows.Close()
					return err
				}
				var doc struct {
					Mode string   `json:"deliveryMode"`
					IDs  []string `json:"outboxIds"`
				}
				if json.Unmarshal(raw, &doc) != nil || (doc.Mode == durableDeliveryMode && len(doc.IDs) != 1) || (doc.Mode != "" && doc.Mode != "legacy" && doc.Mode != durableDeliveryMode) {
					rows.Close()
					return errors.New("project invite delivery decision invalid")
				}
				if doc.Mode == durableDeliveryMode {
					outboxIDs = append(outboxIDs, doc.IDs...)
				}
				ids = append(ids, id)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			for len(outboxIDs) > 0 {
				n := min(len(outboxIDs), mailoutbox.MaxRecipients)
				if r.canceler == nil {
					return errors.New("project invite cancellation unavailable")
				}
				if err = r.canceler.CancelPendingTx(tc, outboxIDs[:n]); err != nil {
					return errors.New("project invite cancellation failed")
				}
				outboxIDs = outboxIDs[n:]
			}
			if _, err = tx.Exec(tc, `DELETE FROM "projectInvite" WHERE id=ANY($1::text[])`, ids); err != nil {
				return errors.New("project invite deletion failed")
			}
			after = ids[len(ids)-1]
		}
	})
}
