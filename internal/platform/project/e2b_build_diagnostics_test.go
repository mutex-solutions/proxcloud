package project

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func captureLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d", i)
	}
	return out
}

func TestBuildLogCursorBeyond2000(t *testing.T) {
	now := time.Now().UTC()
	b := &templateBuild{Status: sandbox.BuildBuilding}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: captureLines(2100)}}, false, now)
	first := append([]string(nil), buildLogPage(b, 0)...)
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: captureLines(3500)}}, false, now.Add(time.Minute))
	page := buildLogPage(b, len(first))
	if len(page) != 1400 || page[0] != "[kaniko] line 2100" || !reflect.DeepEqual(first, b.Logs[:len(first)]) {
		t.Fatalf("cursor stalled or shifted: first=%d page=%d", len(first), len(page))
	}
	// Repeated polling or API restart retains exactly the same cursor meaning.
	data, _ := json.Marshal(b)
	var restart templateBuild
	_ = json.Unmarshal(data, &restart)
	foldBuildCapture(&restart, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: captureLines(3500)}}, false, now.Add(2*time.Minute))
	if len(restart.Logs) != 3500 || len(buildLogPage(&restart, 3500)) != 0 {
		t.Fatal("duplicate output after restart")
	}
	if len(buildLogPage(&restart, -1)) != 3500 || len(buildLogPage(&restart, 999999)) != 0 {
		t.Fatal("bad cursor bounds")
	}
}

func TestBuildLogArchiveBoundedAppendOnly(t *testing.T) {
	now := time.Now().UTC()
	b := &templateBuild{Status: sandbox.BuildBuilding}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: captureLines(100)}}, false, now)
	prefix := append([]string(nil), b.Logs...)
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: captureLines(10000)}}, false, now.Add(time.Minute))
	if len(b.Logs) != maxStoredLogLines-400 || !b.Diagnostics.Truncated || !reflect.DeepEqual(prefix, b.Logs[:len(prefix)]) || !strings.Contains(b.Logs[len(b.Logs)-1], "limit reached") {
		t.Fatalf("bad truncation: %d %+v", len(b.Logs), b.Diagnostics)
	}
	before := len(b.Logs)
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "push", Lines: []string{"more"}}}, false, now.Add(2*time.Minute))
	if len(b.Logs) != before {
		t.Fatal("archive grew after cap")
	}
	b = &templateBuild{Status: sandbox.BuildBuilding}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: []string{strings.Repeat("x", maxStoredLogBytes+1)}}}, false, now)
	size := 0
	for _, line := range b.Logs {
		size += len(line)
	}
	if size > maxStoredLogBytes || !b.Diagnostics.Truncated {
		t.Fatal("unbounded single log line")
	}
}

func TestBuildTerminalRecaptureAndCleanup(t *testing.T) {
	now := time.Now().UTC()
	b := &templateBuild{Status: sandbox.BuildError, FinishedAt: &now, Reason: "RUN exit 2"}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Unavailable: true}}, false, now)
	if !b.Diagnostics.LogsUnavailable || b.Diagnostics.ArchivedAt != nil || buildCanCleanup(b, now.Add(time.Hour)) {
		t.Fatal("transient failure must not permit cleanup")
	}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: []string{"RUN exit 2", "stderr"}}}, false, now.Add(time.Minute))
	if b.Diagnostics.ArchivedAt != nil {
		t.Fatal("recovered terminal read must be confirmed")
	}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "kaniko", Lines: []string{"RUN exit 2", "stderr"}}}, false, now.Add(2*time.Minute))
	if b.Diagnostics.LogsUnavailable || b.Diagnostics.ArchivedAt == nil || b.Logs[1] != "[kaniko] stderr" || buildCanCleanup(b, now.Add(time.Minute)) || !buildCanCleanup(b, now.Add(3*time.Minute)) {
		t.Fatalf("recovery: %+v", b)
	}
	// Terminal empty output is not frozen on first capture.
	empty := &templateBuild{Status: sandbox.BuildError, FinishedAt: &now}
	foldBuildCapture(empty, nil, nil, false, now)
	foldBuildCapture(empty, nil, nil, false, now.Add(time.Minute))
	if empty.Diagnostics.ArchivedAt != nil {
		t.Fatal("empty output finalized too soon")
	}
	foldBuildCapture(empty, nil, []sandbox.BuildLogStage{{Phase: "base", Lines: []string{"401 Unauthorized"}}}, false, now.Add(2*time.Minute))
	foldBuildCapture(empty, nil, []sandbox.BuildLogStage{{Phase: "base", Lines: []string{"401 Unauthorized"}}}, false, now.Add(3*time.Minute))
	if empty.Diagnostics.ArchivedAt == nil || len(empty.Logs) != 1 {
		t.Fatal("late terminal output lost")
	}
	lost := &templateBuild{Status: sandbox.BuildError, FinishedAt: &now}
	foldBuildCapture(lost, nil, nil, true, now)
	foldBuildCapture(lost, nil, nil, true, now.Add(buildCaptureGrace))
	if lost.Diagnostics.ArchivedAt == nil || lost.Diagnostics.LossReason == "" || !lost.Diagnostics.LogsUnavailable || !buildCanCleanup(lost, now.Add(time.Hour)) {
		t.Fatal("permanent capture failure lacks durable loss record")
	}
}

