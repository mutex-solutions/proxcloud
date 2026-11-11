package cloud

import (
	"context"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// MeterStaleAfter is how long a provider's state may go unobserved before its resources stop
// billing (plan decision 12): last-known state is trusted up to here, not beyond.
const MeterStaleAfter = 30 * time.Minute

// MeterState is one provider's metering heartbeat (collection meterState, id = serviceId).
type MeterState struct {
	ID          string           `json:"id,omitempty"`
	LastOKAt    *time.Time       `json:"lastOkAt,omitempty"`
	AlertedAt   *time.Time       `json:"alertedAt,omitempty"`
	TaskCursors map[string]int64 `json:"taskCursors,omitempty"`
}

// Stale reports whether the provider's last good sample is older than MeterStaleAfter. A provider
// never sampled is not stale: the sampler may simply be off (a deploy without the scheduler).
func (m *MeterState) Stale(now time.Time) bool {
	return m != nil && m.LastOKAt != nil && now.Sub(*m.LastOKAt) > MeterStaleAfter
}

// MeterState returns the provider's heartbeat, or (nil,nil).
func (r *Repo) MeterState(ctx context.Context, serviceID string) (*MeterState, error) {
	var out MeterState
	found, err := r.meter.Get(ctx, serviceID, &out)
	if err != nil || !found {
		return nil, err
	}
	return &out, nil
}

// SetMeterFields merges fields into the provider's heartbeat doc, creating it on first write.
func (r *Repo) SetMeterFields(ctx context.Context, serviceID string, set pgdoc.M, unset ...string) error {
	found, err := r.meter.SetByID(ctx, serviceID, set, unset)
	if err != nil || found {
		return err
	}
	doc := pgdoc.M{"_id": serviceID}
	for k, v := range set {
		doc[k] = v
	}
	_, err = r.meter.InsertOne(ctx, doc)
	if pgdoc.IsDup(err) {
		_, err = r.meter.SetByID(ctx, serviceID, set, unset)
	}
	return err
}

// TaskEvent is one finished Proxmox task on a VM (collection vmTaskEvent, id = UPID). The insert's
// primary key is the poller's dedupe; LIST_EVENTS reads these back.
type TaskEvent struct {
	ID        string     `json:"id,omitempty"`
	ServiceID string     `json:"serviceId,omitempty"`
	ProjectID string     `json:"projectId,omitempty"`
	VMID      string     `json:"vmid,omitempty"`
	Node      string     `json:"node,omitempty"`
	Type      string     `json:"type,omitempty"`
	User      string     `json:"user,omitempty"`
	Status    string     `json:"status,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// InsertTaskEvent records a finished task; false when the UPID was already recorded.
func (r *Repo) InsertTaskEvent(ctx context.Context, ev *TaskEvent) (bool, error) {
	if _, err := r.tasks.InsertOne(ctx, ev); err != nil {
		if pgdoc.IsDup(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// TaskEventsForVM lists a VM's recorded tasks, newest first. since is the row's createdAt: PVE
// reuses vmids, so an older VM's tasks (possibly another tenant's) stay out.
func (r *Repo) TaskEventsForVM(ctx context.Context, serviceID, projectID, vmid string, since time.Time, limit int) ([]TaskEvent, error) {
	out := []TaskEvent{}
	err := r.tasks.Find(ctx, pgdoc.M{
		"serviceId": serviceID, "projectId": projectID, "vmid": vmid,
		"startedAt": pgdoc.M{"$gte": since},
	}, &out, pgdoc.Sort(pgdoc.DescK("startedAt", pgdoc.KTime)), pgdoc.Limit(int64(limit)))
	return out, err
}

// PruneTaskEvents drops tasks that started before cutoff.
func (r *Repo) PruneTaskEvents(ctx context.Context, cutoff time.Time) error {
	_, err := r.tasks.DeleteMany(ctx, pgdoc.M{"startedAt": pgdoc.M{"$lt": cutoff}})
	return err
}

// FindArchivedSince lists a project's history rows on a service deleted at or after since: the
// rows the charge job still owes a final interval.
func (r *Repo) FindArchivedSince(ctx context.Context, projectID, serviceID string, since time.Time) ([]History, error) {
	out := []History{}
	err := r.history.Find(ctx, pgdoc.M{
		"projectId": projectID, "serviceId": serviceID,
		"deletedAt": pgdoc.M{"$gte": since},
	}, &out)
	return out, err
}
