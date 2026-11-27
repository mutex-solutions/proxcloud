package project

// cloud_function_scale.go — the scale side of functions (cloud_function.go): the one writer of a function's instance
// count, the running intervals billing reads, the function's cloud row, and the project-deletion leg.
//
// scaleFunction is the only writer of functionDoc.Instances. Every boundary of a running interval (a start, a stop)
// is written with the cloud row in one transaction: the row never says something of an interval the record does not.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// scaleFunction sets the function's instance count on the cluster and in its records. It is the only writer of
// functionDoc.Instances, which is what billing charges for. Going up the records are written first, going down the
// cluster is changed first: whichever step fails, an instance never runs unrecorded (unbilled). Two callers racing
// in opposite directions can still leave the cluster and the record apart.
//
// A step down that keeps instances running (several to fewer) is a way down too: the cluster first. Whether it is
// one is judged by fn, which the callers read fresh.
func (h *Handler) scaleFunction(ctx context.Context, fs *function.Service, fn *functionDoc, instances int) error {
	if instances > 0 && instances >= fn.Instances {
		if err := h.recordInstances(ctx, fn, instances); err != nil {
			return err
		}
		return fs.SetScale(ctx, fn.ProjectID, fn.ID, fn.Instances) // the count on record: it may run more
	}
	if err := fs.SetScale(ctx, fn.ProjectID, fn.ID, instances); err != nil && !errors.Is(err, function.ErrNotFound) {
		return err
	}
	return h.recordInstances(ctx, fn, instances)
}

// rollingOut is the snapshot of the function's rollout in progress: nil when there is none, or when its record
// cannot be read.
func (h *Handler) rollingOut(ctx context.Context, fn *functionDoc) *function.Snapshot {
	if fn.DeployingID == "" {
		return nil
	}
	var dep functionDeployment
	if found, err := h.fnDeps.Get(ctx, fn.DeployingID, &dep); err != nil {
		slog.Warn("function: read the deployment in progress", "function", fn.ID, "err", err)
	} else if found {
		return &dep.Snapshot
	}
	return nil
}

// functionEndedKeep is how long a running interval that has ended stays on the function and its cloud row: a charge
// tick that did not see the function stopped (it started again before the tick, or the ticks were late) still
// charges the interval to its end while it is there. Longer than a restart of the charge job; short, because a price
// rule that is added bills what the row carries.
const functionEndedKeep = 10 * time.Minute

// endedIntervals is the function's ended intervals once the one it is in ends at now: those that ended within
// functionEndedKeep, and that one, with the count and size it is billed at once stopped (functionRowData). The size
// is kept with it because the row's is the next deployment's as soon as that is applied. At most one per idle time of
// the function, so a handful.
func (h *Handler) endedIntervals(ctx context.Context, fn *functionDoc, now time.Time) []fnEnded {
	out := slices.DeleteFunc(slices.Clone(fn.Ended), func(e fnEnded) bool { return now.Sub(e.To) > functionEndedKeep })
	if fn.RunningSince != nil {
		e := fnEnded{From: *fn.RunningSince, To: now, Instances: max(fn.LastInstances, 1)}
		e.CPUMillis, e.MemoryMi = h.billedSize(ctx, fn)
		out = append(out, e)
	}
	return out
}

// recordInstances writes the instance count to the function record (stamping lastActiveAt when it runs, and the
// bounds of the running interval when it starts or stops, where the interval that ends joins the ended ones) and,
// when the count changed, to the cloud row: in one transaction, so no boundary of an interval is in the record
// without being in the row. It holds the record's lock while it decides, and decides by the record as it is then,
// not by fn (a copy read earlier): of two writers racing through the same change exactly one stamps the interval,
// and the row is always the projection of the record that was just written. Only whether the caller meant a way up
// is judged by fn: a step down between two running counts is not activity (the idle clock runs on).
//
// A way up never lowers the count: a function that runs more than the caller asks for keeps what it runs. fn is the
// record as it is afterwards.
func (h *Handler) recordInstances(ctx context.Context, fn *functionDoc, instances int) error {
	out := *fn // fn changes only when the transaction commits
	err := h.fns.DB().WithTx(ctx, func(tc context.Context) error {
		out = *fn
		return h.writeInstances(tc, &out, instances)
	})
	if err == nil {
		*fn = out
	}
	return err
}

