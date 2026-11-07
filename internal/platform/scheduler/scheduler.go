// Package scheduler runs cron jobs guarded by a distributed lock (over the `shedLock`
// collection) so a job runs once across the fleet. robfig/cron/v3 (seconds-enabled, 6-field
// crons) is the trigger; internal/platform/lock is the cross-pod guard.
package scheduler

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
)

// Charge cron specs (6-field, seconds-first).
// NOTE: monthlyCharge uses the SAME expression as hourly (top of hour).
const (
	MinutelyChargeSpec = "30 * * * * *" // every minute at :30
	HourlyChargeSpec   = "0 0 * * * *"  // top of every hour
	MonthlyChargeSpec  = "0 0 * * * *"  // == hourly (intentional)
	UsageMetricsSpec   = "0 0 * * * *"  // top of every hour (usage metrics fetch)

	// SavingsExpirationSpec — daily at 00:00 (savings-contract expiration).
	SavingsExpirationSpec = "0 0 0 * * *"

	// SavingsExpiryRemindersSpec — daily at 00:00 (savings-contract expiry reminders):
	// SCHEDULE reminder docs for ACTIVE contracts nearing expiry.
	SavingsExpiryRemindersSpec = "0 0 0 * * *"

	// ReminderNotificationsSpec — hourly at :00 (reminder notifications): DISPATCH the due
	// reminders scheduled above (re-evaluates days-until-expiry each run → sends within the hour).
	ReminderNotificationsSpec = "0 0 * * * *"

	// TransactionScanSpec — every 20 minutes (payment-gateway transaction scanning):
	// reconcile stuck PENDING payment transactions.
	TransactionScanSpec = "0 */20 * * * *"

	// MonthlyBillSpec — daily at 00:30 (send bills): finalize each profile's previous-month
	// OPEN bill (OPEN→SENT/PAID) so it becomes collectable + dunnable. Not at 00:00: the charges at
	// 00:00 and 00:00:30 on the 1st still bill the month's last hour and minute onto that bill.
	MonthlyBillSpec = "0 30 0 * * *"

	// AutoSuspensionSpec — every 30 minutes (auto-suspension job).
	AutoSuspensionSpec = "0 */30 * * * *"

	// MonthlyCollectSpec — 07:00 on the 1st/5th/9th/13th/16th (collect job).
	MonthlyCollectSpec = "0 0 7 1,5,9,13,16 * *"

	// ServicesSyncSpec — every 15 minutes (services sync): sync every ENABLED project's cloud
	// resources.
	ServicesSyncSpec = "0 */15 * * * *"

	// PVEStateSyncSpec — every minute at :15 (Proxmox state sampler), clear of the charge at :30.
	PVEStateSyncSpec = "15 * * * * *"

	// SandboxStateSyncSpec — every minute at :10 (sandbox rows off the sandbox cluster), before the charge at :30.
	SandboxStateSyncSpec = "10 * * * * *"
	// SandboxTemplateAdoptSpec — every 5 minutes: built-in templates follow the prepull DaemonSet (imageAdoption).
	SandboxTemplateAdoptSpec = "0 */5 * * * *"

	// PVETaskPollSpec — every 20 seconds (Proxmox task log poller).
	PVETaskPollSpec = "*/20 * * * * *"

	// PVETrafficSpec — every minute at :45 (Proxmox public-NIC counters).
	PVETrafficSpec = "45 * * * * *"

	// ProjectDeletionSpec — every minute at :30 (project deletion): delete projects past their
	// deletion grace window (cascade cloud delete → remove the doc).
	ProjectDeletionSpec = "30 * * * * *"
)

// Job is a lock-guarded scheduled task.
type Job struct {
	Name       string // the distributed-lock name
	Spec       string // cron expression (seconds-first)
	AtMostFor  time.Duration
	AtLeastFor time.Duration
	Fn         func(ctx context.Context)
}

type Scheduler struct {
	c    *cron.Cron
	lock *lock.ShedLock
}

func New(l *lock.ShedLock) *Scheduler {
	return &Scheduler{c: cron.New(cron.WithSeconds()), lock: l}
}

// Register schedules a job; each fire runs under its distributed lock (skipped on a fleet-mate).
func (s *Scheduler) Register(j Job) error {
	_, err := s.c.AddFunc(j.Spec, func() {
		_, _ = s.RunLocked(context.Background(), j.Name, j.AtMostFor, j.AtLeastFor, time.Now().UTC(), j.Fn)
	})
	return err
}

func (s *Scheduler) Start() { s.c.Start() }
func (s *Scheduler) Stop()  { s.c.Stop() }

// RunLocked acquires the job's distributed lock, runs fn iff acquired, then releases honouring
// atLeastFor. Returns whether fn ran. Exposed (with an injected `now`) so the lock guard is
// deterministically testable without the cron clock.
//
// fn runs panic-guarded: a panicking job must not take down the process (robfig/cron
// does not recover, so an unguarded panic in any job crashes the whole API pod) — it is
// logged with its stack and the lock is still released.
func (s *Scheduler) RunLocked(ctx context.Context, name string, atMostFor, atLeastFor time.Duration, now time.Time, fn func(ctx context.Context)) (bool, error) {
	ok, err := s.lock.Lock(ctx, name, atMostFor, now)
	if err != nil || !ok {
		return false, err
	}
	defer func() { _ = s.lock.Unlock(ctx, name, atLeastFor, now, time.Now().UTC()) }()
	runRecovered(ctx, name, fn)
	return true, nil
}

// runRecovered runs fn, converting a panic into an error log (job name + stack).
func runRecovered(ctx context.Context, name string, fn func(ctx context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduled job panic", "job", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn(ctx)
}
