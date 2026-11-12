package billing

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// countingForecaster counts resolutions; gate (when set) holds each one until closed, and fn
// replaces the answer.
type countingForecaster struct {
	calls atomic.Int32
	gate  chan struct{}
	fn    func(ctx context.Context) (ForecastInput, error)
}

func (c *countingForecaster) ForecastInput(ctx context.Context, _ string, _ time.Time) (ForecastInput, error) {
	c.calls.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	if c.fn != nil {
		return c.fn(ctx)
	}
	return ForecastInput{Resources: []ForecastResource{{ResourceID: "vm1", ResourceType: "instance", ProjectID: "p1",
		Rates: []ForecastRate{hourRate("r", "0.05")}}}}, nil
}

// cachedAt returns a CachedForecaster over inner whose clock reads *clock.
func cachedAt(inner Forecaster, clock *time.Time) *CachedForecaster {
	c := NewCachedForecaster(inner, false)
	c.now = func() time.Time { return *clock }
	return c
}

// Within the TTL the input is reused (the projection still runs on the request's own time and
// bills); past it, it is resolved again.
func TestCachedForecaster_HitWithinTTL(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	inner := &countingForecaster{}
	c := cachedAt(inner, &clock)
	for i := 0; i < 5; i++ {
		if _, err := c.ForecastInput(ctx, "bp", clock); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(10 * time.Second)
	}
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("resolved %d times within the TTL, want 1", n)
	}
	clock = clock.Add(forecastCacheTTL)
	if _, err := c.ForecastInput(ctx, "bp", clock); err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("resolved %d times after the TTL, want 2", n)
	}
	// Another profile is its own entry.
	if _, err := c.ForecastInput(ctx, "bp2", clock); err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 3 {
		t.Fatalf("a second profile shared the entry: %d", n)
	}
}

// Concurrent callers for one profile share one resolution.
func TestCachedForecaster_ConcurrentCallersResolveOnce(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	inner := &countingForecaster{gate: make(chan struct{})}
	c := cachedAt(inner, &clock)
	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := c.ForecastInput(context.Background(), "bp", clock)
			if err == nil && len(in.Resources) != 1 {
				err = errors.New("caller got an empty input")
			}
			errs <- err
		}()
	}
	// Let every caller reach the flight before releasing the one resolution.
	deadline := time.Now().Add(2 * time.Second)
	for inner.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(inner.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("%d concurrent callers resolved %d times, want 1", callers, n)
	}
}

// A resolution past the timeout falls back to "unavailable" (current cost), and is not cached.
func TestCachedForecaster_TimeoutFallsBack(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	inner := &countingForecaster{fn: func(ctx context.Context) (ForecastInput, error) {
		<-ctx.Done() // a resolution slower than the bound
		return ForecastInput{}, ctx.Err()
	}}
	c := cachedAt(inner, &clock)
	c.timeout = 20 * time.Millisecond
	// The caller's own context being canceled does not cancel the shared resolution early, but the
	// bound does.
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	f := MonthEndForecast(reqCtx, c, "bp", nil, clock)
	if time.Since(start) > time.Second {
		t.Fatalf("timeout did not bound the resolution")
	}
	assertUnavailable(t, f)
	MonthEndForecast(context.Background(), c, "bp", nil, clock)
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("a failed resolution was cached: %d resolutions", n)
	}
}

// A resolver that finishes after its deadline without noticing is not trusted (possibly partial).
func TestCachedForecaster_LateSuccessIsNotUsed(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	inner := &countingForecaster{fn: func(ctx context.Context) (ForecastInput, error) {
		<-ctx.Done()
		return ForecastInput{Resources: []ForecastResource{{ResourceID: "partial"}}}, nil
	}}
	c := cachedAt(inner, &clock)
	c.timeout = 10 * time.Millisecond
	assertUnavailable(t, MonthEndForecast(context.Background(), c, "bp", nil, clock))
}

