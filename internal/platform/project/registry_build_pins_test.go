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
