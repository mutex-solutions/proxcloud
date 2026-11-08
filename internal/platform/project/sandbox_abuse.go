package project

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

const SandboxAbuseResource = "SANDBOX_ABUSE"

type captureLockKey struct{}
type abuseGuardKey struct{}

type abusePauseGuard struct {
	Sample         sandbox.Sample      `json:"sample"`
	Policy         sandbox.AbusePolicy `json:"policy"`
	ConnectionHash string              `json:"connectionHash"`
}

func abuseGuardFrom(ctx context.Context) *abusePauseGuard {
	g, _ := ctx.Value(abuseGuardKey{}).(*abusePauseGuard)
	return g
}

// authorizeAbuseAction is called under the shared capture lock immediately before
// acting, and again after an automatic pause's image is ready. Host counter freshness
// and inactivity, not a tenant process list or the helper's writable boot marker,
// authorize deletion. No continuing high-CPU requirement during snapshot assembly.
func (h *Handler) authorizeAbuseAction(ctx context.Context, service string, g *abusePauseGuard) error {
	x, p := g.Sample, g.Policy
	fp, pe := h.svc.repo.FindByID(ctx, x.ProjectID)
	fe, se := h.esSvc.Get(ctx, service)
	if pe != nil || se != nil || fp == nil || fe == nil || !fp.HasService(service) || fe.IsDisabled() || !sameAbusePolicy(fe.SandboxConfig().Abuse, p) || fmt.Sprintf("%x", sha256.Sum256([]byte(fe.SandboxConfig().Kubeconfig))) != g.ConnectionHash || fe.SandboxConfig().Envd {
		return fmt.Errorf("provider policy or project changed")
	}
	ss, err := h.sandboxFor(fe)
	if err != nil {
		return err
	}
	latest, err := ss.AbuseSamples(ctx)
	if err != nil {
		return err
	}
	for _, cur := range latest {
		if cur.ProjectID == x.ProjectID && cur.SandboxID == x.SandboxID {
			if !sandbox.StillInactive(x, cur, p, time.Now().UTC()) {
				return fmt.Errorf("activity/identity changed after sample")
			}
			return ss.CheckIdentity(ctx, x)
		}
	}
	return sandbox.ErrNotFound
}

func sameAbusePolicy(a, b sandbox.AbusePolicy) bool {
	aa, _ := json.Marshal(a.WithDefaults())
	bb, _ := json.Marshal(b.WithDefaults())
	return string(aa) == string(bb) && !a.Disabled
}

type sandboxNotifier interface {
	SendTemplate(context.Context, string, []string, map[string]any) error
}

