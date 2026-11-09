package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// Diagnostics and Logs are updated together under the template's row lock. Logs is
// an immutable bounded prefix: logsOffset remains a plain index for stock E2B SDKs.
// Once full, append one reserved marker and stop; never slide/renumber old lines.
type buildDiagnostics struct {
	Revision          int                       `json:"revision"`
	Stages            []sandbox.BuildStage      `json:"stages,omitempty"`
	PodReason         string                    `json:"podReason,omitempty"`
	PodMessage        string                    `json:"podMessage,omitempty"`
	Cursors           map[string]buildLogCursor `json:"cursors,omitempty"`
	Warnings          []string                  `json:"warnings,omitempty"`
	LogsUnavailable   bool                      `json:"logsUnavailable,omitempty"`
	Truncated         bool                      `json:"truncated,omitempty"`
	ArchivedAt        *time.Time                `json:"archivedAt,omitempty"`
	CleanedAt         *time.Time                `json:"cleanedAt,omitempty"`
	LastCaptureAt     *time.Time                `json:"lastCaptureAt,omitempty"`
	TerminalCaptureAt *time.Time                `json:"terminalCaptureAt,omitempty"`
	TerminalAttempts  int                       `json:"terminalAttempts,omitempty"`
	TerminalSignature string                    `json:"terminalSignature,omitempty"`
	StableCaptures    int                       `json:"stableCaptures,omitempty"`
	LossReason        string                    `json:"lossReason,omitempty"`
	TerminalTail      []string                  `json:"terminalTail,omitempty"`
}

type buildLogCursor struct {
	Lines     int    `json:"lines"`
	Hash      string `json:"hash"`
	Truncated bool   `json:"truncated,omitempty"`
}

func buildTerminal(b *templateBuild) bool {
	return b.Status == sandbox.BuildReady || b.Status == sandbox.BuildError
}

func buildNeedsSync(b *templateBuild) bool {
	if b.Import != nil {
		return b.Status == sandbox.BuildWaiting || b.Status == sandbox.BuildBuilding
	}
	return b.CancelRequested || b.Status == sandbox.BuildWaiting || b.Status == sandbox.BuildBuilding || buildTerminal(b) && (b.Diagnostics == nil || b.Diagnostics.CleanedAt == nil)
}

func appendBuildLog(b *templateBuild, line string) {
	d := b.Diagnostics
	if d.Truncated {
		return
	}
	n := 0
	for _, l := range b.Logs {
		n += len(l)
	}
	line = sandbox.RedactBuildText(line)
	if len(b.Logs) >= maxStoredLogLines-401 || n+len(line) > maxStoredLogBytes-(64<<10)-256 {
		b.Logs = append(b.Logs, "[diagnostics] log archive limit reached; further output was not retained")
		d.Truncated = true
		return
	}
	b.Logs = append(b.Logs, line)
}

