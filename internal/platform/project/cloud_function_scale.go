package project

// cloud_function_scale.go — the scale side of functions (cloud_function.go): the one writer of a function's instance
// count, the running intervals billing reads, the function's cloud row, the two routes sandbox-proxy calls (wake,
// activity), scaling on load, the reaper, and the project-deletion leg.
//
//	POST /api/v1/internal/functions/{functionID}/wake   sandbox-proxy: start an instance for a held request
//	POST /api/v1/internal/functions/activity            sandbox-proxy: which functions have traffic, and how much
//
// scale is the only writer of functionDoc.Instances (scaleFunction, stopFunction and rescaleFunction are its three
// doors). Every boundary of a running interval (a start, a stop) is written with the cloud row in one transaction:
// the row never says something of an interval the record does not.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// fnStop says how a running interval ends, for the callers that know more of it than "now, and billed".
type fnStop struct {
	// at is when the interval ended, when that is earlier than now: a function that idled out while the reaper was
	// not running ended when its idle time ran out, not when the reaper came back. Zero = now. It is held between
	// the interval's start and now.
	at time.Time
	// idle marks a stop the idle rule decided. It yields to activity: when the record carries a later lastActiveAt
	// than the copy the rule was decided on (a wake or a report came between), nothing is stopped.
	idle bool
	// void ends an interval that never had a ready instance (ReadyAt unset in the record as it is then) as if it had
	// never begun: it joins no ended interval and the row keeps no bound of it, so nothing of it is ever billed. An
	// interval that did have one is ended as any other, whatever the caller says.
	void bool
}

// scaleFunction sets the function's instance count on the cluster and in its records. Going up the records are
// written first, going down the cluster is changed first: whichever step fails, an instance never runs unrecorded
// (unbilled). Two callers racing in opposite directions can still leave the cluster and the record apart; the
// deploy sync brings the cluster back to the record.
//
// A step down that keeps instances running (several to fewer) is a way down too: the cluster first. Whether it is
// one is judged by fn, which the callers read fresh.
//
// A stop (to zero) is recorded whatever the cluster answers: a cluster that is gone or cannot be reached must not
// keep a function on record as running, and billed, without end. The cluster's error is still returned, after the
// record is written; the deploy sync repeats the call until the Deployment is at the recorded zero.
func (h *Handler) scaleFunction(ctx context.Context, fs *function.Service, fn *functionDoc, instances int) error {
	return h.scale(ctx, fs, fn, instances, fnStop{})
}

// stopFunction is scaleFunction to zero for a caller that says how the interval ends (fnStop).
func (h *Handler) stopFunction(ctx context.Context, fs *function.Service, fn *functionDoc, stop fnStop) error {
	return h.scale(ctx, fs, fn, 0, stop)
}

func (h *Handler) scale(ctx context.Context, fs *function.Service, fn *functionDoc, instances int, stop fnStop) error {
	if instances > 0 && instances >= fn.Instances {
		if err := h.recordInstances(ctx, fn, instances, stop); err != nil {
			return err
		}
		return fs.SetScale(ctx, fn.ProjectID, fn.ID, fn.Instances) // the count on record: it may run more
	}
	cerr := fs.SetScale(ctx, fn.ProjectID, fn.ID, instances)
	if errors.Is(cerr, function.ErrNotFound) {
		cerr = nil
	}
	if cerr != nil && instances > 0 {
		return cerr
	}
	if cerr != nil {
		slog.Warn("function: the cluster did not take a stop, recorded all the same", "function", fn.ID, "err", cerr)
	}
	if err := h.recordInstances(ctx, fn, instances, stop); err != nil {
		return err
	}
	if instances == 0 && fn.Instances > 0 && cerr == nil {
		// The stop yielded to activity (fnStop.idle) after the cluster was taken down: back to the count on record.
		return fs.SetScale(ctx, fn.ProjectID, fn.ID, fn.Instances)
	}
	return cerr
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
	out := h.keptIntervals(fn, now)
	if fn.RunningSince != nil {
		e := fnEnded{From: *fn.RunningSince, To: now, Instances: max(fn.LastInstances, 1)}
		e.CPUMillis, e.MemoryMi = h.billedSize(ctx, fn)
		out = append(out, e)
	}
	return out
}

