//go:build sandboxlive

// Live drill of the sandbox registry (docs/managed-sandbox.md §6) on a sandbox cluster whose registry account sync
// runs (installer stage 55), with the proxcloud service account's kubeconfig:
//
//	SANDBOX_KUBECONFIG=~/sa.kubeconfig SANDBOX_SERVICE=<the sync's SERVICE_ID or any> SANDBOX_PROJECTS=a1,b1 \
//	SANDBOX_LIBRARY_IMAGE=<host>/library/python:3.12-slim@sha256:... SANDBOX_LIBRARY_OLD=<same repo, other digest> \
//	SANDBOX_OWN_IMAGE=<host>/t/a1/app:1 SANDBOX_OTHER_IMAGE=<host>/t/b1/app:1 \
//	  go test -tags sandboxlive ./internal/cloud/sandbox/ -run TestRegistryLive -v
//
// Project a1 starts a sandbox from the library image and from its own template, both through its own pull Secret;
// one from b1's repository must not pull. Then image adoption: a template pinned at SANDBOX_LIBRARY_OLD is offered
// the digest the prepull DaemonSet runs.
package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRegistryLive(t *testing.T) {
	kc, err := os.ReadFile(os.Getenv("SANDBOX_KUBECONFIG"))
	if err != nil {
		t.Skipf("SANDBOX_KUBECONFIG: %v", err)
	}
	projects := strings.Split(os.Getenv("SANDBOX_PROJECTS"), ",")
	lib, own, other := os.Getenv("SANDBOX_LIBRARY_IMAGE"), os.Getenv("SANDBOX_OWN_IMAGE"), os.Getenv("SANDBOX_OTHER_IMAGE")
	host, _, _ := strings.Cut(lib, "/")
	keep := []string{"sh", "-c", "echo registry-live; exec sleep infinity"}
	cfg := Config{
		Kubeconfig: string(kc), TemplateRegistry: host + "/t",
		Templates: map[string]Template{
			"lib":   {Image: lib, CPU: 1, MemoryGiB: 1, Command: keep},
			"own":   {Image: own, CPU: 1, MemoryGiB: 1, Command: keep},
			"other": {Image: other, CPU: 1, MemoryGiB: 1, Command: keep},
		},
		Limits: Limits{MaxConcurrent: 4},
	}
	s, err := New(cfg, os.Getenv("SANDBOX_SERVICE"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := projects[0]
	for _, p := range projects {
		p := p
		if err := s.EnsureProjectNamespace(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.DeleteProjectNamespace(context.Background(), p) })
	}
	// The account sync writes the project's Secret on its next pass (30 s); the portal's account cannot read Secrets, so
	// the kubelet's retries are what wait for it.

	waitPhase := func(id string, want func(st map[string]any) (bool, string)) string {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			pods, err := s.api.ListPods(ctx, NamespaceFor(a), LabelSandboxID+"="+id)
			if err == nil && len(pods) == 1 {
				cs, _ := dig(pods[0], "status", "containerStatuses").([]any)
				for _, c := range cs {
					if ok, why := want(c.(map[string]any)); ok {
						return why
					}
				}
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("%s: no expected state within 4m", id)
		return ""
	}
	running := func(c map[string]any) (bool, string) {
		return dig(c, "state", "running") != nil, str(c["imageID"])
	}
	refused := func(c map[string]any) (bool, string) {
		msg := str(dig(c, "state", "waiting", "message"))
		r := str(dig(c, "state", "waiting", "reason"))
		return (r == "ErrImagePull" || r == "ImagePullBackOff") && msg != "", r + ": " + msg
	}
	for _, tc := range []struct {
		tpl  string
		want func(map[string]any) (bool, string)
	}{{"lib", running}, {"own", running}, {"other", refused}} {
		sp := Spec{ProjectID: a, Template: tc.tpl, Name: tc.tpl}
		t0 := time.Now()
		if _, err := s.CreateSandbox(ctx, &sp); err != nil {
			t.Fatalf("create %s: %v", tc.tpl, err)
		}
		cr, _ := s.api.GetCR(ctx, CRGroup, CRVersion, CRPlural, NamespaceFor(a), sp.ID)
		if ps := dig(cr, "spec", "podTemplate", "spec", "imagePullSecrets"); ps == nil {
			t.Fatalf("%s: the pod names no pull Secret", tc.tpl)
		}
		why := waitPhase(sp.ID, tc.want)
		t.Logf("%s (%s) after %s: %s", tc.tpl, cfg.Templates[tc.tpl].Image, time.Since(t0).Round(time.Second), why)
		if err := s.DeleteSandbox(ctx, a, sp.ID); err != nil {
			t.Fatalf("delete %s: %v", tc.tpl, err)
		}
	}

	if old := os.Getenv("SANDBOX_LIBRARY_OLD"); old != "" {
		s.cfg.Templates = map[string]Template{"lib": {Image: old, CPU: 1, MemoryGiB: 1}}
		ads, err := s.TemplateAdoptions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ads) != 1 || !ads[0].Ready || ads[0].To != lib || ads[0].From != old {
			t.Fatalf("adoptions %+v", ads)
		}
		t.Logf("adoption: %s -> %s (%d/%d prepull pods)", ads[0].From, ads[0].To, ads[0].Pulled, ads[0].Total)
	}
}
