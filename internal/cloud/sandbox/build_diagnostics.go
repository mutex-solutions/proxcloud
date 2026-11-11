package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// BuildStage retains failure evidence without exposing pod/node names or API errors.
type BuildStage struct {
	Phase    string `json:"phase"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   int    `json:"signal,omitempty"`
	CPU      string `json:"cpu,omitempty"`
	Memory   string `json:"memory,omitempty"`
	Disk     string `json:"disk,omitempty"`
}

type BuildLogStage struct {
	Phase       string
	Lines       []string
	Unavailable bool
	Truncated   bool
	Tail        []string // terminal error context when the bounded prefix was exceeded
}

type buildPrefixReader interface {
	PodLogPrefix(context.Context, string, string, string, int) (string, bool, error)
}

const BuildStageLogBytes = 128 << 10

// CaptureBuildLogs only reads containers proven to belong to this project and to have
// started. A missing prefix reader is an explicit failure, never a sliding-tail fallback.
func (s *Service) CaptureBuildLogs(ctx context.Context, projectID, buildID string) ([]BuildLogStage, error) {
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(buildID))
	if err != nil {
		return nil, err
	}
	if pod == nil || !s.ownsBuild(pod, projectID, buildID) {
		return nil, ErrNotFound
	}
	reader, supported := s.api.(buildPrefixReader)
	var out []BuildLogStage
	terminal := buildStateOf(pod).Status
	for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, cs := range containerStatuses(pod, key) {
			phase := str(cs["name"])
			if phase != "fetch" && phase != "base" && phase != "kaniko" && phase != "prepare-output" && phase != "push" {
				continue
			}
			if dig(cs, "state", "running") == nil && dig(cs, "state", "terminated") == nil {
				continue
			}
			stage := BuildLogStage{Phase: phase}
			if !supported {
				stage.Unavailable = true
				out = append(out, stage)
				continue
			}
			text, cut, err := reader.PodLogPrefix(ctx, BuildNamespace, buildName(buildID), phase, BuildStageLogBytes)
			stage.Unavailable, stage.Truncated = err != nil, cut
			if err == nil {
				// Never commit a growing partial line. At terminal/cap it cannot change.
				if !strings.HasSuffix(text, "\n") && terminal != BuildReady && terminal != BuildError && !cut {
					if i := strings.LastIndex(text, "\n"); i >= 0 {
						text = text[:i+1]
					} else {
						text = ""
					}
				}
				if text != "" {
					stage.Lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
				}
				for i := range stage.Lines {
					stage.Lines[i] = RedactBuildText(stage.Lines[i])
				}
				if dig(cs, "state", "terminated") != nil {
					// Keep the final stderr/build-step context as a separate terminal
					// segment. Never splice a sliding tail into the prefix cursor.
					tail, err := s.api.PodLogs(ctx, BuildNamespace, buildName(buildID), phase, 100)
					if err != nil {
						stage.Unavailable = true
					} else {
						if len(tail) > 16<<10 {
							tail = tail[len(tail)-(16<<10):]
						}
						if tail != "" {
							stage.Tail = strings.Split(strings.TrimSuffix(tail, "\n"), "\n")
						}
						for i := range stage.Tail {
							stage.Tail[i] = RedactBuildText(stage.Tail[i])
						}
					}
				}
			}
			out = append(out, stage)
		}
	}
	return out, nil
}

// CleanupBuild checks tenant ownership again before archive-authorized deletion.
func (s *Service) CleanupBuild(ctx context.Context, projectID, buildID string) error {
	api, ok := s.api.(coreUIDDeleter)
	if !ok {
		return fmt.Errorf("sandbox: cleanup requires conditional UID deletion")
	}
	pod, err := s.api.GetCore(ctx, "pods", BuildNamespace, buildName(buildID))
	if err != nil {
		return err
	}
	if pod != nil {
		if !s.ownsBuild(pod, projectID, buildID) {
			return nil
		}
		phase := digStr(pod, "status", "phase")
		if phase != "Succeeded" && phase != "Failed" {
			return nil
		}
		uid := digStr(pod, "metadata", "uid")
		if uid == "" {
			return fmt.Errorf("sandbox: cleanup pod UID missing")
		}
		if err = api.DeleteCoreUID(ctx, "pods", BuildNamespace, buildName(buildID), uid); err != nil {
			return err
		}
	}
	cm, err := s.api.GetCore(ctx, "configmaps", BuildNamespace, buildName(buildID))
	if err != nil || cm == nil {
		return err
	}
	if !s.ownsBuild(cm, projectID, buildID) {
		return nil
	}
	if pod != nil && (digStr(cm, "metadata", "annotations", AnnotationBuildOperation) != digStr(pod, "metadata", "annotations", AnnotationBuildOperation) || digStr(cm, "metadata", "annotations", AnnotationBuildNonce) != digStr(pod, "metadata", "annotations", AnnotationBuildNonce)) {
		return fmt.Errorf("sandbox: cleanup config generation changed")
	}
	uid := digStr(cm, "metadata", "uid")
	if uid == "" {
		return fmt.Errorf("sandbox: cleanup config UID missing")
	}
	return api.DeleteCoreUID(ctx, "configmaps", BuildNamespace, buildName(buildID), uid)
}

var buildRedactions = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer|basic)\s+)\S+`),
	regexp.MustCompile(`(?i)((?:[?&]|\b)(?:token|access_token|password|secret|signature|x-amz-signature)=)[^&\s"']+`),
	regexp.MustCompile(`(?i)("(?:auth|password|identitytoken|access_token|secretKey)"\s*:\s*")[^"]*`),
	regexp.MustCompile(`(https?://)[^/@\s]+:[^/@\s]+@`),
}

