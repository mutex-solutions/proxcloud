package project

// cloud_function_deploy.go — the deploy flow of a function (cloud_function.go):
//
//	startDeployment   claim the function, record the deployment DEPLOYING, apply it, warm one instance
//	watchDeployment   in-process poller: every 2 s, for about 130 s per instance that is replaced
//	settleDeployment  rollout done → ACTIVE; failed or out of time → FAILED
//	finishDeployment  bring the function in line with the settled record: adopt the snapshot, or go
//	                  back to the one that was serving
//
// and the job that keeps record, row and cluster equal:
//
//	SyncFunctionDeployments  settle the rollouts whose poller died, then reconcileFunctions per provider
//
// The record's state change is the commit point and finishDeployment can be repeated, so a crash anywhere can be
// repaired from the records. The claim on the function and the record's first state are written together: there is no
// claim without its record.

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

const (
	// keepDeployments is how many deployment records a function keeps (newest first).
	keepDeployments = 10
	// functionStuckAfter: a rollout still unsettled this long after it started (per instance, rolloutTime) has lost
	// its poller (an API restart) and is settled by the deploy sync.
	functionStuckAfter = 3 * time.Minute
	// functionStaleFailure: a progress-deadline failure cannot be real this soon after a rollout started; one seen
	// earlier is the previous rollout's condition, not yet replaced.
	functionStaleFailure = 30 * time.Second
	// functionUnreadyAfter is how long after the deploy sync first saw a running interval without a ready instance
	// (or instances the cluster has not placed) it acts on it: its next run, a minute later. Half a period, so a
	// run that is a little early still counts as the next one.
	functionUnreadyAfter = 30 * time.Second
)

// functionPollEvery is the rollout poll period, and functionRolloutBound how long the poller watches the rollout of
// one instance: the Deployment's own progress deadline (120 s) plus room to observe it (variables so tests need not
// wait for them).
var (
	functionPollEvery    = 2 * time.Second
	functionRolloutBound = 130 * time.Second
)

// rolloutTime is the bound d of a rollout of one instance for a function that runs so many: they are replaced one
// after another (maxSurge 1, maxUnavailable 0), and the Deployment's progress deadline starts again with each one
// that becomes ready. A rollout that stops making progress is failed by that deadline (Status.Failed) long before
// this runs out.
func rolloutTime(d time.Duration, instances int) time.Duration {
	return d * time.Duration(max(1, instances))
}

// fnClaimFields are what a function holds only while a rollout is in progress.
var fnClaimFields = []string{"deployingId", "pendingSecrets", "pendingRegistryPassword"}

// snapshotFields are the fields of a snapshot as a record holds them.
func snapshotFields(s function.Snapshot) pgdoc.M {
	return pgdoc.M{"image": s.Image, "port": s.Port, "cpuMillis": s.CPUMillis, "memoryMi": s.MemoryMi, "env": s.Env, "command": s.Command, "args": s.Args}
}

