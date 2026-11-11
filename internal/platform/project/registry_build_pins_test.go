package project

import (
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"testing"
)

func TestRegistryMergeFinishedTagsPreserveIntentionalEdits(t *testing.T) {
	for _, tc := range []struct {
		name, status, target string
		want                 string
	}{
		{"stale building sees completed tag", sandbox.BuildBuilding, "", "x"},
		{"ready intentionally removes tag", sandbox.BuildReady, "", ""},
		{"ready intentionally remaps tag", sandbox.BuildReady, "y", "y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := sandboxTemplateDoc{Tags: map[string]string{"default": "x"}, Builds: []templateBuild{{BuildID: "x", Status: sandbox.BuildReady, Registry: &buildRegistryOperation{State: buildPinFinished}}}}
			old := sandboxTemplateDoc{Tags: map[string]string{}, Builds: []templateBuild{{BuildID: "x", Status: tc.status}}}
			if tc.target != "" {
				old.Tags["default"] = tc.target
			}
			mergeConcurrent(&old, &cur)
			if old.Tags["default"] != tc.want || old.Builds[0].Status != sandbox.BuildReady || old.Builds[0].Registry == nil {
				t.Fatalf("merged=%+v", old)
			}
		})
	}
}

func TestRegistryRetiredIdentityRejectsStalePrePrepareSnapshot(t *testing.T) {
	for _, cleaned := range []bool{false, true} {
		old := templateBuild{BuildID: "retired", Status: sandbox.BuildReady, Registry: &buildRegistryOperation{State: buildPinFinished}}
		cur := sandboxTemplateDoc{RetiredBuilds: []templateBuild{old}}
		cur.rememberRetiredBuild(old)
		if cleaned {
			cur.RetiredBuilds = nil
		}
		stale := sandboxTemplateDoc{Tags: map[string]string{}, Builds: []templateBuild{{BuildID: "retired", Status: sandbox.BuildWaiting}, {BuildID: "fresh", Status: sandbox.BuildWaiting}}}
		mergeConcurrent(&stale, &cur)
		if stale.build("retired") != nil || stale.build("fresh") == nil {
			t.Fatalf("cleaned=%t stale build resurrected: %+v", cleaned, stale)
		}
		if _, ok := stale.RetiredBuildIDs["retired"]; !ok {
			t.Fatal("durable identity lost")
		}
	}
}

func TestRegistryTerminalHandoffIsNotPubliclyReady(t *testing.T) {
	tmpl := sandboxTemplateDoc{Tags: map[string]string{"default": "new"}, Builds: []templateBuild{{BuildID: "old", Status: sandbox.BuildReady}, {BuildID: "new", Status: sandbox.BuildReady, Registry: &buildRegistryOperation{State: buildTerminalObserved}}}}
	if tmpl.e2bView()["buildStatus"] != sandbox.BuildBuilding {
		t.Fatal("list exposed early ready")
	}
	if b := tmpl.defaultBuild(); b == nil || b.BuildID != "old" {
		t.Fatal("default selected unfinished handoff")
	}
	if v := consoleView(&tmpl); v.Status != sandbox.BuildBuilding || v.Builds[0].Status != sandbox.BuildBuilding {
		t.Fatal("console exposed early ready")
	}
	tmpl.Builds = tmpl.Builds[1:]
	if tmpl.defaultBuild() != nil {
		t.Fatal("unfinished template usable")
	}
	tmpl.Builds[0].Registry.State = buildPinFinished
	if tmpl.defaultBuild() == nil {
		t.Fatal("completed template not usable")
	}
}

func TestRegistryTrimNeverDropsUnknownOrHandoff(t *testing.T) {
	for _, state := range []string{buildPreparing, buildPinned, buildDispatching, buildTerminalObserved, buildPinUnknown} {
		tmpl := sandboxTemplateDoc{Tags: map[string]string{}, Builds: []templateBuild{{BuildID: "held", Status: sandbox.BuildError, Registry: &buildRegistryOperation{State: state}}}}
		for i := 0; i < keptBuilds+1; i++ {
			tmpl.Builds = append(tmpl.Builds, templateBuild{BuildID: "old", Status: sandbox.BuildError})
		}
		trimBuilds(&tmpl)
		if tmpl.build("held") == nil {
			t.Fatalf("trimmed %s operation", state)
		}
	}
}
