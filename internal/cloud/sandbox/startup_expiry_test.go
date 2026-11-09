package sandbox

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/billingresource"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

// A second/third read is a test failure, not a 120-second scheduling wait. The
// parent watchdog also bounds a regression that never reaches another API call.
type startupExpiryAPI struct {
	*readyCASAPI
	gets, patches int
	maxGets       int
	firstGetErr   error
	readBudgets   []time.Duration
}

func (f *startupExpiryAPI) GetCR(ctx context.Context, g, v, p, ns, name string) (map[string]any, error) {
	f.gets++
	if end, ok := ctx.Deadline(); ok {
		f.readBudgets = append(f.readBudgets, time.Until(end))
	}
	if f.gets > f.maxGets {
		return nil, ErrNotFound
	}
	if f.gets == 1 && f.firstGetErr != nil {
		return nil, f.firstGetErr
	}
	return f.readyCASAPI.GetCR(ctx, g, v, p, ns, name)
}

func (f *startupExpiryAPI) MergePatchCR(ctx context.Context, g, v, p, ns, name string, patch map[string]any) (map[string]any, error) {
	f.patches++
	return f.readyCASAPI.MergePatchCR(ctx, g, v, p, ns, name, patch)
}

func newStartupExpiryFixture(t *testing.T, seconds int) (*Service, *startupExpiryAPI, map[string]any, *time.Time) {
	t.Helper()
	f := &startupExpiryAPI{readyCASAPI: &readyCASAPI{fakeAPI: newFake()}, maxGets: 1}
	s := NewWithAPI(f, testConfig(), "svc")
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sp := Spec{ProjectID: "p1", Template: "python", TimeoutSeconds: seconds}
	if _, err := s.CreateSandbox(context.Background(), &sp); err != nil {
		t.Fatal(err)
	}
	// Create reads the CR once to attach the owned egress policy; count only startup.
	f.gets, f.patches, f.readBudgets = 0, 0, nil
	cr := f.crs["sb-p1/"+sp.ID]
	setReadyAt(cr, now)
	if s.cfg.Startup.TotalSeconds != 900 || s.cfg.Startup.SchedulingSeconds != 120 {
		t.Fatal("expiry must not rely on a shortened startup budget", s.cfg.Startup)
	}
	return s, f, cr, &now
}

func TestWaitReadyAuthoritativeLifetimeExpiryIsTerminalOnFirstRead(t *testing.T) {
	for _, kind := range []string{"provisional", "candidate"} {
		for _, age := range []time.Duration{0, time.Second} {
			t.Run(kind+"/"+age.String(), func(t *testing.T) {
				s, f, cr, now := newStartupExpiryFixture(t, 1)
				ann := cr["metadata"].(map[string]any)["annotations"].(map[string]any)
				if kind == "provisional" {
					cr["spec"].(map[string]any)["shutdownTime"] = now.Add(-age).Format(time.RFC3339)
				} else {
					ann[AnnotationReadyCandidate] = now.Add(-time.Second - age).Format(time.RFC3339)
					ann[AnnotationReadyCandidateUID] = digStr(cr, "metadata", "uid")
					end, err := time.Parse(time.RFC3339, endOf(cr))
					if err != nil || !now.Before(end) {
						t.Fatal("candidate case needs a still-live provisional deadline", end, err)
					}
				}
				originalEnd, originalCandidate := endOf(cr), str(ann[AnnotationReadyCandidate])
				id := digStr(cr, "metadata", "name")
				// Existing GetSandbox callers retain errors.Is deadline compatibility.
				if live, err := s.GetSandbox(context.Background(), "p1", id); live != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errStartupLifetimeExpired) {
					t.Fatal("direct read lost authoritative expiry", live, err)
				}
				f.gets = 0
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				live, err := s.WaitReady(ctx, "p1", id)
				var startup *StartupError
				want := StartupProgress{"runtime", "sandbox lifetime expired during startup", true}
				if live != nil || !errors.As(err, &startup) || startup.Progress != want || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errStartupLifetimeExpired) {
					t.Fatal("expiry was retried or returned usable Ready", live, err)
				}
				if startup.Error() != "sandbox startup runtime: sandbox lifetime expired during startup" {
					t.Fatal("expiry exposed an unstable public reason", startup)
				}
				if f.gets != 1 || f.patches != 0 || endOf(cr) != originalEnd || str(ann[AnnotationReadyCandidate]) != originalCandidate {
					t.Fatal("expiry waited for another tick, patched or renewed lifetime", f.gets, f.patches)
				}
				data := sandboxData(cr)
				if data["status"] != StatusPending || data["ready"] != false || data["runningSince"] != nil || data["runtimeClock"] != "ready" {
					t.Fatal("expired startup became usable or billable", data)
				}
				row := &cloud.CloudResource{ID: "r", Data: data}
				MarkVanished(row, now.Add(time.Minute))
				brs, billErr := billingresource.NewSandboxProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{}, row)
				if billErr != nil || len(brs) != 0 || row.Data["runningSince"] != nil {
					t.Fatal("expired pending Ready-clock sandbox billed", brs, billErr)
				}
			})
		}
	}
}