func logPrefixHash(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		_, _ = h.Write([]byte(l))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// foldBuildCapture operates only on the locked current record, not a request's stale
// copy. Retrying overlapping snapshots therefore cannot duplicate output or lose it.
func foldBuildCapture(b *templateBuild, st *sandbox.BuildState, stages []sandbox.BuildLogStage, captureErr bool, now time.Time) {
	if b.Diagnostics == nil {
		b.Diagnostics = &buildDiagnostics{}
	}
	d := b.Diagnostics
	if d.ArchivedAt != nil {
		return
	}
	if d.Cursors == nil {
		d.Cursors = map[string]buildLogCursor{}
	}
	d.Revision++
	d.LastCaptureAt = &now
	d.Warnings, d.LogsUnavailable = nil, captureErr
	if captureErr {
		d.Warnings = append(d.Warnings, "build output could not be read; capture will be retried")
	}
	if st != nil {
		d.Stages, d.PodReason, d.PodMessage = st.Stages, st.PodReason, st.PodMessage
	}
	for _, stage := range stages {
		if stage.Unavailable {
			d.LogsUnavailable = true
			d.Warnings = append(d.Warnings, fmt.Sprintf("%s logs unavailable; capture will be retried", stage.Phase))
			continue
		}
		cur := d.Cursors[stage.Phase]
		if cur.Lines > len(stage.Lines) || (cur.Lines > 0 && cur.Hash != logPrefixHash(stage.Lines[:cur.Lines])) {
			d.LogsUnavailable = true
			d.LossReason = "a container log prefix changed or rotated before it could be fully archived"
			d.Warnings = append(d.Warnings, d.LossReason)
			continue
		}
		for _, line := range stage.Lines[cur.Lines:] {
			appendBuildLog(b, "["+stage.Phase+"] "+line)
		}
		d.Cursors[stage.Phase] = buildLogCursor{Lines: len(stage.Lines), Hash: logPrefixHash(stage.Lines), Truncated: cur.Truncated || stage.Truncated}
		if stage.Truncated && !cur.Truncated {
			appendBuildLog(b, "["+stage.Phase+"] log source byte limit reached; further output was not retained")
			// One marker only, even while other phases continue to be captured.
			if !strings.Contains(d.LossReason, stage.Phase+" log byte limit") {
				d.LossReason = strings.TrimSpace(d.LossReason + " " + stage.Phase + " log byte limit reached;")
			}
		}
	}
	// Refresh (rather than append) the provisional terminal tail until the
	// archive is stable; it is not part of the SDK cursor before finalization.
	d.TerminalTail = nil
	for _, stage := range stages {
		if stage.Unavailable || !(stage.Truncated || d.Truncated) {
			continue
		}
		for _, line := range stage.Tail {
			line = "[" + stage.Phase + " terminal tail] " + sandbox.RedactBuildText(line)
			size := 0
			for _, l := range d.TerminalTail {
				size += len(l)
			}
			if len(d.TerminalTail) >= 400 || size+len(line) > 64<<10 {
				break
			}
			d.TerminalTail = append(d.TerminalTail, line)
		}
	}
	terminal := buildTerminal(b) || st != nil && (st.Status == sandbox.BuildReady || st.Status == sandbox.BuildError)
	if !terminal {
		return
	}
	if d.TerminalCaptureAt == nil {
		d.TerminalCaptureAt = &now
	}
	d.TerminalAttempts++
	// A newly available/changed terminal prefix needs a second matching capture.
	// This prevents kubelet lag or a recovered read from immediately freezing it.
	signature := logPrefixHash(d.TerminalTail)
	for _, phase := range []string{"fetch", "base", "kaniko", "push"} {
		signature += phase + ":" + d.Cursors[phase].Hash + ";"
	}
	if !d.LogsUnavailable && signature == d.TerminalSignature {
		d.StableCaptures++
	} else {
		d.StableCaptures = 0
	}
	d.TerminalSignature = signature
	complete := !d.LogsUnavailable && d.StableCaptures >= 1 && (len(b.Logs) > 0 || d.StableCaptures >= 2)
	if !complete && now.Sub(*d.TerminalCaptureAt) < buildCaptureGrace {
		return
	}
	if !complete {
		d.LogsUnavailable = true
		d.LossReason = "build output capture failed after the terminal retry window; some output is permanently unavailable"
		appendBuildLog(b, "[diagnostics] "+d.LossReason)
	} else if len(b.Logs) == 0 {
		appendBuildLog(b, "[diagnostics] containers reported no build output after repeated terminal captures")
	}
	// Append the terminal tail once, after the stable prefix (including its gap
	// marker). Reserved space keeps this available to stock E2B logsOffset clients.
	b.Logs = append(b.Logs, d.TerminalTail...)
	d.ArchivedAt = &now
}

// saveBuildCapture deliberately does not invoke finishBuild: late log updates must
// never repeat its one-shot snapshot billing/tag/cleanup side effects.
func (h *Handler) saveBuildCapture(ctx context.Context, projectID, templateID, buildID string, st *sandbox.BuildState, stages []sandbox.BuildLogStage, captureErr bool) (*sandboxTemplateDoc, error) {
	var out *sandboxTemplateDoc
	err := h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var t sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, templateID, &t)
		if err != nil || !found || t.ProjectID != projectID {
			return err
		}
		out = &t
		b := t.build(buildID)
		if b == nil {
			return nil
		}
		now := time.Now().UTC()
		if d := b.Diagnostics; d != nil && (d.ArchivedAt != nil || d.LastCaptureAt != nil && now.Sub(*d.LastCaptureAt) < buildCaptureRetry) {
			return nil
		}
		if buildTerminal(b) && b.FinishedAt == nil {
			b.FinishedAt = &now
		} // legacy terminal records
		foldBuildCapture(b, st, stages, captureErr, now)
		return h.saveTemplate(tc, &t)
	})
	return out, err
}

func (h *Handler) cleanupBuildArchive(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild, now time.Time) {
	if !buildCanCleanup(b, now) {
		return
	}
	if err := c.ss.CleanupBuild(ctx, t.ProjectID, b.BuildID); err != nil {
		return
	}
	var saved *sandboxTemplateDoc
	if err := h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil || !found || cur.ProjectID != t.ProjectID {
			return err
		}
		cb := cur.build(b.BuildID)
		if cb == nil || !buildCanCleanup(cb, now) {
			return nil
		}
		cb.Diagnostics.CleanedAt = &now
		cb.Diagnostics.Revision++
		saved = &cur
		return h.saveTemplate(tc, &cur)
	}); err == nil {
		adoptBuild(t, b, saved)
	}
}

func buildCanCleanup(b *templateBuild, now time.Time) bool {
	return buildTerminal(b) && b.FinishedAt != nil && now.Sub(*b.FinishedAt) >= buildPodGrace && b.Diagnostics != nil && b.Diagnostics.ArchivedAt != nil && b.Diagnostics.CleanedAt == nil
}

func buildLogPage(b *templateBuild, offset int) []string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(b.Logs) {
		offset = len(b.Logs)
	}
	page := b.Logs[offset:]
	if page == nil {
		return []string{}
	}
	return page
}