func (h *Handler) writeInstances(ctx context.Context, fn *functionDoc, instances int) error {
	now := time.Now().UTC()
	var cur functionDoc
	found, err := h.fns.GetForUpdate(ctx, fn.ID, &cur)
	if err != nil {
		return err
	}
	if !found { // deleted meanwhile: its row must not be written again
		if instances > 0 {
			return function.ErrNotFound
		}
		fn.Instances = 0
		return nil
	}
	var set pgdoc.M
	var unset []string
	switch up := instances >= fn.Instances; {
	case instances > 0 && cur.Instances == 0: // a start: a new interval
		set, unset = pgdoc.M{"instances": instances, "lastInstances": instances, "lastActiveAt": now, "runningSince": now}, []string{"stoppedAt"}
	case instances > 0 && up && cur.Instances > instances: // it runs more than was asked for, and keeps it
		set = pgdoc.M{"lastActiveAt": now}
	case instances > 0:
		if set = (pgdoc.M{"instances": instances, "lastInstances": instances}); up {
			set["lastActiveAt"] = now
		}
	case cur.Instances > 0: // a stop: the interval ends
		set = pgdoc.M{"instances": 0, "stoppedAt": now, "endedIntervals": h.endedIntervals(ctx, &cur, now)}
	default: // stopped already: no second boundary
		*fn = cur
		return nil
	}
	if _, err := h.fns.SetByID(ctx, fn.ID, set, unset); err != nil {
		return err
	}
	var next functionDoc // a new one: decoding onto cur would keep what the write has just removed
	if found, err := h.fns.Get(ctx, fn.ID, &next); err != nil || !found {
		return cmp.Or(err, function.ErrNotFound)
	}
	if *fn = next; next.Instances == cur.Instances {
		return nil
	}
	_, err = h.syncFunctionRow(ctx, fn, now)
	return err
}

// Cloud row statuses (data.status of a FUNCTION row).
const (
	functionRowRunning   = "RUNNING"
	functionRowIdle      = "IDLE"
	functionRowSuspended = "SUSPENDED"
	functionRowDeleted   = "DELETED"
)

// billedSize is the size billing charges an instance of the function at, and the size the project's quota counts it
// at. While a rollout is in progress the new snapshot's instance runs next to the serving one, so the larger of the
// two holds until the rollout settles; otherwise a rollout that never becomes ready could run a large instance at
// the price of a small one, again and again.
func (h *Handler) billedSize(ctx context.Context, fn *functionDoc) (cpu, memory int) {
	cpu, memory = fn.CPUMillis, fn.MemoryMi
	if s := h.rollingOut(ctx, fn); s != nil {
		cpu, memory = max(cpu, s.CPUMillis), max(memory, s.MemoryMi)
	}
	return cpu, memory
}

// functionRowData is the payload of a function's cloud row: a projection of its records, and what billing reads
// (cpuMillis, memoryMi, instances, billedInstances, status, runningSince, stoppedAt, endedIntervals). It holds the
// names of the function's secrets and nothing else of them.
func (h *Handler) functionRowData(ctx context.Context, fn *functionDoc) map[string]any {
	status := functionRowIdle
	switch {
	case fn.Suspended:
		status = functionRowSuspended
	case fn.Instances > 0:
		status = functionRowRunning
	}
	cpu, memory := h.billedSize(ctx, fn)
	// billedInstances is what the minute rate is multiplied by: the count that runs, and once the function stops the
	// count it last ran with, so the end of the interval is billed at it.
	billed := fn.Instances
	if billed == 0 {
		billed = max(fn.LastInstances, 1)
	}
	data := map[string]any{
		"name": fn.Name, "host": fn.Host, "url": "https://" + fn.Host, "image": fn.Image,
		"cpuMillis": cpu, "memoryMi": memory, "instances": fn.Instances, "billedInstances": billed, "status": status,
	}
	// The bounds of the current or last running interval.
	if fn.RunningSince != nil {
		data["runningSince"] = fn.RunningSince.Format(time.RFC3339)
	}
	if fn.StoppedAt != nil {
		data["stoppedAt"] = fn.StoppedAt.Format(time.RFC3339)
	}
	// The intervals that ended lately, for a charge tick that did not see the function stopped, each with the size
	// it ran with.
	if len(fn.Ended) > 0 {
		ended := make([]any, len(fn.Ended))
		for i, e := range fn.Ended {
			ended[i] = map[string]any{"from": e.From.Format(time.RFC3339), "to": e.To.Format(time.RFC3339), "instances": e.Instances,
				"cpuMillis": e.CPUMillis, "memoryMi": e.MemoryMi}
		}
		data["endedIntervals"] = ended
	}
	return data
}

