package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// releaseJobs records what the runner does with the one job it is given.
type releaseJobs struct {
	job   *durablejobs.Job
	calls []string
	delay time.Duration
}

func (q *releaseJobs) ClaimKinds(_ context.Context, _ time.Duration, kinds []string) (*durablejobs.Job, error) {
	if len(kinds) != 1 || kinds[0] != durablejobs.KamajiRelease {
		return nil, fmt.Errorf("claimed %v", kinds)
	}
	j := q.job
	q.job = nil
	return j, nil
}
func (q *releaseJobs) Complete(context.Context, *durablejobs.Job, func(context.Context) error) error {
	q.calls = append(q.calls, "complete")
	return nil
}
func (q *releaseJobs) Defer(_ context.Context, _ *durablejobs.Job, d time.Duration, code string) error {
	q.calls, q.delay = append(q.calls, "defer "+code), d
	return nil
}
func (q *releaseJobs) Fail(_ context.Context, _ *durablejobs.Job, _ time.Duration, permanent bool, code string) error {
	q.calls = append(q.calls, fmt.Sprintf("fail %s permanent=%v", code, permanent))
	return nil
}

func TestKamajiReleaseRunner(t *testing.T) {
	const payload = `{"serviceId":"k8s","projectId":"p1","clusterId":"pc-c-a"}`
	down := errors.New("management cluster: connection refused")
	for name, tc := range map[string]struct {
		payload string
		age     time.Duration
		done    bool
		err     error
		want    string
		looks   int
	}{
		"cascade running":         {payload, time.Minute, false, nil, "defer cascade_running", 1},
		"cascade finished":        {payload, time.Minute, true, nil, "complete", 1},
		"management cluster down": {payload, time.Minute, false, down, "defer release_failed", 1},
		// The last look still counts: a cascade that ended in the job's last minutes is released.
		"finished at the end of its life": {payload, kamajiReleaseLifetime + time.Hour, true, nil, "complete", 1},
		"gave up":                         {payload, kamajiReleaseLifetime + time.Hour, false, nil, "fail cascade_not_finished permanent=true", 1},
		"gave up on an error":             {payload, kamajiReleaseLifetime + time.Hour, false, down, "fail cascade_not_finished permanent=true", 1},
		"payload of another shape":        {`{"clusterId":"pc-c-a"}`, time.Minute, true, nil, "fail invalid_payload permanent=true", 0},
	} {
		t.Run(name, func(t *testing.T) {
			q := &releaseJobs{job: &durablejobs.Job{ID: "j1", Kind: durablejobs.KamajiRelease, Payload: []byte(tc.payload), CreatedAt: time.Now().Add(-tc.age)}}
			looks := 0
			r := &KamajiReleaseRunner{Jobs: q, Log: slog.Default(), Finalize: func(_ context.Context, service, project, cluster string) (bool, error) {
				looks++
				if service != "k8s" || project != "p1" || cluster != "pc-c-a" {
					t.Errorf("finalize of %s/%s/%s", service, project, cluster)
				}
				return tc.done, tc.err
			}}
			did, err := r.RunOnce(context.Background())
			if !did || err != nil {
				t.Fatalf("did %v, err %v", did, err)
			}
			if len(q.calls) != 1 || q.calls[0] != tc.want || looks != tc.looks {
				t.Fatalf("calls %v after %d looks, want [%s] after %d", q.calls, looks, tc.want, tc.looks)
			}
			if strings.HasPrefix(tc.want, "defer") && (q.delay < kamajiReleaseMinWait || q.delay > kamajiReleaseMaxWait) {
				t.Fatalf("waits %s", q.delay)
			}
			if did, err := r.RunOnce(context.Background()); did || err != nil {
				t.Fatalf("an empty queue: did %v, err %v", did, err)
			}
		})
	}
}

func TestKamajiReleaseWait(t *testing.T) {
	for age, want := range map[time.Duration]time.Duration{
		0: 15 * time.Second, time.Minute: 15 * time.Second, 4 * time.Minute: time.Minute, time.Hour: 5 * time.Minute,
	} {
		if got := kamajiReleaseWait(age); got != want {
			t.Errorf("at %s the job waits %s, want %s", age, got, want)
		}
	}
}

type oneService struct{}

func (oneService) Get(context.Context, string) (*externalservice.ExternalService, error) {
	return &externalservice.ExternalService{ID: "pve"}, nil
}

type noShares struct{}

func (noShares) FindByProjectAndType(context.Context, string, string) ([]cloud.CloudResource, error) {
	return nil, nil
}

// CAPMOX deletes the workers with the cluster's token, so the release keeps the token while a VM of the cluster, or
// one it cannot tell apart, is left in the project's pool.
func TestKamajiReleaseKeepsTheTokenWhileAWorkerIsLeft(t *testing.T) {
	var vms string
	var revoked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case r.Method == http.MethodGet && path == "/cluster/resources" && r.URL.Query().Get("type") == "node":
			fmt.Fprint(w, `{"data":[{"type":"node","node":"pve1","status":"online"},{"type":"node","node":"pve2","status":"offline"}]}`)
		case r.Method == http.MethodGet && path == "/cluster/resources":
			fmt.Fprintf(w, `{"data":[%s]}`, vms)
		case r.Method == http.MethodDelete && strings.Contains(path, "/token/"):
			revoked = append(revoked, path)
			fmt.Fprint(w, `{"data":null}`)
		default:
			http.Error(w, "no fake for "+r.Method+" "+path, http.StatusNotImplemented)
		}
	}))
	defer srv.Close()
	release := KamajiReleaseCluster(oneService{}, noShares{}, func(*externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "s"}, "pve", "pc-")
	}, nil, false, nil)
	ctx := context.Background()

	for name, rows := range map[string]string{
		"a worker of the cluster": `{"type":"qemu","vmid":124,"node":"pve1","name":"pc-c-a-workers-pve1-x7k2p","pool":"pc-p1","status":"running"}`,
		"a VM on a dead node":     `{"type":"qemu","vmid":130,"node":"pve2","pool":"pc-p1","status":"unknown"}`,
	} {
		vms = rows
		if err := release(ctx, "pve", "p1", "pc-c-a"); err == nil || len(revoked) != 0 {
			t.Fatalf("%s: err %v, revoked %v: want the token kept", name, err, revoked)
		}
	}
	// The tenant's own VM, another cluster's worker in the same pool, this cluster's name in another project's pool.
	vms = `{"type":"qemu","vmid":125,"node":"pve1","name":"web","pool":"pc-p1","status":"running"},
		{"type":"qemu","vmid":126,"node":"pve1","name":"pc-c-ab-workers-pve1-aaaaa","pool":"pc-p1","status":"running"},
		{"type":"qemu","vmid":127,"node":"pve1","name":"pc-c-a-workers-pve1-bbbbb","pool":"pc-p2","status":"running"}`
	if err := release(ctx, "pve", "p1", "pc-c-a"); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 2 || !strings.HasSuffix(revoked[0], "/access/users/capmox@pve/token/pc-c-a") {
		t.Fatalf("revoked = %v", revoked)
	}
}