// Over the resource cap the forecast is unavailable (current cost), never an error.
func TestCachedForecaster_CapFallsBack(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	inner := &countingForecaster{fn: func(context.Context) (ForecastInput, error) {
		return ForecastInput{}, ErrForecastTooLarge
	}}
	assertUnavailable(t, MonthEndForecast(context.Background(), cachedAt(inner, &clock), "bp", nil, clock))
}

// A panicking resolver never turns a cost-info read into a 500: with or without the cache wrapper,
// the forecast is unavailable.
func TestMonthEndForecast_PanicFallsBack(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	boom := &countingForecaster{fn: func(context.Context) (ForecastInput, error) { panic("boom") }}
	assertUnavailable(t, MonthEndForecast(context.Background(), boom, "bp", nil, clock))
	assertUnavailable(t, MonthEndForecast(context.Background(), cachedAt(boom, &clock), "bp", nil, clock))
}

// The cache is keyed by billing month: the first read of a new month resolves again, even inside
// the TTL.
func TestCachedForecaster_MonthRolloverInvalidates(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 11, 30, 23, 59, 50, 0, time.UTC)
	inner := &countingForecaster{}
	c := cachedAt(inner, &clock)
	if _, err := c.ForecastInput(ctx, "bp", clock); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(20 * time.Second) // 2026-12-01T00:00:10Z, within the TTL
	if _, err := c.ForecastInput(ctx, "bp", clock); err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("a new billing month reused last month's input: %d resolutions", n)
	}
}

// The projection runs on each request's clock, so a cached input still moves with time.
func TestCachedForecaster_ProjectionUsesRequestTime(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 11, 15, 12, 30, 0, 0, time.UTC)
	c := cachedAt(&countingForecaster{}, &clock)
	vm := pricing.BillItem{ProjectID: "p1", ResourceID: "vm1", ResourceType: "instance", NetAmount: mustDec("17.40"),
		TimeUnits: &pricing.BillItemTimeUnits{Minute: i64(0), Hour: i64(348), Month: i64(0)}}
	at := func(last time.Time) []pricing.Bill {
		v := vm
		tu := *vm.TimeUnits
		tu.HourLastRateTime = &last
		v.TimeUnits = &tu
		return []pricing.Bill{openBill(v)}
	}
	first := MonthEndForecast(ctx, c, "bp", at(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)), clock)
	clock = clock.Add(40 * time.Second)
	// The 13:00 pass ran meanwhile (fresh bills): one hour fewer left, same cached input.
	second := MonthEndForecast(ctx, c, "bp", at(time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)), clock)
	if d := first.Projected.Sub(second.Projected); !d.Equal(decimal.RequireFromString("0.05")) {
		t.Fatalf("projection %s → %s, want one hour (0.05) less", first.Projected, second.Projected)
	}
}

// Under the external billing provider every forecast is flagged.
func TestCachedForecaster_ExternalBillingIsApproximate(t *testing.T) {
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c := NewCachedForecaster(&countingForecaster{}, true)
	c.now = func() time.Time { return clock }
	ci := CostInfoMap(decimal.Zero, decimal.Zero, map[string]any{}, map[string]any{}, []any{})
	ApplyForecast(ci, MonthEndForecast(context.Background(), c, "bp", nil, clock), "")
	if r := ci["forecastApproximateReasons"].([]string); len(r) != 1 || r[0] != ForecastExternalBilling {
		t.Fatalf("reasons = %v, want [external_billing]", r)
	}
}

func assertUnavailable(t *testing.T, f *Forecast) {
	t.Helper()
	if f == nil {
		t.Fatal("nil forecast")
	}
	if !f.Projected.IsZero() {
		t.Errorf("an unavailable forecast projected %s", f.Projected)
	}
	if r := f.ApproximateReasons(""); len(r) != 1 || r[0] != ForecastUnavailable {
		t.Errorf("reasons = %v, want [unavailable]", r)
	}
}