func TestWaitReadyLiveCandidateKeepsFirstRuntimeOrigin(t *testing.T) {
	s, f, cr, now := newStartupExpiryFixture(t, 60)
	f.maxGets = 2
	first := now.Add(-10 * time.Second)
	ann := cr["metadata"].(map[string]any)["annotations"].(map[string]any)
	ann[AnnotationReadyCandidate] = first.Format(time.RFC3339)
	ann[AnnotationReadyCandidateUID] = digStr(cr, "metadata", "uid")
	id := digStr(cr, "metadata", "name")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		live, err := s.WaitReady(ctx, "p1", id)
		if err != nil || live["status"] != StatusRunning || live["ready"] != true || live["runningSince"] != first.Format(time.RFC3339) || live["shutdownTime"] != first.Add(time.Minute).Format(time.RFC3339) {
			t.Fatal("live candidate was refused or renewed", live, err)
		}
		*now = now.Add(time.Second)
		setReadyAt(cr, *now)
	}
	if f.gets != 2 || f.patches != 1 || digStr(cr, "metadata", "annotations", AnnotationReadyCandidate) != first.Format(time.RFC3339) {
		t.Fatal("Ready flap refreshed the first candidate", f.gets, f.patches)
	}
}

func TestWaitReadyTransientNetworkErrorsRemainRetryable(t *testing.T) {
	for _, getErr := range []error{context.DeadlineExceeded, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("temporary transport failure")}} {
		t.Run(getErr.Error(), func(t *testing.T) {
			s, f, cr, _ := newStartupExpiryFixture(t, 60)
			f.maxGets, f.firstGetErr = 2, getErr
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			live, err := s.WaitReady(ctx, "p1", digStr(cr, "metadata", "name"))
			if err != nil || live["ready"] != true || live["status"] != StatusRunning || f.gets != 2 || f.patches != 2 {
				t.Fatal("transient transport error became terminal", live, err, f.gets, f.patches)
			}
			if len(f.readBudgets) != 2 {
				t.Fatal("read deadline missing", f.readBudgets)
			}
			for _, budget := range f.readBudgets {
				if budget <= 0 || budget > 2*time.Second {
					t.Fatal("retry escaped the original caller deadline", f.readBudgets)
				}
			}
		})
	}
}

func TestStartupExpiryMarkerRequiresTrustedIdentityAndValidTimes(t *testing.T) {
	for _, kind := range []string{"candidate-uid-missing", "candidate-uid-wrong", "object-uid-missing", "resourceVersion-missing", "future-candidate", "malformed-candidate", "malformed-provisional"} {
		t.Run(kind, func(t *testing.T) {
			s, f, cr, now := newStartupExpiryFixture(t, 1)
			meta := cr["metadata"].(map[string]any)
			ann := meta["annotations"].(map[string]any)
			ann[AnnotationReadyCandidate] = now.Add(-2 * time.Second).Format(time.RFC3339)
			ann[AnnotationReadyCandidateUID] = meta["uid"]
			switch kind {
			case "candidate-uid-missing":
				delete(ann, AnnotationReadyCandidateUID)
			case "candidate-uid-wrong":
				ann[AnnotationReadyCandidateUID] = "different-object"
			case "object-uid-missing":
				delete(meta, "uid")
			case "resourceVersion-missing":
				delete(meta, "resourceVersion")
			case "future-candidate":
				ann[AnnotationReadyCandidate] = now.Add(time.Second).Format(time.RFC3339)
			case "malformed-candidate":
				ann[AnnotationReadyCandidate] = "invalid"
			case "malformed-provisional":
				cr["spec"].(map[string]any)["shutdownTime"] = "invalid"
			}
			live, err := s.GetSandbox(context.Background(), "p1", digStr(cr, "metadata", "name"))
			if err == nil || live != nil || errors.Is(err, errStartupLifetimeExpired) || f.patches != 0 {
				t.Fatal("untrusted identity/time granted expiry or a patch", live, err, f.patches)
			}
		})
	}
}