// keptIntervals are the ended intervals the function still carries at now, without the one it is in.
func (h *Handler) keptIntervals(fn *functionDoc, now time.Time) []fnEnded {
	return slices.DeleteFunc(slices.Clone(fn.Ended), func(e fnEnded) bool { return now.Sub(e.To) > functionEndedKeep })
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
// record as it is afterwards. stop is read only when the write is a stop.
func (h *Handler) recordInstances(ctx context.Context, fn *functionDoc, instances int, stop fnStop) error {
	out := *fn // fn changes only when the transaction commits
	err := h.fns.DB().WithTx(ctx, func(tc context.Context) error {
		out = *fn
		return h.writeInstances(tc, &out, instances, stop)
	})
	if err == nil {
		*fn = out
	}
	return err
}

// fnIntervalMarks are what a function holds of the interval it is in beside its bounds: removed with every start,
// so each interval proves for itself that it serves.
var fnIntervalMarks = []string{"readyAt", "unreadyAt"}

func (h *Handler) writeInstances(ctx context.Context, fn *functionDoc, instances int, stop fnStop) error {
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
		set = pgdoc.M{"instances": instances, "lastInstances": instances, "lastActiveAt": now, "runningSince": now}
		unset = append([]string{"stoppedAt"}, fnIntervalMarks...)
	case instances > 0 && up && cur.Instances > instances: // it runs more than was asked for, and keeps it
		set = pgdoc.M{"lastActiveAt": now}
	case instances > 0:
		if set = (pgdoc.M{"instances": instances, "lastInstances": instances}); up {
			set["lastActiveAt"] = now
		}
	case cur.Instances > 0 && stop.void && cur.ReadyAt == nil: // a stop of a start that never served: no interval
		set = pgdoc.M{"instances": 0, "endedIntervals": h.keptIntervals(&cur, now)}
		unset = append([]string{"runningSince", "stoppedAt"}, fnIntervalMarks...)
	case cur.Instances > 0 && stop.idle && cur.LastActiveAt != nil && (fn.LastActiveAt == nil || cur.LastActiveAt.After(*fn.LastActiveAt)):
		*fn = cur // it had activity since the idle rule looked: not idle, no boundary
		return nil
	case cur.Instances > 0: // a stop: the interval ends
		at := now
		if !stop.at.IsZero() && stop.at.Before(now) {
			at = stop.at.UTC()
		}
		if cur.RunningSince != nil && at.Before(*cur.RunningSince) {
			at = *cur.RunningSince
		}
		set = pgdoc.M{"instances": 0, "stoppedAt": at, "endedIntervals": h.endedIntervals(ctx, &cur, at)}
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
// (cpuMillis, memoryMi, instances, billedInstances, status, runningSince, readyAt, stoppedAt, endedIntervals). It holds the
// names of the function's secrets and nothing else of them. With readyAt (below) these are the fields the meter is
// written against.
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
	// When the running interval was first seen with a ready instance. A running interval without it is one billing
	// holds back: it bills from runningSince once this is stamped, or once the interval ends some other way than as
	// void (a void one leaves no bound here at all).
	if fn.ReadyAt != nil {
		data["readyAt"] = fn.ReadyAt.Format(time.RFC3339)
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
		intAny(row["cpuMillis"]) != intAny(want["cpuMillis"]) || intAny(row["memoryMi"]) != intAny(want["memoryMi"]) ||
		strAny(row["runningSince"]) != strAny(want["runningSince"]) || strAny(row["stoppedAt"]) != strAny(want["stoppedAt"]) ||
		strAny(row["readyAt"]) != strAny(want["readyAt"])
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

// proxyError answers sandbox-proxy in the portal's own envelope, which is what its Waker reads.
func proxyError(w http.ResponseWriter, status int, msg string) {
	if status >= 500 || status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "2")
	}
	httpx.Err(w, status, status, msg)
}

// proxyBearer is the bearer token of a proxy call, "" when the request carries none in that form.
func proxyBearer(r *http.Request) string {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return bearer
}

// maxProxyBody caps the body of both proxy routes: they are reachable without a user credential.
const maxProxyBody = 64 << 10

// functionWake: POST /internal/functions/{functionID}/wake, called by sandbox-proxy when a request reaches a function
// that has no ready instance. pkg/auth lets the path through without a user credential: the bearer must be the
// function wake token of a sandbox provider, checked before anything about the function is looked up, and the
// function must be that provider's (to any other token it does not exist).
//
// It scales the function to one instance (at most: the proxy's lookup can be a moment behind) and returns at once;
// the proxy holds its requests until a pod is ready. The portal alone writes the scale, so the billing gate,
// suspension and the usage row have one writer.
//
//	200  the function is at one instance or more                 {functionID, instances}
//	401  no bearer, or one that is no provider's function wake token
//	404  no such function for this token (the proxy remembers it for a few seconds)
//	403  functions are off where it lives, it is suspended, or its project cannot be billed
//	409  it has nothing to run: no deployment ever became ready and none is in progress
//	429  the cluster refuses the instance for the project's quota
//	503  the job scheduler is off, a release gate does not hold, or a read or the cluster failed (Retry-After)
func (h *Handler) functionWake(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svcIDs := h.functionTokenServices(ctx, proxyBearer(r))
	if len(svcIDs) == 0 {
		proxyError(w, http.StatusUnauthorized, "invalid wake token")
		return
	}
	id := chi.URLParam(r, "functionID")
	if h.fns == nil || h.functionFor == nil || !function.ValidID(id) {
		proxyError(w, http.StatusNotFound, "function not found")
		return
	}
	unavailable := func(what string, err error) {
		slog.Warn("function wake failed", "function", id, "step", what, "err", err)
		proxyError(w, http.StatusServiceUnavailable, "functions are not available")
	}
	var fn functionDoc
	found, err := h.fns.Get(ctx, id, &fn)
	if err != nil {
		unavailable("read the function", err)
		return
	}
	if !found || !slices.Contains(svcIDs, fn.ServiceID) { // another provider's function is none of this token's
		proxyError(w, http.StatusNotFound, "function not found")
		return
	}
	es, fs, err := h.functionSvc(ctx, fn.ServiceID)
	if err != nil {
		unavailable("build the service", err)
		return
	}
	switch {
	case !functionsOffered(es, fs):
		proxyError(w, http.StatusForbidden, errFunctionsOff.Msg)
		return
	case fn.Suspended:
		proxyError(w, http.StatusForbidden, "the function is suspended")
		return
	case fn.CurrentDeploymentID == "" && fn.DeployingID == "":
		proxyError(w, http.StatusConflict, "the function has no working deployment")
		return
	}
	proj, err := h.svc.repo.FindByID(ctx, fn.ProjectID)
	if err != nil {
		unavailable("read the project", err)
		return
	}
	if proj == nil {
		proxyError(w, http.StatusNotFound, "function not found")
		return
	}
	// A project that cannot be billed (disabled, in deletion, its billing profile suspended) is refused in one
	// word, whatever the tenant's API says of it: the proxy must not ask again, and has no use for the reason.
	if err := h.sandboxBillingGate(ctx, proj); err != nil {
		var herr *httpx.HTTPError
		var forbidden sandboxForbidden
		if errors.As(err, &forbidden) || (errors.As(err, &herr) && herr.Status < http.StatusInternalServerError) {
			proxyError(w, http.StatusForbidden, "the function's project cannot be billed")
			return
		}
		unavailable("read the project's billing", err)
		return
	}
	// The floor never exceeds one instance, so max(1, floor) is one. A function that runs more (the cluster lost
	// them, or the proxy's view is a moment old) keeps them.
	if err = h.functionUsageGate(ctx, &fnCaller{proj: proj, es: es, fs: fs}); err == nil {
		err = h.scaleFunction(ctx, fs, &fn, max(1, fn.Instances))
	}
	if err != nil {
		status, msg := http.StatusServiceUnavailable, "functions are not available"
		var herr *httpx.HTTPError
		if s, m, ok := functionHTTPError(err); ok {
			status, msg = s, m
		} else if errors.As(err, &herr) {
			status, msg = herr.Status, herr.Msg
		}
		slog.Warn("function wake failed", "function", id, "status", status, "err", err)
		proxyError(w, status, msg)
		return
	}
	httpx.OK(w, map[string]any{"functionID": id, "instances": fn.Instances})
}

// maxActivityIDs bounds one activity report.
const maxActivityIDs = 1000

// functionActivity: POST /internal/functions/activity {ids, every, replica, partial, load}, sent by sandbox-proxy
// every `every` seconds with the functions that had a request, or have one in flight, since its last report: all of
// them, or none. It stamps their lastActiveAt, which is what keeps them from the reaper. Only functions of the
// providers the token belongs to are touched: a report can name any id, and reaches none of another provider's.
//
// load is {id: {peak, requests}}: per function, the most requests the replica had in flight at once in the period
// and how many it started. A report that is not partial is kept with the replica's record (fnProxy), and the
// provider's running functions are then scaled to the load of all its replicas (scaleOnLoad). partial is a report of
// some functions only: the first request after a quiet spell, sent at once; it carries no load and is not the
// replica's word on anything else.
//
// Whoever holds the token can therefore keep every function of the provider awake and, through the load, at its
// maxInstances, which its owner pays for; and writes the records the operator's gate reads as "a proxy reports". It
// reads nothing and changes no function's configuration.
func (h *Handler) functionActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svcIDs := h.functionTokenServices(ctx, proxyBearer(r))
	if len(svcIDs) == 0 {
		proxyError(w, http.StatusUnauthorized, "invalid wake token")
		return
	}
	var b struct {
		IDs     []string          `json:"ids"`
		Every   int               `json:"every"`
		Replica string            `json:"replica"`
		Partial bool              `json:"partial"`
		Load    map[string]fnLoad `json:"load"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxProxyBody)).Decode(&b); err != nil {
		proxyError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(b.IDs) > maxActivityIDs || len(b.Load) > maxActivityIDs {
		proxyError(w, http.StatusBadRequest, "too many ids in one report")
		return
	}
	if len(b.Replica) > 253 || strings.Contains(b.Replica, "/") || b.Replica == fnReaperID { // a pod name; it is part of a record id
		proxyError(w, http.StatusBadRequest, "replica is not a pod name")
		return
	}
	ids := b.IDs[:0]
	for _, id := range b.IDs {
		if function.ValidID(id) {
			ids = append(ids, id)
		}
	}
	if b.Every <= 0 || b.Every > activityEveryDefault {
		b.Every = activityEveryDefault
	}
	for id, l := range b.Load {
		if !function.ValidID(id) || l.Peak <= 0 {
			delete(b.Load, id)
		}
	}
	if h.fns == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The functions first, and the replica with the same time (as PostgreSQL keeps it): a report is never on record
	// as later than the stamp of a function it named.
	now := time.Now().UTC().Truncate(time.Microsecond)
	var err error
	if len(ids) > 0 {
		_, err = h.fns.SetFields(ctx, pgdoc.M{"_id": pgdoc.M{"$in": ids}, "serviceId": pgdoc.M{"$in": svcIDs}}, pgdoc.M{"lastActiveAt": now}, nil)
	}
	for i := 0; err == nil && !b.Partial && i < len(svcIDs); i++ {
		err = h.fnProxies().Upsert(ctx, svcIDs[i]+"/"+b.Replica, fnProxy{ServiceID: svcIDs[i], At: now, Every: b.Every, Load: b.Load})
	}
	if err != nil {
		slog.Error("function activity", "err", err)
		proxyError(w, http.StatusServiceUnavailable, "functions are not available")
		return
	}
	if !b.Partial && h.functionFor != nil {
		// Not cut short by a proxy that gives up on its report: a scale change is finished.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		for _, id := range svcIDs {
			h.scaleOnLoad(sctx, id)
		}
		cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

// functionScaleDownWindow is the stabilisation of a scale down: a function steps down one instance at a time, once
// neither its load has asked for every instance it has (busyAt) nor its count changed (scaledAt) for this long. Up is
// at once.
const functionScaleDownWindow = 60 * time.Second

// functionLoadGap: a function above one instance whose load was not looked at for this long (no report was decided
// on: the API, its database or the path from the proxies was down) was not seen calm for that time, and the first
// report after it is one replica's alone. Its window starts again. Longer than one lost report of a lone
// sandbox-proxy (two periods of 5 s).
const functionLoadGap = 15 * time.Second

// functionUnplacedHold is how long the load adds no instance to a function after instances the cluster could not
// place were taken off its count (trimUnplaced): without it the next report would ask for them again, and they would
// be billed again.
const functionUnplacedHold = 5 * time.Minute

// functionMax is the most instances the function may run under the location's current config: its maxInstances (one
// for a function that names none), held to the location's cap.
func functionMax(fn *functionDoc, fc sandbox.FunctionsConfig) int {
	return max(1, min(fn.MaxInstances, fc.MaxInstances))
}

// loadTarget is the count a running function is moved to for its load: desired = ceil(peak / concurrency), held
// between max(floor, 1) and maxN; all the way up at once, one instance down at a time and only when calm (the
// stabilisation window has passed). busy says the load asks for every instance the function has. Zero is never the
// load's to decide: a function stops by the idle rule (functionTarget), and starts by a wake.
func loadTarget(instances, peak, concurrency, floor, maxN int, calm bool) (target int, busy bool) {
	desired := min(max((peak+concurrency-1)/max(concurrency, 1), floor, 1), maxN)
	switch {
	case instances > maxN: // its cap was lowered
		return maxN, true
	case desired > instances:
		return desired, true
	case desired < instances && calm:
		return instances - 1, false
	}
	return instances, desired == instances
}

// scaleOnLoad scales the provider's running functions to their load: the sum, over every sandbox-proxy replica heard
// from within two of its periods, of each function's peak of requests in flight (functionActivity). Functions with a
// rollout in progress are left alone, as by the reaper, and so is one at zero: a cold start is a wake.
//
// One API replica decides at a time per provider: a PostgreSQL advisory lock, tried and not waited for, since the
// replica that holds it decides on the same records; the next report decides again. Each change is a compare-and-set
// on the count it was decided on (rescaleFunction), so it never undoes what a wake, the reaper or a deploy did
// meanwhile.
//
// A function above one instance whose load was not looked at for functionLoadGap is not stepped down on this report,
// and its window starts again: nobody saw it calm meanwhile, and after a gap the first report is one replica's, the
// others' load on record being too old to count. One that had instances the cluster could not place gets no more for
// functionUnplacedHold (trimUnplaced).
//
// It has never run on a cluster, and concurrency is not enforced per instance: the proxy picks an instance at random.
func (h *Handler) scaleOnLoad(ctx context.Context, serviceID string) {
	es, fs, err := h.functionSvc(ctx, serviceID)
	if err != nil || !functionsOffered(es, fs) {
		return // the reaper stops what runs where functions are not offered
	}
	_, err = h.fns.DB().WithAdvisoryLock(ctx, "function-scale:"+serviceID, true, func() error {
		return h.scaleOnLoadLocked(ctx, fs, serviceID)
	})
	if err != nil {
		slog.Warn("function scale on load", "service", serviceID, "err", err)
	}
}

func (h *Handler) scaleOnLoadLocked(ctx context.Context, fs *function.Service, serviceID string) error {
	now := time.Now().UTC()
	proxies, err := h.proxiesOf(ctx, serviceID, now)
	if err != nil {
		return err
	}
	peaks := map[string]int{}
	for _, p := range proxies {
		if now.Sub(p.At) <= 2*time.Duration(p.Every)*time.Second { // older is no longer its load
			for id, l := range p.Load {
				peaks[id] += l.Peak
			}
		}
	}
	// The functions with load, and the ones above one instance (their load may have gone).
	which := []pgdoc.M{{"instances": pgdoc.M{"$gt": 1}}}
	if len(peaks) > 0 {
		which = append(which, pgdoc.M{"_id": pgdoc.M{"$in": slices.Collect(maps.Keys(peaks))}})
	}
	var fns []functionDoc
	if err := h.fns.Find(ctx, pgdoc.M{"serviceId": serviceID, "deployingId": nil, "instances": pgdoc.M{"$gt": 0}, "$or": which}, &fns); err != nil {
		return err
	}
	fc := fs.Config()
	billable := map[string]bool{}
	var busy []string
	for i := range fns {
		fn := &fns[i]
		gap := fn.Instances > 1 && (fn.LoadAt == nil || now.Sub(*fn.LoadAt) > functionLoadGap)
		calm := !gap
		for _, t := range []*time.Time{fn.BusyAt, fn.ScaledAt} {
			calm = calm && (t == nil || now.Sub(*t) >= functionScaleDownWindow)
		}
		target, full := loadTarget(fn.Instances, peaks[fn.ID], cmp.Or(fn.Concurrency, function.DefaultConcurrency), functionFloor(fn, fc), functionMax(fn, fc), calm)
		if (full || gap) && fn.Instances > 1 {
			busy = append(busy, fn.ID)
		}
		held := fn.UnplacedAt != nil && now.Sub(*fn.UnplacedAt) < functionUnplacedHold
		if target == fn.Instances || fn.Suspended || (target > fn.Instances && (held || !h.projectBillable(ctx, fn.ProjectID, billable))) {
			continue
		}
		if err := h.rescaleFunction(ctx, fs, fn, target, now); err != nil {
			slog.Warn("function scale on load", "function", fn.ID, "instances", target, "err", err)
			continue
		}
		slog.Info("function scaled on load", "function", fn.ID, "project", fn.ProjectID, "instances", target, "peak", peaks[fn.ID])
	}
	if len(busy) > 0 {
		if _, err := h.fns.SetFields(ctx, pgdoc.M{"_id": pgdoc.M{"$in": busy}}, pgdoc.M{"busyAt": now}, nil); err != nil {
			return err
		}
	}
	// Looked at: also the ones this pass took above one instance, not the ones that roll out.
	_, err = h.fns.SetFields(ctx, pgdoc.M{"serviceId": serviceID, "deployingId": nil, "instances": pgdoc.M{"$gt": 1}}, pgdoc.M{"loadAt": now}, nil)
	return err
}

// rescaleFunction moves a running function from the count it was read with to n (both above zero) as scaleFunction
// does, up with the record first and down with the cluster first, but as a compare-and-set on that count: a function
// another writer changed meanwhile is left as that one left it. It stamps no activity: load is not, the reports are.
// The count and the row that is billed change in one transaction. Neither count is zero, so it makes no boundary of
// an interval.
//
// Billed are the instances asked for, also one the cluster cannot place yet: the deploy sync takes those off the
// count again (trimUnplaced).
func (h *Handler) rescaleFunction(ctx context.Context, fs *function.Service, fn *functionDoc, n int, now time.Time) error {
	cas := func() (won bool, err error) {
		err = h.fns.DB().WithTx(ctx, func(tc context.Context) error {
			won, err = h.fns.SetFieldsOne(tc, pgdoc.M{"_id": fn.ID, "instances": fn.Instances, "deployingId": nil},
				pgdoc.M{"instances": n, "lastInstances": n, "scaledAt": now}, nil)
			if err != nil || !won {
				return err
			}
			var cur functionDoc
			if found, err := h.fns.Get(tc, fn.ID, &cur); err != nil || !found {
				return cmp.Or(err, function.ErrNotFound)
			}
			*fn = cur
			_, err = h.syncFunctionRow(tc, fn, now)
			return err
		})
		return won && err == nil, err
	}
	if n > fn.Instances {
		if won, err := cas(); err != nil || !won {
			return err
		}
		return fs.SetScale(ctx, fn.ProjectID, fn.ID, n) // on an error: recorded and billed, not started; the deploy sync starts it
	}
	if err := fs.SetScale(ctx, fn.ProjectID, fn.ID, n); err != nil {
		return err
	}
	won, err := cas()
	if err == nil && !won { // changed meanwhile: the cluster goes back to what the record says
		var cur functionDoc
		if found, gerr := h.fns.Get(ctx, fn.ID, &cur); gerr == nil && found {
			err = fs.SetScale(ctx, fn.ProjectID, fn.ID, cur.Instances)
		}
	}
	return err
}

// functionIdleOut is when the function's idle time runs out: its last activity plus its idleSeconds, held to the
// location's current bounds (so narrowing them needs no redeploy). The zero time for a function that never had any.
func functionIdleOut(fn *functionDoc, fc sandbox.FunctionsConfig) time.Time {
	if fn.LastActiveAt == nil {
		return time.Time{}
	}
	return fn.LastActiveAt.Add(time.Duration(min(max(fn.IdleSeconds, fc.MinIdleSeconds), fc.MaxIdleSeconds)) * time.Second)
}

// functionTarget is the instance count the reaper wants for a function that may run: zero while suspended, the floor
// when it is below it or has been idle past its idle time (from any count: idle is idle), its cap when it runs more,
// otherwise what it has; what it has between the floor and the cap is the load's (scaleOnLoad). floor is
// functionFloor; fc is the location's current config.
func functionTarget(fn *functionDoc, fc sandbox.FunctionsConfig, floor int, now time.Time) int {
	if fn.Suspended {
		return 0
	}
	if fn.Instances < floor {
		return floor
	}
	if fn.Instances > floor && now.After(functionIdleOut(fn, fc)) {
		return floor
	}
	return min(fn.Instances, functionMax(fn, fc)) // its cap was lowered: down to it
}

// projectBillable reports whether a project's functions may run (the sandbox billing gate). cache holds the answer
// per project for one run.
func (h *Handler) projectBillable(ctx context.Context, projectID string, cache map[string]bool) bool {
	ok, seen := cache[projectID]
	if !seen {
		proj, err := h.svc.repo.FindByID(ctx, projectID)
		ok = err == nil && proj != nil && h.sandboxBillingGate(ctx, proj) == nil
		cache[projectID] = ok
	}
	return ok
}

// ReapFunctions is the scheduler's function reaper, every 15 s: it scales a function that has been idle past its idle
// time down to its floor (usually zero), and one that is below its floor up to it. Functions with a rollout in
// progress are left alone.
//
// Two things stop a function whatever its traffic and its floor, as suspending it does: a location that no longer
// offers functions (the block switched off or not valid, a disabled provider), and a project that cannot be billed.
// A wake is refused for both, but a function that is warm never needs a wake: without this it would run, and be
// charged, for as long as it gets requests. So after "off" every function of the provider is at zero within one
// period.
//
// A function that idled out is ended at the moment its idle time ran out, not at the moment the reaper got to it: a
// reaper that was away (the scheduler off, the portal down) does not make its owner pay for the time nobody looked.
// And the first run after such a gap stops nothing for being idle (reaperRanLately): the reports that keep serving
// functions awake were away as long.
//
// ponytail: reads every function without a rollout on each run; select in SQL (running, or a floor above zero) when
// the function count makes the scan matter.
func (h *Handler) ReapFunctions(ctx context.Context) {
	if h.fns == nil || h.functionFor == nil {
		return
	}
	heard := h.reaperRanLately(ctx, time.Now().UTC())
	var fns []functionDoc
	if err := h.fns.Find(ctx, pgdoc.M{"deployingId": nil}, &fns); err != nil {
		slog.Warn("function reaper: list", "err", err)
		return
	}
	type location struct {
		fs      *function.Service // nil = unavailable this run
		offered bool
	}
	services := map[string]location{}
	billable := map[string]bool{}
	held := 0
	for i := range fns {
		fn := &fns[i]
		loc, seen := services[fn.ServiceID]
		if !seen {
			es, fs, err := h.functionSvc(ctx, fn.ServiceID)
			if err != nil {
				slog.Warn("function reaper: service", "service", fn.ServiceID, "err", err)
			} else {
				loc = location{fs: fs, offered: functionsOffered(es, fs)}
			}
			services[fn.ServiceID] = loc
		}
		if loc.fs == nil {
			continue
		}
		fc := loc.fs.Config()
		// target is the count the function should have, and how its interval ends when that is zero.
		target := func(f *functionDoc) (int, fnStop) {
			floor := functionFloor(f, fc)
			if !loc.offered || ((f.Instances > 0 || floor > 0) && !h.projectBillable(ctx, f.ProjectID, billable)) {
				return 0, fnStop{}
			}
			now := time.Now()
			n := functionTarget(f, fc, floor, now)
			if out := functionIdleOut(f, fc); !f.Suspended && f.Instances > floor && n == floor && now.After(out) {
				return n, fnStop{idle: true, at: out}
			}
			return n, fnStop{}
		}
		if n, _ := target(fn); n == fn.Instances {
			continue
		}
		// The list is seconds old by now. Decide again on a fresh read, so a function that was woken meanwhile is
		// not scaled down under the request that woke it.
		var cur functionDoc
		if found, err := h.fns.Get(ctx, fn.ID, &cur); err != nil || !found || cur.DeployingID != "" {
			continue
		}
		want, stop := target(&cur)
		if want == cur.Instances {
			continue
		}
		if stop.idle && !heard {
			held++
			continue
		}
		var err error
		if want == 0 {
			err = h.stopFunction(ctx, loc.fs, &cur, stop)
		} else {
			err = h.scaleFunction(ctx, loc.fs, &cur, want)
		}
		if err != nil {
			slog.Warn("function reaper: scale", "function", cur.ID, "instances", want, "err", err)
			continue
		}
		slog.Info("function reaper: scaled", "function", cur.ID, "project", cur.ProjectID, "instances", cur.Instances)
	}
	if held > 0 {
		slog.Info("function reaper: first run after a gap, idle functions are left until the next run", "functions", held)
	}
}

// FunctionsExist reports whether a sandbox provider holds a function record: what a platform run asks before it
// takes functions away from sandboxes that stay.
func (h *Handler) FunctionsExist(ctx context.Context, providerID string) (bool, error) {
	if h.fns == nil {
		return false, nil
	}
	return h.fns.Exists(ctx, pgdoc.M{"serviceId": providerID})
}

// EndFunctions ends every running interval of a provider's functions in the records, and only there: for a platform
// run that is about to delete the cluster they ran on (sandboxes off, a rebuild of the sandbox cluster, Remove), or
// finds it gone. No cluster call is made. The records stay, at zero instances: nothing of them is billed from here
// on, and a cluster that comes back gets its Deployments again from them (the deploy sync scales it to the record).
// The provider must no longer offer functions, or the reaper raises the ones with a floor again.
func (h *Handler) EndFunctions(ctx context.Context, providerID string) error {
	if h.fns == nil {
		return nil
	}
	var fns []functionDoc
	if err := h.fns.Find(ctx, pgdoc.M{"serviceId": providerID, "instances": pgdoc.M{"$gt": 0}}, &fns); err != nil {
		return err
	}
	var errs []error
	for i := range fns {
		if err := h.recordInstances(ctx, &fns[i], 0, fnStop{}); err != nil {
			errs = append(errs, fmt.Errorf("function %s: %w", fns[i].ID, err))
		}
	}
	return errors.Join(errs...)
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