// functionRowStale reports a row that no longer says what billing must charge for its function.
func functionRowStale(row, want map[string]any) bool {
	return intAny(row["instances"]) != intAny(want["instances"]) || intAny(row["billedInstances"]) != intAny(want["billedInstances"]) || row["status"] != want["status"] ||
		intAny(row["cpuMillis"]) != intAny(want["cpuMillis"]) || intAny(row["memoryMi"]) != intAny(want["memoryMi"])
}

// syncFunctionRow rewrites the function's cloud row from its records. Callers write the record first, and pass the
// context of the transaction that wrote it.
func (h *Handler) syncFunctionRow(ctx context.Context, fn *functionDoc, now time.Time) (*cloud.CloudResource, error) {
	return h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: fn.ServiceID, Region: fn.Region, ProjectID: fn.ProjectID,
		Type: cloud.TypeFunction, ExternalID: fn.ID, Data: h.functionRowData(ctx, fn), CreatedAt: &now, UpdatedAt: &now,
	})
}

// deletedFunctionRow is a row's payload once its function is gone: stopped now if it was running.
func deletedFunctionRow(d map[string]any, now time.Time) map[string]any {
	out := maps.Clone(d)
	if intAny(d["instances"]) > 0 {
		out["stoppedAt"] = now.Format(time.RFC3339)
	}
	out["instances"], out["status"] = 0, functionRowDeleted
	return out
}

// endFunctionRow ends a deleted function's row: marked DELETED with stoppedAt set when it was running, and archived
// at once. The archived row is what billing rates until the cycle ends, so the last running interval is billed to
// its end and nothing after it; no row lingers among the live ones.
//
// void archives it without any interval: the row of a function that never came to be.
func (h *Handler) endFunctionRow(ctx context.Context, fn *functionDoc, now time.Time, void bool) error {
	row, err := h.cloud.FindByServiceIDAndExternalID(ctx, fn.ServiceID, fn.ID)
	if err != nil || row == nil {
		return err
	}
	if row.Data, row.UpdatedAt = deletedFunctionRow(row.Data, now), &now; void {
		for _, k := range []string{"runningSince", "stoppedAt", "endedIntervals"} {
			delete(row.Data, k)
		}
	}
	return h.cloud.DeleteAndArchive(ctx, row, now)
}

// functionFloor is the instance count a function is kept at when idle: its effective floor under the location's
// current config, or zero when it has nothing healthy to run.
func functionFloor(fn *functionDoc, fc sandbox.FunctionsConfig) int {
	if fn.CurrentDeploymentID == "" {
		return 0
	}
	return function.EffectiveFloor(fc, fn.MinInstances)
}

// DeleteProjectFunctions removes everything of a project's functions (project teardown): the function namespace on
// every sandbox location of the project, with whatever is left in it, then the records and the cloud rows, archived
// with the end of their last running interval. A location with functions of the project that cannot be cleaned is an
// error and nothing is dropped: records without a namespace delete would leave instances running that nothing bills
// or stops.
func (h *Handler) DeleteProjectFunctions(ctx context.Context, projectID string) error {
	if h.fns == nil || h.functionFor == nil {
		return nil
	}
	p, err := h.svc.repo.FindByID(ctx, projectID)
	if err != nil || p == nil {
		return err
	}
	used, err := h.fns.Distinct(ctx, "serviceId", pgdoc.M{"projectId": projectID})
	if err != nil {
		return err
	}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(append(p.ServiceIDs(), used...)))) {
		_, fs, err := h.functionSvc(ctx, id)
		if err == nil {
			err = fs.DeleteNamespace(ctx, projectID)
		}
		// A location the project has no function on may be anything (not a sandbox provider, unreachable): at worst
		// an empty namespace stays there. One that no longer offers functions is cleaned like any other.
		if err != nil && slices.Contains(used, id) {
			return fmt.Errorf("functions on service %s: %w", id, err)
		}
	}
	if err := h.dropFunctionRecords(ctx, pgdoc.M{"projectId": projectID}, time.Now().UTC(), false); err != nil {
		return err
	}
	// A row whose record was lost some other way is no function any more: ended like the others.
	rows, err := h.cloud.FindByProjectAndType(ctx, projectID, cloud.TypeFunction)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range rows {
		rows[i].Data = deletedFunctionRow(rows[i].Data, now)
		if err := h.cloud.DeleteAndArchive(ctx, &rows[i], now); err != nil {
			return err
		}
	}
	return nil
}
