//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/cloud/syncjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

type teardownDeleter func(ctx context.Context, projectID string) error

func (f teardownDeleter) DeleteProjectResources(ctx context.Context, projectID string) error {
	return f(ctx, projectID)
}

// TestProjectDeleteAfterSandboxesWereSwitchedOff builds what v0.12.0-rc.2 left on a site whose sandboxes were switched
// off (drill E5-A, the lab's project e1-del): the sandbox provider DISABLED with the kubeconfig of an identity that is
// gone, an installation that records no built sandboxes, a project in DELETE_IN_PROGRESS with a row still RUNNING, and
// a second project with rows. Every sandbox service here is the production one, built from the stored row.
//
// As it was: the deletion sweep fails on 401 at every pass, the read warns every minute, a suspension cannot end the
// rows. That is also the line that stays: a provider WITH a placement whose cluster refuses, or does not answer, is
// never read as holding nothing. After the pass the API makes at its start the provider has no placement: the next
// sweep deletes the project, the rows end at their true end, and no cluster is asked for any of it.
func TestProjectDeleteAfterSandboxesWereSwitchedOff(t *testing.T) {
	ctx := context.Background()
	f := newSbxFixture(t, gatesHold)
	enc := textcrypt.New("test-key")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// Sandboxes ran while the provider was live. One ended by its timeout two hours ago and its row was never ended
	// (E2-B); one had hours left when the machines went.
	stale, long := f.create(), f.create()
	started := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	timeout := started.Add(time.Hour)
	for id, shutdown := range map[string]time.Time{stale.ID: timeout, long.ID: time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)} {
		row := f.row(id)
		row.Data["status"], row.Data["runningSince"], row.Data["shutdownTime"] = sandbox.StatusRunning, started.Format(time.RFC3339), shutdown.Format(time.RFC3339)
		if _, err := f.rows.Update(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	other := func(externalID string, shutdown time.Time) *cloud.CloudResource {
		t.Helper()
		r, err := f.rows.Insert(ctx, &cloud.CloudResource{Type: cloud.TypeSandbox, ExternalID: externalID, ServiceID: "sbx", ProjectID: "pb", Region: "az1",
			Data: map[string]any{"status": sandbox.StatusRunning, "template": "python", "cpu": 1, "memoryGiB": 2,
				"runningSince": started.Format(time.RFC3339), "shutdownTime": shutdown.Format(time.RFC3339)}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	inPB := other("sbx-00000000000000b1", timeout)

	// The off of rc.2: the cluster's identity is gone and the row keeps its kubeconfig. Every call answers 401.
	var asked atomic.Int64
	dead := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`))
	}))
	t.Cleanup(dead.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: dead.Certificate().Raw})
	kubeconfig, err := kamajik8s.BuildKubeconfig("pc-platform", dead.URL, ca, "token-of-an-identity-that-is-gone")
	if err != nil {
		t.Fatal(err)
	}
	docs := admin.NewRepo(f.db)
	rewrite := func(collection, id string, change func(doc pgdoc.M)) {
		t.Helper()
		doc, err := docs.FindDoc(ctx, collection, id)
		if err != nil || doc == nil {
			t.Fatalf("%s/%s: %v %v", collection, id, doc, err)
		}
		change(doc)
		if err := docs.ReplaceDoc(ctx, collection, id, doc); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("externalService", "sbx", func(doc pgdoc.M) {
		cfg := doc["config"].(map[string]any)
		cfg[platformlife.ProviderMarker], cfg["sandbox"].(map[string]any)["mode"] = "pi-1", "shared"
		doc["status"], doc["secret"] = externalservice.StatusDisabled, map[string]any{"kubeconfig": enc.Encrypt(kubeconfig)}
	})
	mustInsert(t, f.db, platformlife.InstallationCollection, pgdoc.M{"_id": "pi-1", "name": "lab", "serviceId": "svc-pve",
		"status": pgdoc.M{"phase": "ready", "providers": pgdoc.M{"sandbox": "sbx"},
			"sandbox": pgdoc.M{"placement": "shared", "nodeRange": "10.200.200.64/28", "proxyAddress": "10.200.200.210", "count": 1}}})
	store := platformlife.NewStore(f.db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	adminH := admin.NewHandler(docs, nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(f.db), slog.Default()), f.es, "", "", "")
	adminH.SetPlatform(store)
	production := func(es *externalservice.ExternalService) (*sandbox.Service, error) {
		return sandbox.New(es.SandboxConfig(), es.ID)
	}
	f.h.SetSandbox(production)
	read := syncjob.New(f.projs, f.es, f.rows, slog.Default()).WithSandboxFactory(production)
	if _, err := f.db.C("project").SetByID(ctx, "pa", pgdoc.M{"status": project.StatusDeleteInProgress, "scheduledForDeletionAt": time.Now().UTC().Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	sweep := func() int {
		t.Helper()
		n, err := project.NewDeletionJob(f.projs, teardownDeleter(f.h.TeardownProject), slog.Default()).ExecuteAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	pb := func() *project.Project {
		t.Helper()
		p, err := f.projs.FindByID(ctx, "pb")
		if err != nil || p == nil {
			t.Fatalf("project pb: %v %v", p, err)
		}
		return p
	}
	stuck := func(why string) {
		t.Helper()
		logs.Reset()
		if n := sweep(); n != 0 {
			t.Fatalf("%s: the sweep deleted %d project(s) over a cluster it could not ask", why, n)
		}
		if p, err := f.projs.FindByID(ctx, "pa"); err != nil || p == nil || p.Status != project.StatusDeleteInProgress {
			t.Fatalf("%s: the project is %+v, %v", why, p, err)
		}
		if f.row(stale.ID) == nil || f.row(long.ID) == nil {
			t.Fatalf("%s: a row was ended on the word of a cluster that did not answer", why)
		}
		if out := logs.String(); !strings.Contains(out, "teardown: delete sandbox") || !strings.Contains(out, "retrying on the next sweep") {
			t.Fatalf("%s: the sweep does not say why it failed:\n%s", why, out)
		}
	}

	// As rc.2 left it. The installation still records built sandboxes here (an off that is not over yet, or sandboxes
	// that are on): the start-up pass does not touch such a provider, whatever its cluster answers.
	if ids, err := adminH.PlatformSandboxOffRepair(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("a provider whose installation records built sandboxes was marked: %v %v", ids, err)
	}
	refused := func(why, reason string) {
		t.Helper()
		logs.Reset()
		if _, err := read.RunSandboxState(ctx); err != nil {
			t.Fatal(err)
		}
		if out := logs.String(); strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "reconcile sandboxes") || strings.Contains(out, "401") != (reason == "401") || f.row(inPB.ID) == nil {
			t.Fatalf("%s: the read of a provider with a placement: one WARN with the reason and the row kept, got\n%s", why, out)
		}
		if n, err := f.h.DeleteProjectSandboxes(ctx, pb()); err == nil || n != 0 || f.row(inPB.ID).Data["status"] != sandbox.StatusRunning {
			t.Fatalf("%s: a suspension over a cluster that cannot be asked: %d ended, err %v, row %v", why, n, err, f.row(inPB.ID).Data["status"])
		}
	}
	refused("401, built sandboxes", "401")
	if asked.Load() == 0 {
		t.Fatal("the cluster of a provider with a placement was not asked")
	}

	// The off ended as rc.2 ended it: no built sandboxes recorded, the kubeconfig still on the row. Not DISABLED, the
	// provider is still not touched.
	rewrite(platformlife.InstallationCollection, "pi-1", func(doc pgdoc.M) { delete(doc["status"].(map[string]any), "sandbox") })
	f.setProvider(externalservice.StatusPublic)
	if ids, err := adminH.PlatformSandboxOffRepair(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("a provider that is not DISABLED was marked: %v %v", ids, err)
	}
	f.setProvider(externalservice.StatusDisabled)
	// The lab's state: the sweep fails at every pass, and says 401.
	stuck("401, the state of the lab")
	if out := logs.String(); !strings.Contains(out, "401") {
		t.Fatalf("the reason is not in the log:\n%s", out)
	}
	// A cluster that does not answer at all is no different.
	dead.Close()
	refused("unreachable", "")

	// The API starts on this release. From here on any call to the cluster would fail: its server is closed.
	began := time.Now().UTC().Truncate(time.Second)
	ids, err := adminH.PlatformSandboxOffRepair(ctx)
	if err != nil || !slices.Equal(ids, []string{"sbx"}) {
		t.Fatalf("the start-up pass: %v %v", ids, err)
	}
	es, err := f.es.Get(ctx, "sbx")
	if err != nil {
		t.Fatal(err)
	}
	since, off := es.SandboxOffSince()
	if !off || since.Before(began) || es.SandboxConfig().Kubeconfig != "" || !es.IsDisabled() || len(es.SandboxConfig().Templates) != 1 {
		t.Fatalf("the provider after the pass: off %v since %s, %+v", off, since, es)
	}
	if again, err := adminH.PlatformSandboxOffRepair(ctx); err != nil || len(again) != 0 {
		t.Fatalf("the pass again: %v %v", again, err)
	}
	if es2, _ := f.es.Get(ctx, "sbx"); es2 == nil || !func() bool { s, ok := es2.SandboxOffSince(); return ok && s.Equal(since) }() {
		t.Fatal("the second pass moved the time of the off")
	}

	// The first sweep finishes the delete.
	logs.Reset()
	if n := sweep(); n != 1 {
		t.Fatalf("the first sweep after the pass deleted %d project(s):\n%s", n, logs.String())
	}
	if p, err := f.projs.FindByID(ctx, "pa"); err != nil || p != nil {
		t.Fatalf("the project is left: %+v %v", p, err)
	}
	if out := logs.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Fatalf("the sweep logged:\n%s", out)
	}
	ended := func(projectID, rowID string) (data map[string]any, charged time.Time) {
		t.Helper()
		if r := f.row(rowID); r != nil {
			t.Fatalf("row %s is left: %v", rowID, r.Data)
		}
		history, err := f.rows.FindArchivedSince(ctx, projectID, "sbx", time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range history {
			if h.CloudResourceID == rowID {
				data = h.Data
			}
		}
		brs, err := billingresource.GetArchivedBillingResources(ctx, f.rows, map[string]billingresource.Provider{cloud.TypeSandbox: billingresource.NewSandboxProvider()},
			projectID, "sbx", time.Now().Add(-time.Hour), billingapi.BillingContext{})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range brs {
			if b.ResourceID == rowID && b.DeletedAt != nil {
				charged = *b.DeletedAt
			}
		}
		if data == nil || data["status"] != sandbox.StatusDeleted || charged.IsZero() {
			t.Fatalf("row %s is not archived DELETED and charged to an end: %v, %s", rowID, data, charged)
		}
		return data, charged
	}
	// The one that ended by its timeout: that time. The one that still ran: the time the provider lost its placement,
	// not its shutdownTime six hours ahead.
	if data, charged := ended("pa", stale.ID); data["endedAt"] != timeout.Format(time.RFC3339) || !charged.Equal(timeout) {
		t.Fatalf("the stale sandbox ends at %v and is charged to %s; its true end is %s", data["endedAt"], charged, timeout)
	}
	if data, charged := ended("pa", long.ID); data["endedAt"] != since.Format(time.RFC3339) || charged.After(since.Add(time.Minute)) {
		t.Fatalf("the sandbox that ran when the machines went ends at %v and is charged to %s; the provider has no placement since %s", data["endedAt"], charged, since)
	}

	// The minutely read ends what is left in a project that stays, and says nothing.
	logs.Reset()
	if _, err := read.RunSandboxState(ctx); err != nil {
		t.Fatal(err)
	}
	if data, charged := ended("pb", inPB.ID); data["endedAt"] != timeout.Format(time.RFC3339) || !charged.Equal(timeout) || logs.Len() != 0 {
		t.Fatalf("the read: ends at %v, charged to %s, log\n%s", data["endedAt"], charged, logs.String())
	}
	// A suspension (DeleteProjectSandboxes) ends a row the read has not seen yet, with no call either.
	// The registry prune asks every sandbox provider's cluster which images it uses, and stops at one it cannot ask:
	// a provider with no placement has none and is passed over.
	f.h.SetSandboxTemplates(f.db.C("sandbox_templates"))
	if err := f.h.PruneSandboxRegistryOlderThan(ctx, time.Hour); err != nil {
		t.Fatalf("the registry prune beside a provider with no placement: %v", err)
	}
	// The row had hours left and the provider lost its placement half an hour ago (the off of this release records
	// the moment the last machine was gone): that moment is the end, not the time of the suspension.
	wentAt := began.Add(-30 * time.Minute)
	rewrite("externalService", "sbx", func(doc pgdoc.M) {
		doc["config"].(map[string]any)["sandbox"].(map[string]any)[externalservice.SandboxOffSinceKey] = wentAt.Format(time.RFC3339)
	})
	late := other("sbx-00000000000000b2", time.Now().UTC().Add(6*time.Hour))
	if n, err := f.h.DeleteProjectSandboxes(ctx, pb()); err != nil || n != 0 {
		t.Fatalf("a suspension on a provider with no placement: %d deleted on a cluster, err %v", n, err)
	}
	if data, charged := ended("pb", late.ID); data["endedAt"] != wentAt.Format(time.RFC3339) || charged.After(wentAt.Add(time.Minute)) {
		t.Fatalf("the suspension ends the row at %v and it is charged to %s; the machines went at %s", data["endedAt"], charged, wentAt)
	}

	// A kubeconfig on the row again (the next build's adoption stores one): the provider has a placement, and its
	// cluster is asked again. Here it is the dead one, so the read fails and the row stays.
	rewrite("externalService", "sbx", func(doc pgdoc.M) { doc["secret"] = map[string]any{"kubeconfig": enc.Encrypt(kubeconfig)} })
	kept := other("sbx-00000000000000b3", timeout)
	logs.Reset()
	if _, err := read.RunSandboxState(ctx); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); f.row(kept.ID) == nil || strings.Count(out, "level=WARN") != 1 {
		t.Fatalf("a row with a kubeconfig beside the mark was read as having no placement: row %v, log\n%s", f.row(kept.ID), out)
	}
	// The API starts again. That row is not what rc.2 left (it carries the mark): the pass leaves it its kubeconfig,
	// and its cluster is still the one that is asked.
	if ids, err := adminH.PlatformSandboxOffRepair(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("the start-up pass took a kubeconfig set beside the mark: %v %v", ids, err)
	}
	if es, err := f.es.Get(ctx, "sbx"); err != nil || es.SandboxConfig().Kubeconfig != kubeconfig {
		t.Fatalf("the kubeconfig beside the mark after the pass: %v", err)
	}
}