// SetSandboxSafety wires durable observation/rotation state and the existing mail path.
func (h *Handler) SetSandboxSafety(store *pgdoc.Store, n sandboxNotifier) {
	h.sbSafety = store
	h.sbNotifier = n
}
func (h *Handler) RunSandboxAbuse(ctx context.Context) {
	if h.sbSafety == nil || h.sandboxFor == nil || h.esSvc == nil || h.audit == nil {
		return
	}
	services, err := h.esSvc.List(ctx)
	if err != nil {
		return
	}
	for i := range services {
		es := &services[i]
		if !es.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(es)
		if err != nil {
			continue
		}
		p := ss.Config().Abuse.WithDefaults()
		if p.Disabled {
			continue
		}
		samples, err := ss.AbuseSamples(ctx)
		if err != nil {
			slog.Warn("sandbox abuse: telemetry unavailable; no action", "service", es.ID)
			// Invalidate previous windows immediately, even if a manual retry happens inside maxGap.
			_, _ = h.sbSafety.DeleteMany(ctx, pgdoc.M{"_id": pgdoc.M{"$regex": "^abuse:" + regexp.QuoteMeta(es.ID) + ":"}})
			continue
		}
		for _, x := range samples {
			proj, err := h.svc.repo.FindByID(ctx, x.ProjectID)
			if err != nil || proj == nil {
				continue
			}
			attached := false
			for _, id := range proj.ServiceIDs() {
				if id == es.ID {
					attached = true
				}
			}
			if !attached {
				continue
			}
			flag := false
			key := "abuse:" + es.ID + ":" + x.ProjectID + ":" + x.SandboxID
			err = h.sbSafety.DB().WithTx(ctx, func(tc context.Context) error {
				if err := h.sbSafety.DB().LockXact(tc, key); err != nil {
					return err
				}
				var o sandbox.Observation
				if _, err := h.sbSafety.GetForUpdate(tc, key, &o); err != nil {
					return err
				}
				next, f := o.Next(x, p, time.Now().UTC())
				flag = f
				if err := h.sbSafety.Upsert(tc, key, next); err != nil {
					return err
				}
				if !flag {
					return nil
				}
				ev := audit.SystemEvent()
				ev.EventContext = audit.ContextProject
				ev.OrganizationID = proj.OrganizationID
				ev.ProjectID = proj.ID
				ev.ResourceType = SandboxAbuseResource
				ev.ResourceID = x.SandboxID
				ev.Action = "DETECTED"
				ev.Outcome = audit.OutcomeSuccess
				ev.ResourceMetadata = map[string]any{"serviceId": es.ID, "rule": "sustained-cpu-without-activity", "windowSeconds": p.WindowSeconds, "cpuPercent": p.CPUPercent, "action": p.Action}
				return h.audit.Log(tc, ev)
			})
			if err != nil {
				slog.Error("sandbox abuse: persist failed", "service", es.ID)
				continue
			}
			if !flag {
				continue
			}
			// Event is durable before any notification/action. Email includes only scoped identifiers,
			// never image/env/exec/network addresses or credentials.
			if h.sbNotifier != nil {
				if err := h.sbNotifier.SendTemplate(ctx, "sandbox_abuse_alert", p.NotifyEmails, map[string]any{"projectId": proj.ID, "sandboxId": x.SandboxID, "serviceId": es.ID, "action": p.Action}); err != nil {
					slog.Warn("sandbox abuse: mail delivery failed", "service", es.ID)
				}
			}
			if p.Action == "alert" {
				continue
			}
			// envd SDK exec bypasses the portal and a silent long-running streaming
			// exec has no trustworthy end marker. Alerts are safe; destructive actions
			// must fail closed rather than infer SDK exec inactivity from byte deltas.
			if ss.Config().Envd {
				ev := audit.SystemEvent()
				ev.EventContext = audit.ContextProject
				ev.OrganizationID = proj.OrganizationID
				ev.ProjectID = proj.ID
				ev.ResourceType = SandboxAbuseResource
				ev.ResourceID = x.SandboxID
				ev.Action = "ACTION_" + p.Action
				ev.Outcome = audit.OutcomeFailure
				ev.ResourceMetadata = map[string]any{"serviceId": es.ID, "status": "refused-active-exec-unprovable"}
				_ = h.audit.Log(ctx, ev)
				continue
			}
			// Re-resolve attachment and provider settings after notification: no action with stale ownership/config.
			freshProject, pe := h.svc.repo.FindByID(ctx, proj.ID)
			freshES, se := h.esSvc.Get(ctx, es.ID)
			if pe != nil || se != nil || freshProject == nil || freshES == nil || !freshProject.HasService(es.ID) || !sameAbusePolicy(freshES.SandboxConfig().Abuse.WithDefaults(), p) {
				continue
			}
			acquired, actionErr := h.sbSafety.DB().WithAdvisoryLock(ctx, "sandbox-capture:"+es.ID+":"+proj.ID+":"+x.SandboxID, true, func() error {
				ctx := context.WithValue(ctx, captureLockKey{}, x.SandboxID)
				guard := &abusePauseGuard{Sample: x, Policy: p, ConnectionHash: fmt.Sprintf("%x", sha256.Sum256([]byte(ss.Config().Kubeconfig)))}
				ctx = context.WithValue(ctx, abuseGuardKey{}, guard)
				err := h.authorizeAbuseAction(ctx, es.ID, guard)
				if err == nil {
					c := &e2bCaller{proj: proj, es: es, ss: ss}
					// A pause/resume/capture in progress is never cancelled by this heuristic.
					if h.loadPaused(ctx, proj.ID, x.SandboxID) != nil {
						err = errSandboxBusy
					} else if p.Action == "pause" {
						err = h.pauseSandbox(ctx, c, x.SandboxID)
					} else {
						// Never kill during snapshot assembly, including a named snapshot (no paused record).
						builds, be := ss.ListBuilds(ctx, proj.ID)
						if be != nil {
							return be
						}
						for _, b := range builds {
							if b["status"] != sandbox.StatusDeleted {
								return errSandboxBusy
							}
						}
						err = h.underPauseLock(ctx, c, x.SandboxID, func() error {
							if err := ss.CheckIdentity(ctx, x); err != nil {
								return err
							}
							if cr := h.e2bRow(ctx, c, x.SandboxID); cr != nil {
								return h.sandboxDelete(ctx, es, proj, cr)
							}
							return ss.DeleteSandbox(ctx, proj.ID, x.SandboxID)
						})
					}
				}
				return err
			})
			err = actionErr
			if !acquired && err == nil {
				err = errSandboxBusy
			}
			ev := audit.SystemEvent()
			ev.EventContext = audit.ContextProject
			ev.OrganizationID = proj.OrganizationID
			ev.ProjectID = proj.ID
			ev.ResourceType = SandboxAbuseResource
			ev.ResourceID = x.SandboxID
			ev.Action = "ACTION_" + p.Action
			ev.Outcome = audit.OutcomeSuccess
			ev.ResourceMetadata = map[string]any{"serviceId": es.ID, "status": "requested"}
			if err != nil {
				ev.Outcome = audit.OutcomeFailure
				ev.ResourceMetadata["status"] = "refused-or-failed"
			}
			if e := h.audit.Log(ctx, ev); e != nil {
				slog.Error("sandbox abuse: action audit failed")
			}
		}
	}
}