func TestStageSourceCapAndRotation(t *testing.T) {
	now := time.Now().UTC()
	b := &templateBuild{Status: sandbox.BuildBuilding}
	s := []sandbox.BuildLogStage{{Phase: "base", Lines: []string{"one"}, Truncated: true}}
	foldBuildCapture(b, nil, s, false, now)
	foldBuildCapture(b, nil, s, false, now.Add(time.Minute))
	if len(b.Logs) != 2 {
		t.Fatal("source truncation marker duplicated")
	}
	foldBuildCapture(b, nil, []sandbox.BuildLogStage{{Phase: "base", Lines: []string{"rotated"}}}, false, now.Add(2*time.Minute))
	if !b.Diagnostics.LogsUnavailable || b.Diagnostics.LossReason == "" || b.Logs[0] != "[base] one" {
		t.Fatal("rotation silently corrupted cursor")
	}
}

func TestBuildLogEntryStageAndTimestamp(t *testing.T) {
	entry := logEntry("[kaniko] 2026-11-09T00:00:01.123Z ERROR RUN exit 2")
	if entry["phase"] != "kaniko" || entry["timestamp"] != "2026-11-09T00:00:01.123Z" || entry["level"] != "error" {
		t.Fatalf("entry=%v", entry)
	}
}

func TestBuildTerminalTailAfterPrefixCap(t *testing.T) {
	now := time.Now().UTC()
	b := &templateBuild{Status: sandbox.BuildError, FinishedAt: &now}
	stage := []sandbox.BuildLogStage{{Phase: "kaniko", Lines: captureLines(10000), Truncated: true, Tail: []string{"RUN exit 2", "deliberate stderr"}}}
	foldBuildCapture(b, nil, stage, false, now)
	prefixLen := len(b.Logs)
	foldBuildCapture(b, nil, stage, false, now.Add(time.Minute))
	if len(b.Logs) != prefixLen+2 || b.Logs[len(b.Logs)-1] != "[kaniko terminal tail] deliberate stderr" || b.Diagnostics.ArchivedAt == nil {
		t.Fatalf("terminal error hidden by prefix cap: %+v", b.Diagnostics)
	}
	foldBuildCapture(b, nil, stage, false, now.Add(2*time.Minute))
	if len(b.Logs) != prefixLen+2 {
		t.Fatal("terminal tail duplicated")
	}
}

func TestMergeConcurrentPreservesDiagnostics(t *testing.T) {
	fresh := &sandboxTemplateDoc{Builds: []templateBuild{{BuildID: "b", Status: sandbox.BuildError, Logs: []string{"durable"}, Diagnostics: &buildDiagnostics{Revision: 3}}}}
	stale := &sandboxTemplateDoc{Builds: []templateBuild{{BuildID: "b", Status: sandbox.BuildError, Diagnostics: &buildDiagnostics{Revision: 1}}}}
	mergeConcurrent(stale, fresh)
	if stale.Builds[0].Diagnostics.Revision != 3 || stale.Builds[0].Logs[0] != "durable" {
		t.Fatal("stale template save lost durable logs")
	}
}
