package project

import (
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func TestImportPolicyIsConfiguredExactAndProjectBounded(t *testing.T) {
	project := strings.Repeat("a", 24)
	digest := "sha256:" + strings.Repeat("d", 64)
	cfg := sandbox.Config{TemplateRegistry: "registry.example.org:5443/t"}
	managed := cfg.TemplateRegistry + "/" + project + "/" + strings.Repeat("b", 20) + "@" + digest
	b := importTemplateRequest{Name: "app", Source: managed}
	if err := validateImportRequest(&b, project, cfg); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{strings.Replace(managed, project, strings.Repeat("e", 24), 1), strings.Replace(managed, strings.Repeat("b", 20), "not-a-template", 1), "registry.example.org:5443/customer/app@" + digest, "external.example.org/approved/app@" + digest} {
		b.Source = source
		if err := validateImportRequest(&b, project, cfg); err == nil {
			t.Fatalf("unapproved source accepted: %s", source)
		}
	}
	cfg.Import.SourceRepositories = map[string][]string{"https://external.example.org": {"approved/app", "projects/{project}/app"}, "https://registry.example.org:5443": {"customer/app"}}
	b.Source = "external.example.org/approved/app@" + digest
	if err := validateImportRequest(&b, project, cfg); err == nil {
		t.Fatal("external source used a platform credential fallback")
	}
	b.Registry = &struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{"caller", "private"}
	if err := validateImportRequest(&b, project, cfg); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"external.example.org/approved/app-child@" + digest, "external.example.org/approved/app/child@" + digest, "registry.example.org:5443/t/" + strings.Repeat("e", 24) + "/" + strings.Repeat("b", 20) + "@" + digest} {
		b.Source = source
		if err := validateImportRequest(&b, project, cfg); err == nil {
			t.Fatalf("prefix/foreign source accepted: %s", source)
		}
	}
	b.Source = "registry.example.org:5443/customer/app@" + digest
	if err := validateImportRequest(&b, project, cfg); err != nil {
		t.Fatal("same-host caller repository rejected", err)
	}
	b.Registry = nil
	cfg.Import.AllowAnonymous = true
	if err := validateImportRequest(&b, project, cfg); err != nil {
		t.Fatal("explicit operator anonymous policy rejected", err)
	}
}

func TestImportIdempotenceUsesRuntimeSettingsNotNamesOrCredentials(t *testing.T) {
	b := importTemplateRequest{Source: "registry.example.org/app@sha256:" + strings.Repeat("a", 64), Name: "first"}
	key := importKey(b, 2, 2)
	b.Name = "second"
	b.Aliases = []string{"another"}
	b.Tags = []string{"v1"}
	if importKey(b, 2, 2) != key {
		t.Fatal("names must not create a second artifact")
	}
	b.ReadyCmd = "ready"
	if importKey(b, 2, 2) == key {
		t.Fatal("runtime readiness settings omitted")
	}
}

func TestSuccessfulHistoryRetentionKeepsTombstonesAndActiveHolds(t *testing.T) {
	tdoc := &sandboxTemplateDoc{Tags: map[string]string{"old": "tagged"}, Builds: []templateBuild{{BuildID: "tagged", Status: sandbox.BuildReady}, {BuildID: "old-ready", Status: sandbox.BuildReady}, {BuildID: "failed", Status: sandbox.BuildError, Diagnostics: &buildDiagnostics{CleanedAt: &time.Time{}}}}}
	for i := 0; i < 20; i++ {
		tdoc.Builds = append(tdoc.Builds, templateBuild{BuildID: strings.Repeat("x", i+1), Status: sandbox.BuildReady})
	}
	retainTemplateBuilds(tdoc)
	if tdoc.build("old-ready") == nil || tdoc.build("tagged") == nil {
		t.Fatal("successful artifact reference root dropped")
	}
	if tdoc.build("failed") != nil {
		t.Fatal("old cleaned failed history not bounded")
	}
	if _, ok := tdoc.RetiredBuildIDs["failed"]; !ok {
		t.Fatal("failed build identity lost its nonexpiring tombstone")
	}
}
