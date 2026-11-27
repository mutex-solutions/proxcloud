package project

// cloud_function_deploy.go — the deploy flow of a function (cloud_function.go):
//
//	startDeployment   claim the function, record the deployment DEPLOYING, apply it, warm one instance
//	watchDeployment   in-process poller: every 2 s, for about 130 s per instance that is replaced
//	settleDeployment  rollout done → ACTIVE; failed or out of time → FAILED
//	finishDeployment  bring the function in line with the settled record: adopt the snapshot, or go
//	                  back to the one that was serving
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

	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

const (
	// keepDeployments is how many deployment records a function keeps (newest first).
	keepDeployments = 10
	// functionStaleFailure: a progress-deadline failure cannot be real this soon after a rollout started; one seen
	// earlier is the previous rollout's condition, not yet replaced.
	functionStaleFailure = 30 * time.Second
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
//     holds them). With nothing healthy to go back to, the function is scaled to zero instead of being billed for a
//     crash loop, and stays FAILED until a deployment succeeds.
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
		} else if err := h.scaleFunction(ctx, fs, &fn, 0); err != nil && !errors.Is(err, function.ErrNotFound) {
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
