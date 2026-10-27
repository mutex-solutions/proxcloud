package audit

import (
	"context"
	"regexp"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo backs the auditEvent table (write + cursor-paginated read).
type Repo struct{ col *pgdoc.Store }

func NewRepo(db *pgdoc.DB) *Repo { return &Repo{col: db.C("auditEvent")} }

// Insert persists an event, stamping the timestamp on write.
func (r *Repo) Insert(ctx context.Context, ev AuditEvent) error {
	now := time.Now().UTC()
	ev.Timestamp = &now
	_, err := r.col.InsertOne(ctx, ev)
	return err
}

// Filter mirrors AuditQueryFilter (the AND-combined query fields).
type Filter struct {
	OrganizationID   string
	ProjectID        string
	ActorID          string
	RequestInterface string
	EventContext     string
	ResourceType     string
	ResourceID       string
	Action           string
	Outcome          string
	From             *time.Time
	To               *time.Time
	Search           string
}

func (f Filter) criteria() pgdoc.M {
	m := pgdoc.M{}
	if f.RequestInterface != "" {
		m["requestInterface"] = f.RequestInterface
	}
	if f.EventContext != "" {
		m["eventContext"] = f.EventContext
	}
	if f.OrganizationID != "" {
		m["organizationId"] = f.OrganizationID
	}
	if f.ProjectID != "" {
		m["projectId"] = f.ProjectID
	}
	if f.ActorID != "" {
		m["actor.id"] = f.ActorID
	}
	if f.ResourceType != "" {
		m["resourceType"] = f.ResourceType
	}
	if f.ResourceID != "" {
		m["resourceId"] = f.ResourceID
	}
	if f.Action != "" {
		m["action"] = f.Action
	}
	if f.Outcome != "" {
		m["outcome"] = f.Outcome
	}
	if f.From != nil || f.To != nil {
		ts := pgdoc.M{}
		if f.From != nil {
			ts["$gte"] = *f.From
		}
		if f.To != nil {
			ts["$lte"] = *f.To
		}
		m["timestamp"] = ts
	}
	if f.Search != "" {
		esc := regexp.QuoteMeta(f.Search)
		m["$or"] = []pgdoc.M{
			{"resourceDisplayName": pgdoc.M{"$regex": esc, "$options": "i"}},
			{"actor.displayName": pgdoc.M{"$regex": esc, "$options": "i"}},
			{"resourceId": pgdoc.M{"$regex": esc, "$options": "i"}},
		}
	}
	return m
}

// Query runs a cursor-paginated query (sort _id DESC): forward via `after`
// (_id < cursor), backward via `before` (_id > cursor, fetched ASC then reversed).
// Returns events + next/prev markers. Ids are time-prefixed, so id order ≈ recency.
func (r *Repo) Query(ctx context.Context, f Filter, after, before string, limit int) ([]AuditEvent, *string, *string, error) {
	q := f.criteria()
	fetch := int64(limit + 1)

	if before != "" {
		q["_id"] = pgdoc.M{"$gt": before}
		var docs []AuditEvent
		if err := r.col.Find(ctx, q, &docs, pgdoc.Sort(pgdoc.Asc("_id")), pgdoc.Limit(fetch)); err != nil {
			return nil, nil, nil, err
		}
		hasMore := len(docs) > limit
		if hasMore {
			docs = docs[len(docs)-limit:] // keep the `limit` closest to the cursor
		}
		reverse(docs)
		var prev, next *string
		if hasMore && len(docs) > 0 {
			prev = &docs[0].ID
		}
		if len(docs) > 0 {
			next = &docs[len(docs)-1].ID
		}
		return docs, next, prev, nil
	}

	if after != "" {
		q["_id"] = pgdoc.M{"$lt": after}
	}
	var docs []AuditEvent
	if err := r.col.Find(ctx, q, &docs, pgdoc.Sort(pgdoc.Desc("_id")), pgdoc.Limit(fetch)); err != nil {
		return nil, nil, nil, err
	}
	var next, prev *string
	if len(docs) > limit {
		next = &docs[limit-1].ID
		docs = docs[:limit]
	}
	if after != "" && len(docs) > 0 {
		prev = &docs[0].ID
	}
	return docs, next, prev, nil
}

// QueryAll returns all events matching the filter (sort _id DESC), capped at limit — backs the
// admin audit export (limit 10000).
func (r *Repo) QueryAll(ctx context.Context, f Filter, limit int) ([]AuditEvent, error) {
	out := []AuditEvent{}
	if err := r.col.Find(ctx, f.criteria(), &out, pgdoc.Sort(pgdoc.Desc("_id")), pgdoc.Limit(int64(limit))); err != nil {
		return nil, err
	}
	return out, nil
}

func reverse(s []AuditEvent) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