// startDeployment rolls the function to snap as a new deployment and returns once the rollout is under way; the
// outcome is recorded by the poller. errFunctionBusy when another rollout is in progress. rollbackOf is the seq of the
// deployment whose snapshot this is, 0 for a new config.
//
// secrets is the whole sealed secret set of a snapshot that changes them (snap.SecretsVersion is not the function's):
// it waits in the record as pendingSecrets until the rollout settles. Otherwise it is not looked at and the rollout
// runs with the serving secrets. registryPassword is the same for a snapshot with new registry credentials
// (snap.RegistryAuth has another version).
//
// before, when given, runs inside the transaction of the claim and ahead of it: the caller's quota check and what it
// writes with the rollout. Its error ends the deployment with nothing claimed and nothing recorded.
func (h *Handler) startDeployment(ctx context.Context, fs *function.Service, fn *functionDoc, snap function.Snapshot, rollbackOf int, secrets map[string]fnSecret, registryPassword *fnSecret, before func(tc context.Context) error) error {
	// Nothing is claimed for a snapshot whose values do not open: its apply could only fail, and so could the restore
	// of what was serving.
	probe := *fn
	if snap.SecretsVersion != fn.SecretsVersion {
		probe.PendingSecrets = secrets
	}
	if registryVersion(snap.RegistryAuth) != registryVersion(fn.RegistryAuth) {
		probe.PendingRegistryPassword = registryPassword
	}
	if _, err := h.secretValues(&probe, snap); err != nil {
		return err
	}
	if _, err := h.registryPassword(&probe, snap); err != nil {
		return err
	}
	now := time.Now().UTC()
	dep := &functionDeployment{
		ID: pgdoc.NewID(), FunctionID: fn.ID, ProjectID: fn.ProjectID, Seq: fn.DeploySeq + 1,
		Snapshot: snap, RollbackOf: rollbackOf, State: depDeploying, CreatedAt: &now,
	}
	// The claim also holds the secrets and the registry credentials the snapshot was built on: a rollout that
	// settled since the caller read the function may have changed them, and this one would undo that.
	var basis, registryBasis any // nil = none (the record has no version then)
	if fn.SecretsVersion != "" {
		basis = fn.SecretsVersion
	}
	if fn.RegistryAuth != nil {
		registryBasis = fn.RegistryAuth.Version
	}
	claim := pgdoc.M{"_id": fn.ID, "deployingId": nil, "secretsVersion": basis, "registryAuth.version": registryBasis}
	set := pgdoc.M{"deployingId": dep.ID, "deploySeq": dep.Seq, "updatedAt": now}
	changes := snap.SecretsVersion != fn.SecretsVersion
	if changes {
		set["pendingSecrets"] = secrets
	}
	newCredentials := snap.RegistryAuth != nil && snap.RegistryAuth.Version != registryVersion(fn.RegistryAuth)
	if newCredentials {
		set["pendingRegistryPassword"] = registryPassword
	}
	// The claim, the record and the row, or none of them: a claim whose record is not DEPLOYING is one nothing would
	// ever release, and from the moment the new snapshot may run its size is what the row is billed at. The row is
	// written from the record as the claim left it, not from the caller's copy read before it: a scale change may
	// have come between.
	err := h.fns.DB().WithTx(ctx, func(tc context.Context) error {
		if before != nil {
			if err := before(tc); err != nil {
				return err
			}
		}
		won, err := h.fns.SetFieldsOne(tc, claim, set, []string{"lastError"})
		if err != nil {
			return err
		}
		if !won {
			if exists, err := h.fns.Exists(tc, pgdoc.M{"_id": fn.ID}); err == nil && !exists {
				return function.ErrNotFound
			}
			return errFunctionBusy
		}
		if _, err = h.fnDeps.InsertOne(tc, dep); err != nil {
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
	if err != nil {
		return err
	}
	if err = h.applyFunction(ctx, fs, fn, snap); err == nil && !fn.Suspended {
		// Apply never changes the scale, and a rollout at zero instances proves nothing: every deploy warms one
		// instance. One that runs several rolls them all, one surge pod at a time (function.BuildDeployment), and
		// keeps them.
		err = h.scaleFunction(ctx, fs, fn, max(1, fn.Instances))
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		h.closeDeployment(cleanup, fs, dep, depFailed, "the rollout could not be started")
		fn.DeployingID, fn.PendingSecrets, fn.PendingRegistryPassword = "", nil, nil
		return err
	}
	h.pruneDeployments(ctx, fn)
	go h.watchDeployment(fs, dep, fn.Instances)
	return nil
}

// secretValues are the secrets snap runs with, in the clear: the set its rollout switches to when it changes them,
// the serving set otherwise (a rollback, a restore after a failed rollout).
func (h *Handler) secretValues(fn *functionDoc, snap function.Snapshot) (map[string]string, error) {
	if snap.SecretsVersion != fn.SecretsVersion {
		return h.openSecrets(fn, fn.PendingSecrets)
	}
	return h.openSecrets(fn, fn.Secrets)
}

// registryPassword is the password snap pulls its image with, in the clear ("" without credentials), picked like
// secretValues.
func (h *Handler) registryPassword(fn *functionDoc, snap function.Snapshot) (string, error) {
	p := fn.RegistryPassword
	if registryVersion(snap.RegistryAuth) != registryVersion(fn.RegistryAuth) {
		p = fn.PendingRegistryPassword
	}
	if p == nil || snap.RegistryAuth == nil {
		return "", nil
	}
	return h.openValue(fn, fnRegistryPasswordName, *p)
}

// applyFunction applies snap, with its secrets and registry credentials, to the function's Deployment, unless the
// function was deleted meanwhile (ErrNotFound). An apply that lands after functionDelete removed the Deployment
// would bring it back with a ready instance and no record: routed by the proxy, never billed, never stopped. So the
// record is looked for after the apply, and without one the Deployment is removed again; functionDelete does the
// same from its side for an apply that landed just before. An apply that failed is no exception: it may have written
// the Secret, which nothing else would ever empty.
//
// Values that do not open (errFunctionSealed) end it before anything reaches the cluster: an instance never starts
// with a part of its secrets.
func (h *Handler) applyFunction(ctx context.Context, fs *function.Service, fn *functionDoc, snap function.Snapshot) error {
	secrets, err := h.secretValues(fn, snap)
	if err != nil {
		return err
	}
	password, err := h.registryPassword(fn, snap)
	if err != nil {
		return err
	}
	aerr := fs.Apply(ctx, fn.identity(), snap, secrets, password)
	if exists, err := h.fns.Exists(ctx, pgdoc.M{"_id": fn.ID}); err != nil || exists {
		return cmp.Or(aerr, err)
	}
	if err := fs.Delete(ctx, fn.ProjectID, fn.ID); err != nil {
		return err
	}
	return function.ErrNotFound
}

// prunableDeployments picks the records to delete from a function's deployments (newest first): everything past the
// newest keepDeployments, except the one that is serving.
func prunableDeployments(deps []functionDeployment, currentID string) []string {
	var out []string
	for i := range deps {
		if i >= keepDeployments && deps[i].ID != currentID {
			out = append(out, deps[i].ID)
		}
	}
	return out
}

func (h *Handler) pruneDeployments(ctx context.Context, fn *functionDoc) {
	var deps []functionDeployment
	if err := h.fnDeps.Find(ctx, pgdoc.M{"functionId": fn.ID}, &deps, pgdoc.Sort(pgdoc.DescK("seq", pgdoc.KNum))); err != nil {
		slog.Warn("function: list deployments to prune", "function", fn.ID, "err", err)
		return
	}
	if ids := prunableDeployments(deps, fn.CurrentDeploymentID); len(ids) > 0 {
		if _, err := h.fnDeps.DeleteMany(ctx, pgdoc.M{"_id": pgdoc.M{"$in": ids}}); err != nil {
			slog.Warn("function: prune deployments", "function", fn.ID, "err", err)
		}
	}
}

// watchDeployment polls one rollout, of so many instances, until it settles, in the API process that started it.
func (h *Handler) watchDeployment(fs *function.Service, dep *functionDeployment, instances int) {
	ctx, cancel := context.WithTimeout(context.Background(), rolloutTime(functionRolloutBound, instances))
	defer cancel()
	t := time.NewTicker(functionPollEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if h.settleDeployment(ctx, fs, dep, false) {
				return
			}
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			h.settleDeployment(last, fs, dep, true)
			return
		}
	}
}

// settleDeployment looks at the rollout once. It reports true when the deployment is settled (ACTIVE or FAILED) and
// the function brought in line with it. final ends the wait: a rollout that is still in progress, or cannot be
// observed, is failed.
//
// What a failed rollout is recorded with is one of function.Reasons, or the portal's own words: the cluster's own
// text (the kubelet's of a failed pull, which quotes what the registry was offered) goes to the operator's log and
// nowhere else.
func (h *Handler) settleDeployment(ctx context.Context, fs *function.Service, dep *functionDeployment, final bool) bool {
	if dep.State != depDeploying {
		h.finishDeployment(ctx, fs, dep) // settled before; a crash may have left the function behind
		return true
	}
	st, err := fs.Status(ctx, dep.ProjectID, dep.FunctionID)
	failed := func(reason string) {
		if st.Detail != "" {
			slog.Warn("function: rollout failed", "function", dep.FunctionID, "deployment", dep.ID, "reason", st.Reason, "detail", st.Detail)
		}
		h.closeDeployment(ctx, fs, dep, depFailed, reason)
	}
	switch {
	case errors.Is(err, function.ErrNotFound):
		h.closeDeployment(ctx, fs, dep, depFailed, "the function no longer exists")
	case err != nil:
		if !final {
			return false
		}
		slog.Warn("function: rollout status", "function", dep.FunctionID, "err", err)
		h.closeDeployment(ctx, fs, dep, depFailed, "the rollout could not be observed")
	case st.RolloutDone:
		// The digest the new instances run, for a later rollback to this deployment. Asked before the outcome is
		// committed: afterwards another rollout may be replacing them.
		if digest, derr := fs.ImageDigest(ctx, dep.ProjectID, dep.FunctionID); derr != nil {
			slog.Warn("function: image digest of a deployment", "function", dep.FunctionID, "deployment", dep.ID, "err", derr)
		} else {
			dep.ImageDigest = digest
		}
		h.closeDeployment(ctx, fs, dep, depActive, "")
	case st.Failed && (final || dep.CreatedAt == nil || time.Since(*dep.CreatedAt) > functionStaleFailure):
		failed(st.Reason)
	case final:
		failed("the rollout did not finish in time: " + st.Reason)
	default:
		return false
	}
	return true
}

// closeDeployment commits the outcome of a rollout (only the first writer wins) and brings the function in line with
// whatever was committed.
func (h *Handler) closeDeployment(ctx context.Context, fs *function.Service, dep *functionDeployment, state, message string) {
	now := time.Now().UTC()
	set := pgdoc.M{"state": state, "finishedAt": now}
	if message != "" {
		set["message"] = message
	}
	if state == depActive && dep.ImageDigest != "" {
		set["imageDigest"] = dep.ImageDigest
	}
	won, err := h.fnDeps.SetFieldsOne(ctx, pgdoc.M{"_id": dep.ID, "state": depDeploying}, set, nil)
	if err != nil {
		slog.Error("function: record deployment outcome", "function", dep.FunctionID, "deployment", dep.ID, "err", err)
		return // still DEPLOYING: settled again from the record
	}
	if won {
		dep.State, dep.Message, dep.FinishedAt = state, message, &now
	} else if found, err := h.fnDeps.Get(ctx, dep.ID, dep); err != nil || !found {
		return // settled elsewhere and unreadable, or the function was deleted with its records
	}
	h.finishDeployment(ctx, fs, dep)
}

// finishDeployment brings the function in line with a settled deployment, if it still holds the claim of that
// deployment (so it is safe to repeat):
//   - ACTIVE: the function adopts the snapshot (and, when the rollout changed them, its secrets and its registry
//     credentials) and the deployment becomes the serving one;
//   - FAILED: the cluster goes back to the snapshot and the secrets that were serving (the function record still
//     holds them). With nothing healthy to go back to, the function is scaled to zero and its interval ended as
//     void: a first deployment that never became ready is billed nothing. It stays FAILED until a deployment
//     succeeds.
//
// Either way the record and its cloud row change in one transaction: the size billing reads is never the size of a
// snapshot the record no longer has.
func (h *Handler) finishDeployment(ctx context.Context, fs *function.Service, dep *functionDeployment) {
	var fn functionDoc
	if found, err := h.fns.Get(ctx, dep.FunctionID, &fn); err != nil || !found || fn.DeployingID != dep.ID {
		return
	}
	now := time.Now().UTC()
	claim := pgdoc.M{"_id": fn.ID, "deployingId": dep.ID}
	l := slog.With("function", fn.ID, "deployment", dep.ID, "seq", dep.Seq)
	// release writes what ends the claim and the row that follows from it.
	release := func(set pgdoc.M, unset []string) (won bool, err error) {
		err = h.fns.DB().WithTx(ctx, func(tc context.Context) error {
			if won, err = h.fns.SetFieldsOne(tc, claim, set, unset); err != nil || !won {
				return err
			}
			var cur functionDoc
			if found, err := h.fns.Get(tc, fn.ID, &cur); err != nil || !found {
				return cmp.Or(err, function.ErrNotFound)
			}
			fn = cur
			_, err = h.syncFunctionRow(tc, &fn, now)
			return err
		})
		return won && err == nil, err
	}
	switch dep.State {
	case depActive:
		if _, err := h.fnDeps.SetFields(ctx, pgdoc.M{"functionId": fn.ID, "state": depActive, "_id": pgdoc.M{"$ne": dep.ID}},
			pgdoc.M{"state": depSuperseded}, nil); err != nil {
			l.Warn("function: supersede deployments", "err", err)
		}
		s := dep.Snapshot
		set := snapshotFields(s)
		set["currentDeploymentId"], set["currentSeq"], set["deployedAt"], set["updatedAt"] = dep.ID, dep.Seq, dep.FinishedAt, now
		// A rollout that is done with instances running has them ready: the interval serves, and billing may
		// charge it from its start.
		if fn.Instances > 0 && fn.ReadyAt == nil {
			set["readyAt"] = now
		}
		unset := append([]string{"lastError"}, fnClaimFields...)
		switch {
		case s.SecretsVersion == fn.SecretsVersion: // the serving secrets stay
		case s.SecretsVersion == "": // absent, not empty: the next rollout's claim matches on it
			unset = append(unset, "secrets", "secretNames", "secretsVersion")
		default:
			set["secrets"], set["secretNames"], set["secretsVersion"] = fn.PendingSecrets, s.SecretNames, s.SecretsVersion
		}
		switch { // the registry credentials, the same way
		case registryVersion(s.RegistryAuth) == registryVersion(fn.RegistryAuth):
		case s.RegistryAuth == nil:
			unset = append(unset, "registryAuth", "registryPassword")
		default:
			set["registryAuth"], set["registryPassword"] = s.RegistryAuth, fn.PendingRegistryPassword
		}
		if won, err := release(set, unset); err != nil || !won {
			l.Warn("function: adopt deployment", "err", err)
			return
		}
		l.Info("function: deployment active")
	case depFailed:
		if fn.CurrentDeploymentID != "" {
			// The pod template that ran, which the cluster takes back without starting or moving a pod.
			if err := h.applyFunction(ctx, fs, &fn, fn.Snapshot); err != nil && !errors.Is(err, function.ErrNotFound) {
				l.Error("function: restore the previous deployment", "err", err)
				return // the claim stays: settled again from the record
			}
		} else if err := h.stopFunction(ctx, fs, &fn, fnStop{void: true}); err != nil && fn.Instances > 0 {
			// A start that never had a ready instance is nobody's cost: its interval is void. The stop is on record
			// even when the cluster did not take it (the deploy sync repeats that call); only a record that could
			// not be written keeps the claim.
			l.Error("function: stop after a failed first deployment", "err", err)
			return
		}
		// The failed snapshot no longer runs: back to the size of the serving one.
		if _, err := release(pgdoc.M{"lastError": dep.Message, "updatedAt": now}, fnClaimFields); err != nil {
			l.Warn("function: release deployment claim", "err", err)
		}
		l.Info("function: deployment failed", "reason", dep.Message)
	}
}

// SyncFunctionDeployments is the scheduler's backstop for functions, every minute at second 20 (before the charge at
// second 30):
//
//   - a rollout whose poller died with its API process (the function still holds the claim long after the rollout
//     started) is settled from the cluster's current state;
//   - the sandbox-proxy replicas not heard from for fnProxyKeep are forgotten;
//   - each provider's cloud rows, function records and cluster are brought back in line where they drifted apart
//     (reconcileFunctions).
//
// Run twice on the same state, the second run changes nothing.
func (h *Handler) SyncFunctionDeployments(ctx context.Context) {
	if h.fns == nil || h.functionFor == nil {
		return
	}
	now := time.Now().UTC()
	var stuck []functionDoc
	if err := h.fns.Find(ctx, pgdoc.M{"deployingId": pgdoc.M{"$exists": true}, "updatedAt": pgdoc.M{"$lt": now.Add(-functionStuckAfter)}}, &stuck); err != nil {
		slog.Warn("function sync: list stuck rollouts", "err", err)
	}
	for i := range stuck {
		fn := &stuck[i]
		_, fs, err := h.functionSvc(ctx, fn.ServiceID)
		if err != nil {
			slog.Warn("function sync: service", "function", fn.ID, "err", err)
			continue
		}
		var dep functionDeployment
		found, err := h.fnDeps.Get(ctx, fn.DeployingID, &dep)
		if err != nil {
			continue
		}
		if !found { // a claim without its record: nothing would ever release it
			_, _ = h.fns.SetFieldsOne(ctx, pgdoc.M{"_id": fn.ID, "deployingId": fn.DeployingID}, pgdoc.M{"updatedAt": now}, fnClaimFields)
			continue
		}
		// Settled as the cluster has it; one still in progress is failed only once the time of all its instances has
		// passed (its poller may be watching it for as long).
		final := fn.UpdatedAt == nil || now.Sub(*fn.UpdatedAt) >= rolloutTime(functionStuckAfter, fn.Instances)
		slog.Info("function sync: settling a rollout without a poller", "function", fn.ID, "deployment", dep.ID, "final", final)
		h.settleDeployment(ctx, fs, &dep, final)
	}
	h.forgetProxies(ctx, now)
	h.reconcileFunctions(ctx, now)
}

// reconcileFunctions brings each provider's cloud rows, function records and cluster back in line. Every writer
// changes them one after the other from a copy it read earlier, so a race or a crash between two steps can leave
// them apart, and nothing else would ever notice:
//
//   - a row whose function record is gone is a deleted function's: it is ended and archived (the archived row is
//     what billing rates to the end of its last interval);
//   - a row that no longer says what its record says (instances, status, billed size, the interval's bounds) is
//     written again from the record: billing reads the row;
//   - a Deployment whose scale is not the recorded one is scaled to the record. The record is what is billed and
//     what the reaper decides on: an instance running at a recorded zero would be neither billed nor ever stopped
//     (the proxy never wakes a function that has a ready pod). This is also what repeats a stop the cluster did not
//     take when it was recorded (scale);
//   - a Deployment without a function record is deleted: an apply that raced the function's delete, or a delete
//     that failed half way. Nothing routes, bills or reaps it otherwise;
//   - a function with a serving deployment and no Deployment on the cluster gets it again from its record, at the
//     recorded count (restoreFunction): a sandbox cluster that was rebuilt comes back with its functions;
//   - a function namespace whose project is gone, or holds no function record any more, is deleted
//     (sweepNamespaces);
//   - a running interval that has a ready instance is stamped readyAt, which is what lets billing charge it; one
//     that is recorded at one instance or more and has none on two runs in a row is ended (settleReadiness): it
//     serves nothing, and its callers' retries would keep it "active", and billed, without end;
//   - a function scaled on its load to more instances than the cluster can place is brought down to the ones that
//     are ready (trimUnplaced): the others are billed and serve nothing.
//
// After a good list of a provider's Deployments its meter heartbeat is stamped: billing may trust the rows of a
// provider whose cluster was seen lately, and writes off the time of one that was not (cloud.MeterStaleAfter).
//
// Functions with a rollout in progress are left to the deploy flow.
//
// ponytail: reads every function row and record, and lists every function Deployment, of each sandbox provider once
// a minute; narrow the queries when the function count makes that matter.
func (h *Handler) reconcileFunctions(ctx context.Context, now time.Time) {
	svcs, err := h.esSvc.List(ctx)
	if err != nil {
		slog.Warn("function sync: list providers", "err", err)
		return
	}
	for i := range svcs {
		if !svcs[i].IsSandbox() {
			continue
		}
		id := svcs[i].ID
		rows, err := h.cloud.FindByServiceAndType(ctx, id, cloud.TypeFunction)
		if err != nil {
			continue
		}
		var fns []functionDoc
		if err := h.fns.Find(ctx, pgdoc.M{"serviceId": id}, &fns); err != nil {
			continue
		}
		byID := make(map[string]*functionDoc, len(fns))
		for j := range fns {
			byID[fns[j].ID] = &fns[j]
		}
		for j := range rows {
			row := &rows[j]
			if fn := byID[row.ExternalID]; fn != nil {
				if functionRowStale(row.Data, h.functionRowData(ctx, fn)) {
					h.resyncFunctionRow(ctx, fn.ID, now)
				}
				continue
			}
			// Asked again: a function created since the records were read has its row already.
			if exists, err := h.fns.Exists(ctx, pgdoc.M{"_id": row.ExternalID}); err != nil || exists {
				continue
			}
			slog.Warn("function sync: ending a cloud row without a function record", "function", row.ExternalID)
			row.Data, row.UpdatedAt = deletedFunctionRow(row.Data, now), &now
			if err := h.cloud.DeleteAndArchive(ctx, row, now); err != nil {
				slog.Warn("function sync: cloud row", "function", row.ExternalID, "err", err)
			}
		}

		// The cluster: only where functions are, or were, in use (a sandbox provider that never offered them need
		// not even grant the read).
		es, fs, err := h.functionSvc(ctx, id)
		if err != nil || (len(fns) == 0 && len(rows) == 0 && !functionsOffered(es, fs)) {
			continue
		}
		live, err := fs.ListAll(ctx)
		if err != nil {
			slog.Warn("function sync: list deployments", "service", id, "err", err)
			continue
		}
		if err := h.cloud.SetMeterFields(ctx, id, pgdoc.M{"lastOkAt": now}); err != nil {
			slog.Warn("function sync: meter heartbeat", "service", id, "err", err)
		}
		for fnID, d := range live {
			fn := byID[fnID]
			if fn == nil {
				// Asked again: a function created since the records were read is not an orphan (its record is
				// written before its Deployment).
				if exists, err := h.fns.Exists(ctx, pgdoc.M{"_id": fnID}); err != nil || exists {
					continue
				}
				slog.Warn("function sync: deleting a Deployment without a function record", "function", fnID, "project", d.ProjectID)
				if err := fs.Delete(ctx, d.ProjectID, fnID); err != nil {
					slog.Warn("function sync: delete orphan", "function", fnID, "err", err)
				}
				continue
			}
			if fn.DeployingID != "" || h.settleReadiness(ctx, fs, fn, d.Ready, now) || h.trimUnplaced(ctx, fs, fn, d, now) || d.Desired == fn.Instances {
				continue
			}
			// A scale change in flight looks the same for a moment: decide on a fresh read.
			var cur functionDoc
			if found, err := h.fns.Get(ctx, fnID, &cur); err != nil || !found || cur.DeployingID != "" || cur.Instances == d.Desired {
				continue
			}
			slog.Warn("function sync: scale drift", "function", fnID, "recorded", cur.Instances, "cluster", d.Desired)
			if err := fs.SetScale(ctx, cur.ProjectID, fnID, cur.Instances); err != nil {
				slog.Warn("function sync: repair scale", "function", fnID, "err", err)
			}
		}
		offered := functionsOffered(es, fs)
		for j := range fns {
			fn := &fns[j]
			if _, has := live[fn.ID]; has || fn.DeployingID != "" {
				continue
			}
			// No Deployment: nothing of it is ready. A recorded interval ends like any other that serves nothing.
			if !h.settleReadiness(ctx, fs, fn, 0, now) && offered {
				h.restoreFunction(ctx, es, fs, fn.ID)
			}
		}
		h.sweepNamespaces(ctx, fs, id, now)
	}
}

// settleReadiness is reconcileFunctions' look at whether a function's running interval serves (ready is the number
// of its ready instances as just listed; the function has no rollout in progress). It reports whether it ended the
// interval.
//
//   - Ready, and not yet stamped: readyAt is written, with the row. From then on billing charges the interval from
//     its start.
//   - Not ready: the run that sees it first only notes when (unreadyAt): the instance may be on its way. The next
//     run, a minute later, ends the interval: as void when it never had a ready instance (no room on the machines, a
//     refusal by the namespace's quota or by admission, an image that cannot be pulled, a container that keeps
//     exiting: none of that is its owner's cost), and as any other interval when it had one. Either way within two
//     minutes of the start, whatever its callers retry: a held request counts as activity, so the idle rule alone
//     would never stop it.
//
// A function with a floor of one is started again by the reaper, and ended again here while nothing becomes ready:
// one attempt every two minutes, none of them billed.
func (h *Handler) settleReadiness(ctx context.Context, fs *function.Service, fn *functionDoc, ready int, now time.Time) bool {
	if fn.Instances == 0 || fn.RunningSince == nil {
		return false
	}
	if ready > 0 {
		if fn.ReadyAt != nil && fn.UnreadyAt == nil {
			return false
		}
		set := pgdoc.M{}
		if fn.ReadyAt == nil {
			set["readyAt"] = now
		}
		if err := h.markInterval(ctx, fn, set, []string{"unreadyAt"}, now); err != nil {
			slog.Warn("function sync: stamp a ready interval", "function", fn.ID, "err", err)
		}
		return false
	}
	if fn.UnreadyAt == nil || now.Sub(*fn.UnreadyAt) < functionUnreadyAfter {
		if fn.UnreadyAt == nil {
			if err := h.markInterval(ctx, fn, pgdoc.M{"unreadyAt": now}, nil, now); err != nil {
				slog.Warn("function sync: note an interval without a ready instance", "function", fn.ID, "err", err)
			}
		}
		return false
	}
	var cur functionDoc // decided on a fresh read, like every scale change here
	if found, err := h.fns.Get(ctx, fn.ID, &cur); err != nil || !found || cur.DeployingID != "" || cur.Instances == 0 ||
		cur.RunningSince == nil || !cur.RunningSince.Equal(*fn.RunningSince) || cur.UnreadyAt == nil {
		return false
	}
	slog.Warn("function sync: ending an interval that has no ready instance", "function", fn.ID, "project", fn.ProjectID, "everReady", cur.ReadyAt != nil)
	if err := h.stopFunction(ctx, fs, &cur, fnStop{void: true}); err != nil {
		slog.Warn("function sync: end an interval that has no ready instance", "function", fn.ID, "err", err)
	}
	return cur.Instances == 0
}

// markInterval writes marks of the running interval fn was read in (readyAt, unreadyAt) and the row that follows,
// under the record's lock and only while the function is still in that interval: a mark is never written onto a
// later one, nor onto a function that has stopped or begun a rollout meanwhile.
func (h *Handler) markInterval(ctx context.Context, fn *functionDoc, set pgdoc.M, unset []string, now time.Time) error {
	return h.fns.DB().WithTx(ctx, func(tc context.Context) error {
		var cur functionDoc
		if found, err := h.fns.GetForUpdate(tc, fn.ID, &cur); err != nil || !found {
			return err
		}
		if cur.Instances == 0 || cur.DeployingID != "" || cur.RunningSince == nil || !cur.RunningSince.Equal(*fn.RunningSince) {
			return nil
		}
		if _, err := h.fns.SetByID(tc, fn.ID, set, unset); err != nil {
			return err
		}
		var next functionDoc
		if found, err := h.fns.Get(tc, fn.ID, &next); err != nil || !found {
			return cmp.Or(err, function.ErrNotFound)
		}
		_, err := h.syncFunctionRow(tc, &next, now)
		return err
	})
}

// restoreFunction applies a function's serving deployment to a cluster that has no Deployment of it (the cluster was
// rebuilt, or the Deployment was deleted behind the portal), at the count on record. Decided on a fresh read: a
// function that is being deleted, or deployed, is left to that. A new Deployment starts with one instance; the scale
// call right after brings it to the record, zero for a function that sleeps.
func (h *Handler) restoreFunction(ctx context.Context, es *externalservice.ExternalService, fs *function.Service, id string) {
	var fn functionDoc
	if found, err := h.fns.Get(ctx, id, &fn); err != nil || !found || fn.DeployingID != "" || fn.CurrentDeploymentID == "" {
		return
	}
	if _, err := fs.Status(ctx, fn.ProjectID, fn.ID); !errors.Is(err, function.ErrNotFound) {
		return // it is there after all, or the cluster cannot say
	}
	proj, err := h.svc.repo.FindByID(ctx, fn.ProjectID)
	if err != nil || proj == nil {
		return // a project that is gone is the deletion's to finish
	}
	slog.Warn("function sync: the cluster has no Deployment of a function on record, applying it again", "function", fn.ID, "project", fn.ProjectID)
	err = h.ensureFunctionNamespace(ctx, &fnCaller{proj: proj, es: es, fs: fs})
	if err == nil {
		err = h.applyFunction(ctx, fs, &fn, fn.Snapshot)
	}
	if err == nil {
		err = fs.SetScale(ctx, fn.ProjectID, fn.ID, fn.Instances)
	}
	if err != nil && !errors.Is(err, function.ErrNotFound) {
		slog.Warn("function sync: apply a function again", "function", fn.ID, "err", err)
	}
}

// functionNamespaceGrace is how old a function namespace without any function record must be before the sweep
// deletes it: a create makes the namespace a moment before it writes the record.
const functionNamespaceGrace = 10 * time.Minute

// sweepNamespaces deletes the provider's function namespaces nothing on record needs: one whose project is gone (a
// deletion that lost its last step), and one that holds no function record and is older than
// functionNamespaceGrace (the last function was deleted; the next create makes it again). With the namespace go
// whatever Deployments, pods and Secrets were left in it.
func (h *Handler) sweepNamespaces(ctx context.Context, fs *function.Service, serviceID string, now time.Time) {
	nss, err := fs.Namespaces(ctx)
	if err != nil {
		slog.Warn("function sync: list namespaces", "service", serviceID, "err", err)
		return
	}
	for _, ns := range nss {
		if has, err := h.fns.Exists(ctx, pgdoc.M{"projectId": ns.ProjectID, "serviceId": serviceID}); err != nil || has {
			continue
		}
		proj, err := h.svc.repo.FindByID(ctx, ns.ProjectID)
		if err != nil || (proj != nil && (ns.Created.IsZero() || now.Sub(ns.Created) < functionNamespaceGrace)) {
			continue
		}
		slog.Warn("function sync: deleting a function namespace nothing on record needs", "service", serviceID, "project", ns.ProjectID, "projectGone", proj == nil)
		if err := fs.DeleteNamespace(ctx, ns.ProjectID); err != nil {
			slog.Warn("function sync: delete namespace", "project", ns.ProjectID, "err", err)
		}
	}
}

// trimUnplaced is reconcileFunctions' step down of a function that is recorded, and billed, at more instances than
// the cluster has placed (d is its Deployment as just listed): the ones with no node, or no pod (the namespace's
// quota, admission). It reports whether it changed the count. A function with no ready instance at all is
// settleReadiness' to end.
//
// The run that sees them first only notes when (unplacedAt): they may be on their way to a node. The next run takes
// them off the count, down to the ready ones, as the load would (rescaleFunction: a compare-and-set on the count as
// it was read, the row follows). From the first sighting until functionUnplacedHold after the last, the load adds no
// instance (scaleOnLoad). Without this the instances that wait keep requests in flight, the load asks for more of
// them up to the function's cap, all of them billed, and no rollout can finish.
//
// ponytail: unplaced instances beside a ready one are billed for the one or two minutes until the second run, and
// again after each hold while the cluster stays full; bill the ready count if that matters.
func (h *Handler) trimUnplaced(ctx context.Context, fs *function.Service, fn *functionDoc, d function.Deployed, now time.Time) bool {
	if fn.DeployingID != "" || fn.Instances < 2 || d.Desired != fn.Instances || d.Ready >= fn.Instances {
		return false
	}
	if st, err := fs.Status(ctx, fn.ProjectID, fn.ID); err != nil || (st.Reason != function.ReasonUnplaced && st.Reason != function.ReasonNotCreated) {
		return false
	}
	trimmed := false
	if seen := fn.UnplacedAt; seen != nil && now.Sub(*seen) >= functionUnreadyAfter && now.Sub(*seen) < 6*functionUnreadyAfter {
		n := max(d.Ready, 1)
		slog.Warn("function sync: taking instances the cluster could not place off the count", "function", fn.ID, "project", fn.ProjectID, "recorded", fn.Instances, "ready", d.Ready)
		if err := h.rescaleFunction(ctx, fs, fn, n, now); err != nil {
			slog.Warn("function sync: take unplaced instances off the count", "function", fn.ID, "err", err)
		}
		trimmed = fn.Instances == n
	}
	if _, err := h.fns.SetByID(ctx, fn.ID, pgdoc.M{"unplacedAt": now}, nil); err != nil {
		slog.Warn("function sync: note unplaced instances", "function", fn.ID, "err", err)
	}
	return trimmed
}

// resyncFunctionRow writes a function's cloud row again from a fresh read of its record.
func (h *Handler) resyncFunctionRow(ctx context.Context, id string, now time.Time) {
	err := h.fns.DB().WithTx(ctx, func(tc context.Context) error {
		var fn functionDoc
		if found, err := h.fns.GetForUpdate(tc, id, &fn); err != nil || !found {
			return err
		}
		slog.Warn("function sync: cloud row out of line with its record", "function", id)
		_, err := h.syncFunctionRow(tc, &fn, now)
		return err
	})
	if err != nil {
		slog.Warn("function sync: cloud row", "function", id, "err", err)
	}
}
