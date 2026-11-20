package platformlife

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// tenants puts on the fake platform cluster what the drill's lab had: one tenant cluster (its Application, its
// TenantControlPlane, the Deployment Kamaji runs it as), one database (its Application, its CloudNativePG Cluster,
// its instance pod) and both operators. The methods move the facts the kubelet and the controller manager write; the
// status the operators and ArgoCD write is set once, to Ready and Healthy, and STAYS so, as it did on the lab for
// five minutes after the drain had evicted them (v0101lab/k1: the TenantControlPlane read Ready until 17:59:34 while
// the tenant API was away from 17:54:19).
type tenants struct{ p *platform }

const (
	tcpKey = "pc-6afbcdcd/pc-c-c4287fff-kamaji-cp"
	dbKey  = "pc-db-6afbcdcd/pc-d-64a9ba58"
)

func (tn tenants) deployment(ns, name string, available int) {
	tn.p.wl.put("/apis/apps/v1/deployments/"+ns+"."+name, map[string]any{
		"metadata": map[string]any{"namespace": ns, "name": name, "generation": 3},
		"spec":     map[string]any{"replicas": 1},
		"status":   map[string]any{"observedGeneration": 3, "replicas": 1, "availableReplicas": available}})
}

func (tn tenants) controlPlane(available int) {
	tn.deployment("pc-6afbcdcd", "pc-c-c4287fff-kamaji-cp", available)
}
func (tn tenants) kamaji(available int) { tn.deployment("kamaji-system", "kamaji", available) }
func (tn tenants) cnpg(available int)   { tn.deployment("cnpg-system", "cnpg-cloudnative-pg", available) }

// instance is the database's one pod: absent (evicted), or there with its Ready condition.
func (tn tenants) instance(ready string) {
	const path = "/api/v1/pods/pc-d-64a9ba58-1"
	if ready == "" {
		tn.p.wl.mu.Lock()
		delete(tn.p.wl.objs, path)
		tn.p.wl.mu.Unlock()
		return
	}
	tn.p.wl.put(path, map[string]any{
		"metadata": map[string]any{"namespace": "pc-db-6afbcdcd", "name": "pc-d-64a9ba58-1",
			"labels": map[string]any{"cnpg.io/cluster": "pc-d-64a9ba58", "cnpg.io/podRole": "instance"}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}}})
}

