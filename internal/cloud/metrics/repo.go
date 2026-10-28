package metrics

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo backs the `usageMetrics` collection (the monthly usage records the rating cron
// charges from).
type Repo struct {
	col *pgdoc.Store
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{col: db.C("usageMetrics")}
}

// FindForCurrentMonth mirrors findResourceMetricsForCurrentMonth: the doc for resourceId
// whose billingCycle.startDate ≥ cycleStart. (nil,nil) when absent.
func (r *Repo) FindForCurrentMonth(ctx context.Context, resourceID string, cycleStart time.Time) (*UsageMetrics, error) {
	var m UsageMetrics
	found, err := r.col.FindOne(ctx, pgdoc.M{
		"resourceId":             resourceID,
		"billingCycle.startDate": pgdoc.M{"$gte": cycleStart},
	}, &m)
	if err != nil || !found {
		return nil, err
	}
	return &m, nil
}

// Save inserts a new doc (assigning _id) or replaces the existing one by _id.
func (r *Repo) Save(ctx context.Context, m *UsageMetrics) (*UsageMetrics, error) {
	if m.ID == "" {
		id, err := r.col.InsertOne(ctx, m)
		if err != nil {
			return nil, err
		}
		m.ID = id
		return m, nil
	}
	if _, err := r.col.Replace(ctx, m.ID, m); err != nil {
		return nil, err
	}
	return m, nil
}

// GetMetric is the find-or-create for the current cycle: returns the
// existing month doc or creates a zero-initialised one (details zeroed).
func (r *Repo) GetMetric(ctx context.Context, cr *cloud.CloudResource, startAt, endAt time.Time) (*UsageMetrics, error) {
	m, err := r.FindForCurrentMonth(ctx, cr.ID, startAt)
	if err != nil {
		return nil, err
	}
	if m != nil {
		return m, nil
	}
	s, e := startAt, endAt
	return r.Save(ctx, &UsageMetrics{
		ResourceType: cr.Type,
		ResourceID:   cr.ID,
		BillingCycle: &BillBillingCycle{StartDate: &s, EndDate: &e},
		Details:      zeroDetails(),
	})
}
