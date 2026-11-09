package sandbox

// Startup is a bounded, observable control-plane wait, not the sandbox's runtime TTL.
// The same policy applies to create, snapshot-create, resume, fork and proxy auto-resume.
import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	MaxStartupSeconds        = 1800
	AnnotationStartupPending = "proxcloud.io/startup-pending"
	AnnotationRuntimeSeconds = "proxcloud.io/runtime-seconds"
)

type StartupConfig struct {
	TotalSeconds      int `json:"totalSeconds"`
	SchedulingSeconds int `json:"schedulingSeconds"`
	PullSeconds       int `json:"pullSeconds"`
	RuntimeSeconds    int `json:"runtimeSeconds"`
}

func (c StartupConfig) withDefaults() StartupConfig {
	for _, f := range []struct {
		p   *int
		def int
	}{{&c.TotalSeconds, 900}, {&c.SchedulingSeconds, 120}, {&c.PullSeconds, 720}, {&c.RuntimeSeconds, 600}} {
		if *f.p == 0 {
			*f.p = f.def
		}
		*f.p = min(max(*f.p, 1), MaxStartupSeconds)
	}
	c.SchedulingSeconds = min(c.SchedulingSeconds, c.TotalSeconds)
	c.PullSeconds = min(c.PullSeconds, c.TotalSeconds)
	c.RuntimeSeconds = min(c.RuntimeSeconds, c.TotalSeconds)
	return c
}

func (c StartupConfig) Duration() time.Duration {
	return time.Duration(c.withDefaults().TotalSeconds) * time.Second
}

// StartupProgress contains only stable public reasons, never Kubernetes messages (which can
// contain private registry credentials, node names, IPs or a customer's command/environment).
type StartupProgress struct {
	Phase    string `json:"phase"`
	Reason   string `json:"reason"`
	Terminal bool   `json:"terminal,omitempty"`
}

type StartupError struct {
	Progress StartupProgress
	Cause    error
}

func (e *StartupError) Error() string {
	return fmt.Sprintf("sandbox startup %s: %s", e.Progress.Phase, e.Progress.Reason)
}
func (e *StartupError) Unwrap() error { return e.Cause }

func startupProgress(pod map[string]any) StartupProgress {
	p := StartupProgress{Phase: "scheduling", Reason: "waiting for a schedulable node; check size, GPU and pool capacity"}
	if pod == nil {
		return p
	}
	if digStr(pod, "status", "phase") == "Failed" {
		return StartupProgress{"runtime", "pod failed or was evicted; inspect sandbox logs or contact support", true}
	}
	if digStr(pod, "spec", "nodeName") == "" {
		return p
	}
	p = StartupProgress{Phase: "image-pull", Reason: "pulling image; prepare the template before create or increase the configured pull budget"}
	for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, cs := range containerStatuses(pod, key) {
			reason := digStr(cs, "state", "waiting", "reason")
			switch reason {
			case "ErrImagePull", "ImagePullBackOff":
				p.Reason = "image pull failed or is backing off; verify image digest, registry permissions and registry availability"
			case "InvalidImageName", "ErrImageNeverPull":
				return StartupProgress{"image-pull", "image reference is invalid or unavailable on this node", true}
			case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return StartupProgress{"runtime", "container setup failed; check template configuration or contact support", true}
			}
			if dig(cs, "state", "terminated") != nil {
				// A successful init container is expected; only a failed init is terminal.
				if key == "containerStatuses" || intOf(dig(cs, "state", "terminated", "exitCode")) != 0 {
					return StartupProgress{"runtime", "container exited before readiness; inspect sandbox logs and start/ready commands", true}
				}
			}
			if dig(cs, "state", "running") != nil {
				p = StartupProgress{"runtime", "waiting for envd and the template ready command; inspect sandbox logs and start/ready commands", false}
			}
		}
	}
	return p
}

// WaitReady never returns a merely scheduled/pulled sandbox. It bounds both the entire operation
// and time spent in each phase; phase transitions cannot extend the total deadline.
func (s *Service) WaitReady(ctx context.Context, projectID, id string) (map[string]any, error) {
	cfg := s.cfg.Startup.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, cfg.Duration())
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	phase := ""
	phaseAt := time.Now()
	spent := map[string]time.Duration{}
	progress := StartupProgress{Phase: "scheduling", Reason: "waiting for sandbox control plane"}
	for {
		if ctx.Err() != nil {
			return nil, &StartupError{progress, ctx.Err()}
		}
		live, err := s.GetSandbox(ctx, projectID, id)
		// A transport can return an otherwise successful GET after its deadline.
		// Recheck cancellation and the original persisted lifetime before exposing readiness.
		if ctx.Err() != nil {
			return nil, &StartupError{progress, ctx.Err()}
		}
		if err == nil {
			end, parseErr := time.Parse(time.RFC3339, str(live["shutdownTime"]))
			if parseErr != nil || !s.now().Before(end) {
				return nil, &StartupError{StartupProgress{"runtime", "sandbox lifetime expired during startup", true}, context.DeadlineExceeded}
			}
			if live["status"] == StatusRunning && live["ready"] == true {
				return live, nil
			}
			if p, ok := live["startup"].(StartupProgress); ok {
				progress = p
			}
			if progress.Phase != phase {
				spent[phase] += time.Since(phaseAt)
				phase, phaseAt = progress.Phase, time.Now()
			}
			seconds := cfg.SchedulingSeconds
			switch phase {
			case "image-pull":
				seconds = cfg.PullSeconds
			case "runtime":
				seconds = cfg.RuntimeSeconds
			}
			if progress.Terminal || spent[phase]+time.Since(phaseAt) >= time.Duration(seconds)*time.Second {
				return nil, &StartupError{progress, context.DeadlineExceeded}
			}
		} else if errors.Is(err, ErrNotFound) {
			return nil, &StartupError{StartupProgress{"control-plane", "sandbox disappeared during startup", true}, err}
		} else {
			progress = StartupProgress{Phase: "control-plane", Reason: "sandbox control plane unavailable; retry preparation or contact support"}
		}
		select {
		case <-ctx.Done():
			return nil, &StartupError{progress, ctx.Err()}
		case <-tick.C:
		}
	}
}

// armStartup gives pending work the full startup budget plus its runtime TTL. On the first
// observed Ready, pinRunningSince atomically replaces this provisional end with Ready + TTL.
// If the API crashes, the controller still deletes the CR at this finite provisional deadline.
func armStartup(cr map[string]any, cfg Config, runtimeSeconds int, now time.Time) {
	meta, _ := cr["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	delete(ann, AnnotationRunningSince)
	ann[AnnotationStartupPending] = "true"
	ann[AnnotationRuntimeSeconds] = strconv.Itoa(runtimeSeconds)
	end := now.Add(cfg.Startup.Duration() + time.Duration(runtimeSeconds)*time.Second).UTC().Truncate(time.Second)
	if ann[AnnotationAutoPause] == "true" {
		ann[AnnotationEndAt] = end.Format(time.RFC3339)
		end = end.Add(AutoPauseGrace)
	}
	cr["spec"].(map[string]any)["shutdownTime"] = end.Format(time.RFC3339)
}

// ArmSavedStartup sets the same pending/TTL policy on resume/fork, including legacy saved CRs.
func ArmSavedStartup(saved map[string]any, cfg Config, runtimeSeconds int, now time.Time) {
	armStartup(saved, cfg, runtimeSeconds, now)
}
