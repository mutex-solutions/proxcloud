//go:build integration

package integration

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/mutex-solutions/proxcloud/internal/platform/mail"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func mailKeys(t *testing.T) *textcrypt.BlobKeys {
	t.Helper()
	k, err := textcrypt.NewBlobKeys("v1", map[string]string{"v1": strings.Repeat("test-only-", 4)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func outboxFixture(t *testing.T) (*pgdoc.DB, *mailoutbox.Store) {
	t.Helper()
	db := freshPG(t)
	s := mailoutbox.New(db, mailKeys(t))
	if err := s.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db, s
}
func mailReminder(key string) mailoutbox.Reminder {
	return mailoutbox.Reminder{ContractID: key, ExpiryGeneration: "end_v1", WindowDays: 7, EndDate: time.Now().UTC().Add(time.Hour).Truncate(time.Second), Date: time.Now().UTC().Truncate(time.Second), Recipients: []mailoutbox.Recipient{{Key: "recipient_opaque", Address: "one@example.invalid"}}, From: "sender@example.invalid", Subject: "private subject", TextBody: "private body"}
}
func enqueueMail(t *testing.T, db *pgdoc.DB, s *mailoutbox.Store, r mailoutbox.Reminder) string {
	t.Helper()
	var q mailoutbox.QueueResult
	err := db.WithTx(t.Context(), func(ctx context.Context) error { var err error; q, err = s.EnqueueTx(ctx, r); return err })
	if err != nil {
		t.Fatal(err)
	}
	return q.IDs[0]
}
func claimMail(t *testing.T, s *mailoutbox.Store) *mailoutbox.Claim {
	t.Helper()
	c, err := s.Claim(t.Context(), time.Minute)
	if err != nil || c == nil {
		t.Fatalf("claim=%+v err=%v", c, err)
	}
	return c
}
func expireMailLease(t *testing.T, db *pgdoc.DB, id string) {
	t.Helper()
	if _, err := db.Pool.Exec(t.Context(), `UPDATE mail_outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}
func wantMailState(t *testing.T, s *mailoutbox.Store, id, want string) {
	t.Helper()
	v, err := s.GetStatus(t.Context(), id)
	if err != nil || v.State != want {
		t.Fatalf("status=%+v err=%v want=%s", v, err, want)
	}
}
func TestMailOutboxSameTransactionRollbackInvisibilityAndDedup(t *testing.T) {
	db, s := outboxFixture(t)
	ctx := t.Context()
	r := mailReminder("rollback")
	if _, err := s.EnqueueTx(ctx, r); err == nil {
		t.Fatal("standalone enqueue allowed")
	}
	if err := db.C("mailBusiness").Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("rollback")
	var id string
	write := func(tc context.Context) error {
		if err := db.C("mailBusiness").Upsert(tc, "decision", pgdoc.M{"queued": true}); err != nil {
			return err
		}
		q, err := s.EnqueueTx(tc, r)
		if err == nil {
			id = q.IDs[0]
		}
		return err
	}
	if err := db.WithTx(ctx, func(tc context.Context) error {
		if err := write(tc); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_outbox`).Scan(&n); err != nil || n != 0 {
		t.Fatal("rolled back mail visible")
	}
	if n, _ := db.C("mailBusiness").Count(ctx, nil); n != 0 {
		t.Fatal("business committed")
	}
	if err := db.WithTx(ctx, func(tc context.Context) error {
		if err := write(tc); err != nil {
			return err
		}
		c, err := s.Claim(context.Background(), time.Minute)
		if err != nil || c != nil {
			t.Fatal("uncommitted mail visible")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original := id
	if again := enqueueMail(t, db, s, r); again != original {
		t.Fatal("dedup changed identity")
	}
	changed := r
	changed.TextBody = "changed"
	if err := db.WithTx(ctx, func(tc context.Context) error { _, err := s.EnqueueTx(tc, changed); return err }); !errors.Is(err, mailoutbox.ErrConflict) {
		t.Fatalf("content conflict=%v", err)
	}
	changed = r
	changed.Recipients = append(append([]mailoutbox.Recipient(nil), r.Recipients...), mailoutbox.Recipient{Key: "recipient_2", Address: "two@example.invalid"})
	if err := db.WithTx(ctx, func(tc context.Context) error { _, err := s.EnqueueTx(tc, changed); return err }); !errors.Is(err, mailoutbox.ErrConflict) {
		t.Fatal("recipient manifest changed")
	}
	rotated, _ := textcrypt.NewBlobKeys("v2", map[string]string{"v1": strings.Repeat("test-only-", 4), "v2": strings.Repeat("new-test-", 4)})
	if again := enqueueMail(t, db, mailoutbox.New(db, rotated), r); again != original {
		t.Fatal("rotation broke dedup")
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE mail_outbox SET sealed='changed' WHERE id=$1`, id); err == nil {
		t.Fatal("immutable payload update accepted")
	}
}
func TestMailOutboxRejectsEveryWrongDatabaseBoundary(t *testing.T) {
	a, err := pgdoc.Connect(t.Context(), pgBase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	b, s := outboxFixture(t)
	r := mailReminder("wrongdb")
	enqueueMail(t, b, s, r)
	c := claimMail(t, s)
	_, err = s.BeginDispatch(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.WithTx(t.Context(), func(tc context.Context) error {
		checks := []func() error{
			func() error { _, e := s.EnqueueTx(tc, r); return e }, func() error { return s.Ensure(tc) },
			func() error { _, e := s.GetStatus(tc, c.ID); return e }, func() error { _, e := s.Claim(tc, time.Minute); return e },
			func() error { return s.Renew(tc, c, time.Minute) }, func() error { _, e := s.BeginDispatch(tc, c); return e },
			func() error { return s.BlockClaim(tc, c, mailoutbox.ClassConfig) },
			func() error {
				return s.RecordOutcome(tc, c, mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK})
			},
		}
		for i, f := range checks {
			if err := f(); err == nil {
				return fmt.Errorf("crossDB boundary %d accepted", i)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantMailState(t, s, c.ID, mailoutbox.Dispatching)
	if err = b.WithTx(t.Context(), func(tc context.Context) error {
		_, e := s.Claim(tc, time.Minute)
		if e == nil {
			return errors.New("worker joined business transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestMailOutboxTwoWorkerRaceCrashAndLateReceipt(t *testing.T) {
	db, s := outboxFixture(t)
	for i := 0; i < 12; i++ {
		enqueueMail(t, db, s, mailReminder(fmt.Sprint("race", i)))
	}
	var wg sync.WaitGroup
	seen := sync.Map{}
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			for {
				c, err := s.Claim(t.Context(), time.Minute)
				if err != nil {
					errs <- err
					return
				}
				if c == nil {
					return
				}
				if _, dup := seen.LoadOrStore(c.ID, true); dup {
					errs <- errors.New("claimed twice")
					return
				}
				if _, err = s.BeginDispatch(t.Context(), c); err != nil {
					errs <- err
					return
				}
				if err = s.RecordOutcome(t.Context(), c, mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	n := 0
	seen.Range(func(_, _ any) bool { n++; return true })
	if n != 12 {
		t.Fatal(n)
	}
	id := enqueueMail(t, db, s, mailReminder("crash"))
	old := claimMail(t, s)
	expireMailLease(t, db, id)
	if err := s.Renew(t.Context(), old, time.Minute); !errors.Is(err, mailoutbox.ErrLeaseLost) {
		t.Fatal("expired owner renewed")
	}
	fresh := claimMail(t, s)
	if fresh.Owner == old.Owner || fresh.Epoch <= old.Epoch {
		t.Fatal("owner not fenced")
	}
	if _, err := s.BeginDispatch(t.Context(), old); !errors.Is(err, mailoutbox.ErrLeaseLost) {
		t.Fatal("old claim dispatched")
	}
	if _, err := s.BeginDispatch(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	expireMailLease(t, db, id)
	for range 2 {
		if c, err := s.Claim(t.Context(), time.Minute); err != nil || c != nil {
			t.Fatal("dispatch intent redispatched")
		}
	}
	wantMailState(t, s, id, mailoutbox.Unknown)
	r := mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK}
	if err := s.RecordOutcome(t.Context(), fresh, r); err != nil {
		t.Fatal("late SAME-attempt receipt rejected", err)
	}
	wantMailState(t, s, id, mailoutbox.Accepted)
	if err := s.RecordOutcome(t.Context(), fresh, r); err != nil {
		t.Fatal("idempotent ACK", err)
	}
	var phases int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM mail_outbox_attempt WHERE outbox_id=$1`, id).Scan(&phases); err != nil || phases != 3 {
		t.Fatal("journal missing intent/unknown/receipt", phases, err)
	}
	if _, err := db.Pool.Exec(t.Context(), `DELETE FROM mail_outbox_attempt WHERE outbox_id=$1`, id); err == nil {
		t.Fatal("attempt journal mutable")
	}
}

// Wait on observed PostgreSQL lock state, never a guessed wall-clock sleep.
func waitMailDB(t *testing.T, db *pgdoc.DB, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var ready bool
		if err := db.Pool.QueryRow(ctx, query, args...).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		runtime.Gosched()
	}
}
func TestMailOutboxIntentJournalWaitFinalTTLGuard(t *testing.T) {
	db, s := outboxFixture(t)
	id := enqueueMail(t, db, s, mailReminder("finalttl"))
	c, err := s.Claim(t.Context(), time.Second)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	blocker, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(t.Context(), `LOCK TABLE mail_outbox_attempt IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		e, err := s.BeginDispatch(t.Context(), c)
		if err == nil && len(e.Wire()) == 0 {
			err = errors.New("missing envelope")
		}
		done <- err
	}()
	waitMailDB(t, db, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO mail_outbox_attempt%')`)
	waitMailDB(t, db, `SELECT lease_until<=clock_timestamp() FROM mail_outbox WHERE id=$1`, id)
	if err = blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, mailoutbox.ErrLeaseLost) {
		t.Fatalf("expired post-journal T2=%v", err)
	}
	wantMailState(t, s, id, mailoutbox.Claimed)
	var n int
	if err = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM mail_outbox_attempt WHERE outbox_id=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatal("expired T2 committed intent")
	}
}

func TestMailOutboxKnownUnsentRetryRejectsOlderAttempt(t *testing.T) {
	db, s := outboxFixture(t)
	id := enqueueMail(t, db, s, mailReminder("retry"))
	old := claimMail(t, s)
	if _, err := s.BeginDispatch(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOutcome(t.Context(), old, mailoutbox.Receipt{Delivery: mailoutbox.NotAccepted, Stage: mailoutbox.StageEnvelope, Class: mailoutbox.ClassTemporary}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `UPDATE mail_outbox SET next_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	fresh := claimMail(t, s)
	if _, err := s.BeginDispatch(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOutcome(t.Context(), old, mailoutbox.Receipt{Delivery: mailoutbox.RelayAccepted, Stage: mailoutbox.StageReply, Class: mailoutbox.ClassOK}); !errors.Is(err, mailoutbox.ErrReceipt) {
		t.Fatal("old-attempt ACK changed new attempt")
	}
	if err := s.RecordOutcome(t.Context(), fresh, mailoutbox.Receipt{Delivery: mailoutbox.NotAccepted, Stage: mailoutbox.StageEnvelope, Class: mailoutbox.ClassPermanent, Permanent: true}); err != nil {
		t.Fatal(err)
	}
	wantMailState(t, s, id, mailoutbox.Dead)
}

// A real old SMTP socket remains alive while a second worker sees its expired
// intent. Only one EOD can occur and the late SAME attempt can record DATA 250.
func TestMailOutboxOldLiveSMTPSocketNeverRedispatched(t *testing.T) {
	db, s := outboxFixture(t)
	id := enqueueMail(t, db, s, mailReminder("oldsocket"))
	c := claimMail(t, s)
	e, err := s.BeginDispatch(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	eodSeen := make(chan struct{})
	release := make(chan struct{})
	done := make(chan []string, 1)
	go func() {
		lines := []string{}
		defer func() { done <- lines }()
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake.invalid\r\n")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			lines = append(lines, line)
			if inData {
				if line == "." {
					close(eodSeen)
					<-release
					fmt.Fprint(conn, "250 accepted\r\n")
					inData = false
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250 fake.invalid\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				fmt.Fprint(conn, "250 ok\r\n")
			case line == "DATA":
				fmt.Fprint(conn, "354 go\r\n")
				inData = true
			case line == "QUIT":
				return
			}
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	adapter := mail.NewLoopbackSMTPForTest(func(context.Context) (mail.Config, error) {
		return mail.Config{Host: host, Port: port, From: "sender@example.invalid"}, nil
	})
	receipt := make(chan mailoutbox.Receipt, 1)
	go func() { receipt <- adapter.Deliver(t.Context(), e) }()
	<-eodSeen
	expireMailLease(t, db, id)
	for range 3 {
		if next, err := s.Claim(t.Context(), time.Minute); err != nil || next != nil {
			t.Fatal("old live socket redispatched")
		}
	}
	wantMailState(t, s, id, mailoutbox.Unknown)
	close(release)
	ack := <-receipt
	lines := <-done
	if ack.Delivery != mailoutbox.RelayAccepted {
		t.Fatal("late 250 lost", ack)
	}
	if err = s.RecordOutcome(t.Context(), c, ack); err != nil {
		t.Fatal(err)
	}
	wantMailState(t, s, id, mailoutbox.Accepted)
	eods := 0
	rcpts := 0
	for _, line := range lines {
		if line == "." {
			eods++
		}
		if strings.HasPrefix(line, "RCPT") {
			rcpts++
		}
	}
	if eods != 1 || rcpts != 1 {
		t.Fatal("SMTP transcript duplicated")
	}
}

// The local relay really consumes EOD and returns 250. Only T3 is faulted. A
// transaction lock attempt from inside Deliver also proves no SQL TX is held.
func TestMailOutboxAcceptedSMTPDatabaseACKFailureHoldsUnknown(t *testing.T) {
	db, s := outboxFixture(t)
	id := enqueueMail(t, db, s, mailReminder("ackfault"))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan int, 1)
	go func() {
		eods := 0
		defer func() { done <- eods }()
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake.invalid\r\n")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if inData {
				if line == "." {
					eods++
					fmt.Fprint(conn, "250 accepted\r\n")
					inData = false
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				fmt.Fprint(conn, "250 fake.invalid\r\n")
			case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
				fmt.Fprint(conn, "250 ok\r\n")
			case line == "DATA":
				fmt.Fprint(conn, "354 go\r\n")
				inData = true
			case line == "QUIT":
				return
			}
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	smtp := mail.NewLoopbackSMTPForTest(func(context.Context) (mail.Config, error) {
		return mail.Config{Host: host, Port: port, From: "sender@example.invalid"}, nil
	})
	fake := &mailACKFault{t: t, db: db, id: id, inner: smtp}
	d := mailoutbox.NewDispatcher(s)
	if err = d.StartWithFakeTransport(fake); err != nil {
		t.Fatal(err)
	}
	if _, err = d.RunOnce(t.Context()); err == nil {
		t.Fatal("ACK fault ignored")
	}
	if n := <-done; n != 1 {
		t.Fatal("relay did not consume exactly one EOD")
	}
	wantMailState(t, s, id, mailoutbox.Dispatching)
	if _, err = db.Pool.Exec(t.Context(), `DROP TRIGGER mail_outbox_test_ack_fail ON mail_outbox_attempt; DROP FUNCTION mail_outbox_test_ack_fail()`); err != nil {
		t.Fatal(err)
	}
	expireMailLease(t, db, id)
	if c, err := s.Claim(t.Context(), time.Minute); err != nil || c != nil {
		t.Fatal("ACK failure redispatched")
	}
	wantMailState(t, s, id, mailoutbox.Unknown)
}

type mailACKFault struct {
	t     *testing.T
	db    *pgdoc.DB
	id    string
	inner *mail.DurableSMTP
}

func (f *mailACKFault) LoopbackTestOnly() bool { return true }
func (f *mailACKFault) Deliver(ctx context.Context, e mailoutbox.Envelope) mailoutbox.Receipt {
	// NOWAIT must acquire the row in an independent transaction during I/O.
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM mail_outbox WHERE id=$1 FOR UPDATE NOWAIT`, f.id); err != nil {
		f.t.Fatal("SQL tx held across SMTP", err)
	}
	_ = tx.Rollback(ctx)
	r := f.inner.Deliver(ctx, e)
	if r.Delivery != mailoutbox.RelayAccepted {
		f.t.Fatal(r)
	}
	_, err = f.db.Pool.Exec(ctx, `CREATE FUNCTION mail_outbox_test_ack_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.phase='receipt' THEN RAISE EXCEPTION 'injected ACK failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER mail_outbox_test_ack_fail BEFORE INSERT ON mail_outbox_attempt FOR EACH ROW EXECUTE FUNCTION mail_outbox_test_ack_fail()`)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestMailOutboxRetryBoundAndCorruptCiphertextFailClosed(t *testing.T) {
	db, s := outboxFixture(t)
	id := enqueueMail(t, db, s, mailReminder("bounded"))
	for i := 1; i <= mailoutbox.MaxAttempts; i++ {
		c := claimMail(t, s)
		if _, err := s.BeginDispatch(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		if c.Attempts != i {
			t.Fatal("attempt count", c.Attempts)
		}
		if err := s.RecordOutcome(t.Context(), c, mailoutbox.Receipt{Delivery: mailoutbox.NotAccepted, Stage: mailoutbox.StageEnvelope, Class: mailoutbox.ClassTemporary}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(t.Context(), `UPDATE mail_outbox SET next_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	wantMailState(t, s, id, mailoutbox.Dead)
	if c, err := s.Claim(t.Context(), time.Minute); err != nil || c != nil {
		t.Fatal("attempt bound bypassed")
	}
	id = enqueueMail(t, db, s, mailReminder("corrupt"))
	c := claimMail(t, s)
	// A privileged fault injection simulates on-disk corruption. Normal writes
	// are trigger-blocked; never disable that trigger outside this fixture.
	_, err := db.Pool.Exec(t.Context(), `ALTER TABLE mail_outbox DISABLE TRIGGER mail_outbox_immutable_payload`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Pool.Exec(t.Context(), `UPDATE mail_outbox SET sealed=set_byte(sealed,20,get_byte(sealed,20)#1) WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(t.Context(), `ALTER TABLE mail_outbox ENABLE TRIGGER mail_outbox_immutable_payload`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginDispatch(t.Context(), c); !errors.Is(err, mailoutbox.ErrPayload) {
		t.Fatal("corrupt ciphertext sent")
	}
	wantMailState(t, s, id, mailoutbox.Claimed)
}

func TestMailOutboxMissingKeyFailClosedAndExpiry(t *testing.T) {
	db, s := outboxFixture(t)
	id := enqueueMail(t, db, s, mailReminder("missingkey"))
	c := claimMail(t, s)
	other, _ := textcrypt.NewBlobKeys("v2", map[string]string{"v2": strings.Repeat("another-", 4)})
	if _, err := mailoutbox.New(db, other).BeginDispatch(t.Context(), c); !errors.Is(err, mailoutbox.ErrPayload) {
		t.Fatal("missing key did not fail closed")
	}
	if err := s.BlockClaim(t.Context(), c, mailoutbox.ClassPayload); err != nil {
		t.Fatal(err)
	}
	wantMailState(t, s, id, mailoutbox.Blocked)
	r := mailReminder("expired")
	r.EndDate = time.Now().Add(-time.Hour)
	id = enqueueMail(t, db, s, r)
	wantMailState(t, s, id, mailoutbox.Expired)
	if c, err := s.Claim(t.Context(), time.Minute); err != nil || c != nil {
		t.Fatal("expired or blocked mail dispatched")
	}
	r = mailReminder("empty")
	r.Recipients = nil
	id = enqueueMail(t, db, s, r)
	wantMailState(t, s, id, mailoutbox.NoRecipient)
	r = mailReminder("suppressed")
	r.Disposition = mailoutbox.Suppressed
	id = enqueueMail(t, db, s, r)
	wantMailState(t, s, id, mailoutbox.Suppressed)
}