func newTenants(p *platform) tenants {
	tn := tenants{p}
	for _, name := range []string{"pc-c-c4287fff", "pc-d-64a9ba58"} {
		app := meta(name, map[string]any{"app.kubernetes.io/managed-by": "proxcloud"})
		app["status"] = map[string]any{"health": map[string]any{"status": "Healthy"}}
		p.wl.put(appsPath+name, app)
	}
	p.wl.put("/apis/kamaji.clastix.io/v1alpha1/tenantcontrolplanes/k1", map[string]any{
		"metadata": map[string]any{"namespace": "pc-6afbcdcd", "name": "pc-c-c4287fff-kamaji-cp"},
		"status":   map[string]any{"kubernetesResources": map[string]any{"version": map[string]any{"status": "Ready"}}}})
	p.wl.put("/apis/postgresql.cnpg.io/v1/clusters/d1", map[string]any{
		"metadata": map[string]any{"namespace": "pc-db-6afbcdcd", "name": "pc-d-64a9ba58"},
		"spec":     map[string]any{"instances": 1},
		"status":   map[string]any{"readyInstances": 1, "phase": "Cluster in healthy state"}})
	// A pod of the database's initdb Job carries the cluster's label too, and is no instance.
	p.wl.put("/api/v1/pods/pc-d-64a9ba58-1-initdb", map[string]any{
		"metadata": map[string]any{"namespace": "pc-db-6afbcdcd", "name": "pc-d-64a9ba58-1-initdb",
			"labels": map[string]any{"cnpg.io/cluster": "pc-d-64a9ba58", "cnpg.io/jobRole": "initdb"}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
	tn.controlPlane(1)
	tn.kamaji(1)
	tn.cnpg(1)
	tn.instance("True")
	return tn
}

// unit ends step n of the roll (1 to 4) on the machines' side and makes one look.
func (p *platform) unit(n int) string {
	p.machine([]string{"pc-platform-cp", "pc-platform-system-pve1", "pc-platform-kamaji-pve2", "pc-platform-dbaas-pve3"}[n-1], "1.35.11")
	return p.once()
}

// With every tenant really there, the wait between two units holds nothing: four looks, four units.
func TestRollPassesTheTenantWaitWhenEverythingIsAvailable(t *testing.T) {
	p, _ := rolling(t)
	newTenants(p)
	p.once()
	for n := 1; n <= 3; n++ {
		if end, st := p.unit(n), p.stage(stageCluster); end != "defer:waiting_cluster" || p.rollStep() != n+1 || st.Code == CodeTenantsUnhealthy {
			t.Fatalf("unit %d: %s, step %d, %+v", n, end, p.rollStep(), st)
		}
	}
	p.machine("pc-platform-dbaas-pve3", "1.35.11")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
	if _, in, _ := p.state.snapshot(); in.Status.TenantsDownBefore != nil {
		t.Fatalf("the note outlived the upgrade: %v", *in.Status.TenantsDownBefore)
	}
}

// The drill of 0.10.1-rc.2, unit kamaji: the drain evicted the tenant's control plane pod and, with it, the Kamaji
// operator. The TenantControlPlane went on saying Ready and ArgoCD Healthy; the run stored step 4 after 76 seconds and
// cloned the database unit's machine while the only tenant API server was away for 5 minutes 38 seconds more. The
// wait reads the Deployments: it holds until the control plane's has its replica, whatever the stale status says, and
// its deadline fails the run under tenants_unhealthy with the names.
func TestRollWaitsForATenantControlPlaneWhoseStatusStaysReady(t *testing.T) {
	p, _ := rolling(t)
	tn := newTenants(p)
	p.once()
	p.unit(1)
	if p.unit(2); p.rollStep() != 3 {
		t.Fatalf("before the kamaji unit: step %d: %+v", p.rollStep(), p.stage(stageCluster))
	}
	tn.controlPlane(0) // evicted with the machine
	tn.kamaji(0)       // and the operator that would have said so
	end, st := p.unit(3), p.stage(stageCluster)
	if end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 3 ||
		!strings.Contains(st.Message, "Deployment "+tcpKey+" (0 of 1 replicas available)") || !strings.Contains(st.Message, "Operator kamaji-system/kamaji (0 of 1 replicas available)") ||
		strings.Contains(st.Message, "TenantControlPlane") || strings.Contains(st.Message, "Application") {
		t.Fatalf("the kamaji unit with the tenant's control plane away: %s, step %d, %+v", end, p.rollStep(), st)
	}
	// The operator is back first and has not looked yet: its word is still not taken for the control plane.
	tn.kamaji(1)
	if end, st := p.once(), p.stage(stageCluster); end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 3 ||
		strings.Contains(st.Message, "Operator") || !strings.Contains(st.Message, "1 of the 7 things") {
		t.Fatalf("with the operator back: %s, step %d, %+v", end, p.rollStep(), st)
	}
	// A Deployment whose last change the controller manager has not seen does not count either.
	d := p.wl.obj("/apis/apps/v1/deployments/pc-6afbcdcd.pc-c-c4287fff-kamaji-cp")
	d["status"] = map[string]any{"observedGeneration": 2, "replicas": 1, "availableReplicas": 1}
	if end, st := p.once(), p.stage(stageCluster); end != "defer:waiting_cluster" || p.rollStep() != 3 || !strings.Contains(st.Message, "not observed yet") {
		t.Fatalf("an unobserved Deployment: %s, step %d, %+v", end, p.rollStep(), st)
	}
	tn.controlPlane(0)
	// The deadline ends the wait with the same plain words; the unit after it has not started.
	p.mutate(func(run *Run, _ *Installation, _ *Secrets) {
		for i := range run.Stages {
			if past := time.Now().Add(-time.Second); run.Stages[i].ID == stageCluster {
				run.Stages[i].Deadline = &past
			}
		}
	})
	if end, st := p.once(), p.stage(stageCluster); end != "fail:"+CodeTenantsUnhealthy || p.rollStep() != 3 || !strings.Contains(st.Message, "Deployment "+tcpKey) ||
		!strings.HasPrefix(p.rendered(), "cp 1.35.11 pve1=9201; system 1.35.11 pve1=9201; kamaji 1.35.11 pve2=9202; dbaas 1.35.10") {
		t.Fatalf("after the deadline: %s, step %d, rendered %s, %+v", end, p.rollStep(), p.rendered(), st)
	}
	// Retry waits for the same tenant, and goes on when its API server is back.
	p.begin(t, "", "")
	if end, st := p.until(30), p.stage(stageCluster); end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 3 {
		t.Fatalf("Retry with the control plane still away: %s, step %d, %+v", end, p.rollStep(), st)
	}
	tn.controlPlane(1)
	if end := p.once(); end != "defer:waiting_cluster" || p.rollStep() != 4 {
		t.Fatalf("with the control plane back: %s, step %d: %+v", end, p.rollStep(), p.stage(stageCluster))
	}
}

// The same for a database: its pod is evicted with the dbaas machine. With the CloudNativePG operator away too, the
// Cluster's status and the Application stay as they were; the instance pods are counted instead, and a pod that is
// there and not Ready is not counted.
func TestRollWaitsForADatabaseWhoseStatusStaysReady(t *testing.T) {
	p, _ := rolling(t)
	tn := newTenants(p)
	p.once()
	for n := 1; n <= 3; n++ {
		p.unit(n)
	}
	tn.instance("")
	tn.cnpg(0)
	end, st := p.unit(4), p.stage(stageCluster)
	if end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 4 ||
		!strings.Contains(st.Message, "Database "+dbKey+" (0 of 1 instances Ready)") || !strings.Contains(st.Message, "Operator cnpg-system/cnpg-cloudnative-pg") {
		t.Fatalf("the dbaas unit with the database away: %s, step %d, %+v", end, p.rollStep(), st)
	}
	tn.cnpg(1)
	tn.instance("False") // scheduled on the new node, its volume not attached yet
	if end, st := p.once(), p.stage(stageCluster); end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 4 {
		t.Fatalf("with the pod not Ready: %s, step %d, %+v", end, p.rollStep(), st)
	}
	_, in, _ := p.state.snapshot()
	if c := in.Status.Clusters[0]; c.KubernetesVersion != "1.35.10" {
		t.Fatalf("the record names the new version while the last unit's tenants are away: %+v", c)
	}
	tn.instance("True")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
}

// What was down before the upgrade is noted once and not waited for: a control plane without its replica, a database
// without its instance, by name (their counts may move). A tenant that goes down during the roll still holds it.
func TestRollDoesNotWaitForTenantsDownBefore(t *testing.T) {
	p, _ := rolling(t)
	tn := newTenants(p)
	tn.controlPlane(0)
	tn.instance("")
	p.wl.put("/apis/postgresql.cnpg.io/v1/clusters/d2", map[string]any{
		"metadata": map[string]any{"namespace": "pc-db-6afbcdcd", "name": "pc-d-00000002"}, "spec": map[string]any{"instances": 2}})
	pod := func(n, ready string) {
		p.wl.put("/api/v1/pods/pc-d-00000002-"+n, map[string]any{
			"metadata": map[string]any{"namespace": "pc-db-6afbcdcd", "name": "pc-d-00000002-" + n, "labels": map[string]any{"cnpg.io/cluster": "pc-d-00000002", "cnpg.io/podRole": "instance"}},
			"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}}})
	}
	pod("1", "True")
	pod("2", "True")
	p.once()
	_, in, _ := p.state.snapshot()
	if want := []string{"Database " + dbKey + " (0 of 1 instances Ready)", "Deployment " + tcpKey + " (0 of 1 replicas available)"}; in.Status.TenantsDownBefore == nil ||
		!slices.Equal(*in.Status.TenantsDownBefore, want) {
		t.Fatalf("the note: %v", in.Status.TenantsDownBefore)
	}
	p.unit(1)
	if p.unit(2); p.rollStep() != 3 {
		t.Fatalf("two tenants that were down before held the roll: step %d: %+v", p.rollStep(), p.stage(stageCluster))
	}
	pod("2", "False") // a database that was up loses one of its two instances
	if end, st := p.unit(3), p.stage(stageCluster); end != "defer:waiting_cluster" || st.Code != CodeTenantsUnhealthy || p.rollStep() != 3 ||
		!strings.Contains(st.Message, "Database pc-db-6afbcdcd/pc-d-00000002 (1 of 2 instances Ready)") || strings.Contains(st.Message, tcpKey) || strings.Contains(st.Message, dbKey) {
		t.Fatalf("a tenant that went down in the roll: %s, step %d, %+v", end, p.rollStep(), st)
	}
	pod("2", "True")
	p.once()
	p.machine("pc-platform-dbaas-pve3", "1.35.11")
	if end := p.until(30); end != "complete" {
		t.Fatalf("end %s: %+v", end, p.stage(stageCluster))
	}
}

// A note taken by a release that read only the Applications and the TenantControlPlanes covers the same tenants'
// Deployment and Database; nothing else is covered by it.
func TestDownBefore(t *testing.T) {
	before := []string{"Application pc-d-1 (Degraded)", "TenantControlPlane t-1/k1 (NotReady)", "Operator cnpg-system/cnpg (0 of 1 replicas available)"}
	for d, want := range map[string]bool{
		"Application pc-d-1 (Degraded)":                       true,
		"Application pc-d-1 (Missing)":                        false, // its state moved: the run waits
		"Database pc-db-1/pc-d-1 (0 of 1 instances Ready)":    true,
		"Database pc-db-1/pc-d-10 (0 of 1 instances Ready)":   false,
		"Deployment t-1/k1 (0 of 2 replicas available)":       true,
		"Deployment t-1/k10 (0 of 2 replicas available)":      false,
		"Operator cnpg-system/cnpg (no Deployment)":           true,
		"Operator kamaji-system/kamaji (no Deployment)":       false,
		"TenantControlPlane t-1/k1 (Ready)":                   false,
		"TenantControlPlane t-1/k1 (NotReady)":                true,
		"Deployment t-1/k1-other (0 of 1 replicas available)": false,
	} {
		if got := downBefore(before, d); got != want {
			t.Errorf("downBefore(%q) = %v, want %v", d, got, want)
		}
	}
}