// RedactBuildText removes common credential forms. Arbitrary secrets printed by user
// RUN commands cannot be identified reliably; callers must not print secrets in builds.
func RedactBuildText(text string) string {
	for _, re := range buildRedactions {
		text = re.ReplaceAllString(text, "${1}[REDACTED]")
	}
	return strings.ToValidUTF8(text, "�")
}

func buildStages(pod map[string]any) []BuildStage {
	var out []BuildStage
	for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, cs := range containerStatuses(pod, key) {
			st := BuildStage{Phase: str(cs["name"])}
			if t, ok := dig(cs, "state", "terminated").(map[string]any); ok {
				n := intOf(t["exitCode"])
				st.ExitCode = &n
				st.State, st.Reason, st.Signal = "terminated", str(t["reason"]), intOf(t["signal"])
				// push's successful termination message is a digest, not failure output.
				if n != 0 {
					message := str(t["message"])
					if len(message) > 4096 {
						message = message[:4096] + " [truncated]"
					}
					st.Message = RedactBuildText(message)
				}
			} else if w, ok := dig(cs, "state", "waiting").(map[string]any); ok {
				st.State, st.Reason = "waiting", str(w["reason"])
				// kubelet messages may include node names, pod paths and credentials.
				st.Message = "waiting for " + stepLabel(st.Phase)
			} else if dig(cs, "state", "running") != nil {
				st.State = "running"
			}
			for _, ck := range []string{"initContainers", "containers"} {
				containers, _ := dig(pod, "spec", ck).([]any)
				for _, c := range containers {
					m, _ := c.(map[string]any)
					if str(m["name"]) == st.Phase {
						st.CPU, st.Memory, st.Disk = digStr(m, "resources", "limits", "cpu"), digStr(m, "resources", "limits", "memory"), digStr(m, "resources", "limits", "ephemeral-storage")
					}
				}
			}
			out = append(out, st)
		}
	}
	return out
}

// buildPodMessage extracts only useful resource categories from kubelet text; raw
// messages often reveal node names, IPs and host paths and are not tenant-safe.
func buildPodMessage(pod map[string]any) string {
	message := strings.ToLower(digStr(pod, "status", "message"))
	conditions, _ := dig(pod, "status", "conditions").([]any)
	for _, c := range conditions {
		m, _ := c.(map[string]any)
		if str(m["type"]) == "PodScheduled" && str(m["status"]) == "False" {
			message += " " + strings.ToLower(str(m["message"]))
		}
	}
	var resources []string
	for _, resource := range []string{"memory", "ephemeral-storage", "cpu"} {
		if strings.Contains(message, resource) {
			resources = append(resources, resource)
		}
	}
	if len(resources) > 0 {
		return "resource pressure or insufficient capacity: " + strings.Join(resources, ", ")
	}
	if strings.Contains(message, "taint") {
		return "no eligible build node (scheduling constraints)"
	}
	if digStr(pod, "status", "reason") == "Evicted" {
		return "node resource pressure; inspect build resource limits"
	}
	return ""
}

func stageFailure(st BuildStage) string {
	msg := fmt.Sprintf("%s failed (exit %d", stepLabel(st.Phase), *st.ExitCode)
	if st.Reason != "" {
		msg += ", reason " + st.Reason
	}
	if st.Signal != 0 {
		msg += fmt.Sprintf(", signal %d", st.Signal)
	}
	msg += ")"
	if st.Message != "" {
		msg += ": " + st.Message
	}
	return msg
}