func rotationScope(p sandbox.RotationPolicy) string {
	b, _ := json.Marshal(p)
	return fmt.Sprintf("%x:%x", sha256.Sum256(b), sha256.Sum256([]byte(p.Kubeconfig)))
}

type rotationState struct {
	Started   time.Time `json:"started"`
	Completed time.Time `json:"completed"`
	Nodes     []string  `json:"nodes"`
	Active    bool      `json:"active"`
	Scope     string    `json:"scope"`
}

// withSandboxStart excludes rotation using a provider-wide database lock across
// CR creation/resume. It also fences starts while a durable rollout is active.
func (h *Handler) withSandboxStart(ctx context.Context, service string, fn func(context.Context) error) error {
	if h.sbSafety == nil {
		return fn(ctx)
	}
	return h.sbSafety.DB().WithTx(ctx, func(tc context.Context) error {
		if err := h.sbSafety.DB().LockXact(tc, "sandbox-start:"+service); err != nil {
			return err
		}
		var state rotationState
		if _, err := h.sbSafety.Get(tc, "rotation:"+service, &state); err != nil {
			return err
		}
		if state.Active {
			return fmt.Errorf("sandbox worker rotation in progress; retry later")
		}
		return fn(tc)
	})
}

// RunSandboxRotation is wait-only and default-off. A failed patch/unknown
// convergence keeps the durable start fence closed for operator intervention.
func (h *Handler) RunSandboxRotation(ctx context.Context) {
	if h.sbSafety == nil || h.sandboxFor == nil {
		return
	}
	services, err := h.esSvc.List(ctx)
	if err != nil {
		return
	}
	for i := range services {
		es := &services[i]
		if !es.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(es)
		if err != nil {
			continue
		}
		p := ss.Config().Rotation
		if !p.Enabled {
			continue
		}
		var cordoned []string
		err = h.sbSafety.DB().WithTx(ctx, func(tc context.Context) error {
			if err := h.sbSafety.DB().LockXact(tc, "sandbox-start:"+es.ID); err != nil {
				return err
			}
			key := "rotation:" + es.ID
			var state rotationState
			if _, err := h.sbSafety.GetForUpdate(tc, key, &state); err != nil {
				return err
			}
			now := time.Now().UTC()
			if state.Active {
				scope := rotationScope(p)
				if state.Scope != scope {
					return fmt.Errorf("active rotation scope changed: restore settings before continuing")
				}
				done, err := ss.RotationConverged(ctx, state.Nodes)
				if err != nil {
					return err
				}
				if !done {
					return nil
				}
				state.Active = false
				state.Completed = now
				return h.sbSafety.Upsert(tc, key, state)
			}
			if state.Completed.IsZero() {
				state.Completed = now
				return h.sbSafety.Upsert(tc, key, state)
			}
			if now.Sub(state.Completed) < time.Duration(p.IntervalHours)*time.Hour {
				return nil
			}
			plan, err := ss.RotationInspect(ctx)
			if err != nil {
				return err
			}
			if !plan.Ready {
				return nil
			}
			if err := ss.CordonRotationNodes(ctx, plan.Nodes, true); err != nil {
				return err
			}
			cordoned = plan.Nodes
			again, err := ss.RotationRecheck(ctx)
			if err != nil || !again.Ready {
				_ = ss.CordonRotationNodes(context.WithoutCancel(ctx), plan.Nodes, false)
				if err != nil {
					return err
				}
				return nil
			}
			state.Scope = rotationScope(p)
			state.Active = true
			state.Started = now
			state.Nodes = plan.Nodes
			if err := h.sbSafety.Upsert(tc, key, state); err != nil {
				return err
			}
			ev := audit.SystemEvent()
			ev.ResourceType = "SANDBOX_ROTATION"
			ev.ResourceID = es.ID
			ev.Action = "ROLLOUT_REQUESTED"
			ev.Outcome = audit.OutcomeSuccess
			return h.audit.Log(tc, ev)
		})
		if err != nil {
			if len(cordoned) > 0 {
				_ = ss.CordonRotationNodes(context.WithoutCancel(ctx), cordoned, false)
			}
			slog.Warn("sandbox rotation preflight failed", "service", es.ID)
			continue
		}
		// Fence must commit BEFORE the first CAPI patch; crash here is safe and resumed
		// on the next tick. rolloutAfter is idempotent for this fixed timestamp.
		var state rotationState
		if _, err := h.sbSafety.Get(ctx, "rotation:"+es.ID, &state); err != nil || !state.Active {
			continue
		}
		scope := rotationScope(p)
		if state.Scope != scope {
			continue
		}
		if err := ss.StartRotation(ctx, state.Started); err != nil {
			slog.Warn("sandbox rotation patch failed; start fence retained", "service", es.ID)
		}
	}
}
