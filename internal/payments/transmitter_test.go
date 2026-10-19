package payments_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// fileState reads a bank file's transmission state.
func fileState(t *testing.T, file string) (status string, attempts int, code string) {
	t.Helper()
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status, attempts, coalesce(last_error, '') FROM payments.bank_file WHERE id = $1`,
		file).Scan(&status, &attempts, &code); err != nil {
		t.Fatal(err)
	}
	return status, attempts, code
}

// due reads when a queued file is next due.
func due(t *testing.T, file string) time.Time {
	t.Helper()
	var at time.Time
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT next_attempt_at FROM payments.bank_file WHERE id = $1`, file).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// version is the file's ETag, for the next request on it.
func version(t *testing.T, e env, file string) string {
	t.Helper()
	_, etag := e.Must(200, "GET", "/v1/bank-files/"+file, "", "")
	return etag
}

// checkBackoff: a file the bank does not take is due again after 1, 5 and
// 15 minutes, 1 and 4 hours, not before, and fails after the sixth attempt.
func checkBackoff(t *testing.T, e env, file string) {
	t.Helper()
	tr := e.transmitter(t)
	tr.Log = slog.New(slog.NewTextHandler(&pgtest.SyncBuffer{}, nil))
	var last time.Time
	for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour} {
		if status, attempts, code := fileState(t, file); status != "to_send" || attempts != i+1 || code != "platform.unavailable" {
			t.Fatalf("after attempt %d: got %s, %d attempts, %s; want to_send, %d, platform.unavailable", i+1, status, attempts, code, i+1)
		}
		next := due(t, file)
		if i > 0 && next.Sub(last) != wait {
			t.Errorf("wait after attempt %d: got %v, want %v", i+1, next.Sub(last), wait)
		}
		for _, c := range []struct {
			at   time.Time
			want int
		}{{next.Add(-time.Second), 0}, {next, 1}} {
			tr.Now = func() time.Time { return c.at }
			if n, err := tr.RunOnce(context.Background()); err != nil || n != c.want {
				t.Fatalf("attempt %d at %v: got %d (%v), want %d", i+2, c.at, n, err, c.want)
			}
		}
		last = next
	}
}

// operationOf is the operation a file's transmission was asked for under.
func operationOf(t *testing.T, file string) string {
	t.Helper()
	var op uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT transmit_operation_id FROM payments.bank_file WHERE id = $1`, file).Scan(&op); err != nil {
		t.Fatal(err)
	}
	return op.String()
}

// opStatus reads an operation's status.
func opStatus(t *testing.T, op string) string {
	t.Helper()
	var status string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status FROM platform.operation WHERE id = $1`, op).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// asked asks for a released file's transmission without running the
// transmitter, and returns the file.
func asked(t *testing.T, e env, invoice string) string {
	t.Helper()
	file := releasedFile(t, e, invoice).String()
	e.Must(202, "POST", "/v1/bank-files/"+file+":transmit", "", `"1"`)
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE payments.bank_file SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE id = $1 AND status IN ('to_send', 'sending')`, file)
	})
	return file
}

// TestTransmitter: the transmitter sends each file once however often it
// runs (ADR-0038, T44e); the sweep queues a file whose claim lapsed again;
// a cancelled request is withdrawn before the upload, and one cancelled
// while the bank has the file keeps the file's outcome.
func TestTransmitter(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	s := transmitScenario{e: e}
	t.Run("run twice at once", s.transmitTwice)
	t.Run("a hash its bank took", s.transmitDuplicate)
	t.Run("the sweep", s.transmitSweep)
	t.Run("cancelled", s.transmitCancelled)
	t.Run("a stopping worker", func(t *testing.T) {
		// The worker is stopped while the bank takes the file: the outcome
		// is recorded all the same, and no further file is claimed.
		e := e.in(t)
		file := asked(t, e, "INV-8")
		ctx, cancel := context.WithCancel(context.Background())
		e.connector.during = cancel
		defer func() { e.connector.during = nil }()
		if _, err := e.transmitter(t).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if status, _, _ := fileState(t, file); status != "transmitted" {
			t.Errorf("file: got %s, want transmitted", status)
		}
	})
	t.Run("a server without a directory", func(t *testing.T) {
		// The bank names no connector and the server took its directory
		// away after the request: the attempt fails and is tried again.
		file := asked(t, e.in(t), "INV-7")
		tr := e.transmitter(t)
		tr.Endpoints.Connector = nil
		if _, err := tr.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if status, attempts, code := fileState(t, file); status != "to_send" || attempts != 1 || code != "platform.unavailable" {
			t.Errorf("file: got %s, %d attempts, %s; want to_send, 1, platform.unavailable", status, attempts, code)
		}
	})
}

func (s transmitScenario) transmitTwice(t *testing.T) {
	e := s.e.in(t)
	file := asked(t, e, "INV-1")
	before := len(e.connector.files)
	var wg sync.WaitGroup
	counts := make([]int, 2)
	for i := range counts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := e.transmitter(t)
			tr.Upload = time.Minute
			n, err := tr.RunOnce(context.Background())
			if err != nil {
				t.Errorf("transmitter %d: %v", i, err)
			}
			counts[i] = n
		}()
	}
	wg.Wait()
	if got := counts[0] + counts[1]; got != 1 {
		t.Errorf("files handled by both: got %d, want 1", got)
	}
	if n, err := e.transmitter(t).RunOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("third run: got %d (%v), want nothing to do", n, err)
	}
	if got := len(e.connector.files) - before; got != 1 {
		t.Errorf("uploads: got %d, want 1", got)
	}
	if status, attempts, _ := fileState(t, file); status != "transmitted" || attempts != 1 {
		t.Errorf("file: got %s after %d attempts, want transmitted after 1", status, attempts)
	}
	if got := opStatus(t, operationOf(t, file)); got != "succeeded" {
		t.Errorf("operation: got %s, want succeeded", got)
	}
	n := 0
	for _, ev := range events(t, e.Tenant, "bank_file_transmitted") {
		if ev["bank_file_id"] == file {
			n++
		}
	}
	if n != 1 {
		t.Errorf("transmitted events: got %d, want 1", n)
	}
}

func (s transmitScenario) transmitDuplicate(t *testing.T) {
	e := s.e.in(t)
	file := asked(t, e, "INV-2")
	pgtest.Exec(t, `INSERT INTO payments.sent_bank_file (tenant_id, house_bank_id, sha256, bank_file_id, sent_by)
		SELECT tenant_id, $2, sha256, id, $3 FROM payments.bank_file WHERE id = $1`, file, e.bank, e.Tenant.Human.ID)
	before := len(e.connector.files)
	if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(e.connector.files) - before; got != 0 {
		t.Errorf("uploads: got %d, want none", got)
	}
	n := 0
	for _, ev := range events(t, e.Tenant, "bank_file_transmitted") {
		if ev["bank_file_id"] != file {
			continue
		}
		n++
		if ev["duplicate"] != true {
			t.Errorf("event duplicate: got %v, want true", ev["duplicate"])
		}
	}
	if n != 1 {
		t.Errorf("transmitted events: got %d, want 1", n)
	}
	if status, _, _ := fileState(t, file); status != "transmitted" {
		t.Errorf("file: got %s, want transmitted", status)
	}
}

// transmitSweep: a file in flight whose claim lapsed is queued again and
// sent; on its last attempt it fails; a claim that lapsed while the bank
// had the file leaves the outcome to the later claim.
func (s transmitScenario) transmitSweep(t *testing.T) {
	e := s.e.in(t)
	lapse := func(file string, attempts int) {
		pgtest.Exec(t, `UPDATE payments.bank_file SET status = 'sending', attempts = $2, next_attempt_at = now() - interval '1 second', version = version + 1 WHERE id = $1`, file, attempts)
	}
	file := asked(t, e, "INV-3")
	lapse(file, 1)
	if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, attempts, _ := fileState(t, file); status != "transmitted" || attempts != 2 {
		t.Errorf("swept file: got %s after %d attempts, want transmitted after 2", status, attempts)
	}

	last := asked(t, e, "INV-4")
	lapse(last, 6)
	if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.failedWith(t, map[string]any{"id": operationOf(t, last)}, "platform.unavailable")

	taken := asked(t, e, "INV-5")
	e.connector.during = func() {
		pgtest.Exec(t, `UPDATE payments.bank_file SET attempts = attempts + 1, version = version + 1 WHERE id = $1`, taken)
	}
	defer func() { e.connector.during = nil }()
	if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, attempts, _ := fileState(t, taken); status != "sending" || attempts != 2 {
		t.Errorf("file whose claim was taken: got %s after %d attempts, want sending after 2, left to the later claim", status, attempts)
	}
}

// transmitCancelled: a request cancelled before the claim is withdrawn and
// nothing is sent; one cancelled while the bank has the file, or on its
// failing last attempt, keeps the file's outcome and the cancellation.
func (s transmitScenario) transmitCancelled(t *testing.T) {
	e := s.e.in(t)
	file := asked(t, e, "INV-6")
	e.Must(200, "POST", "/v1/operations/"+operationOf(t, file)+":cancel", "", `"1"`)
	before := len(e.connector.files)
	if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := fileState(t, file); status != "created" || len(e.connector.files) != before {
		t.Errorf("withdrawn file: got %s with %d uploads, want created with none", status, len(e.connector.files)-before)
	}
	defer func() { e.connector.during, e.connector.err = nil, nil }()
	for name, c := range map[string]struct {
		attempts int
		err      error
		want     string
	}{
		"in flight":            {0, nil, "transmitted"},
		"failing last attempt": {5, errBank, "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			file := asked(t, e, "INV-C-"+strings.ReplaceAll(name, " ", "-"))
			op := operationOf(t, file)
			pgtest.Exec(t, `UPDATE payments.bank_file SET attempts = $2, version = version + 1 WHERE id = $1`, file, c.attempts)
			e.connector.err = c.err
			e.connector.during = func() {
				pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, op)
			}
			if _, err := e.transmitter(t).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if status, _, _ := fileState(t, file); status != c.want {
				t.Errorf("file: got %s, want %s", status, c.want)
			}
			if got := opStatus(t, op); got != "cancelled" {
				t.Errorf("operation: got %s, want cancelled", got)
			}
		})
	}
}

// TestTransmitterDatabaseFailures: each statement of each path failing in
// turn, and each commit, fails the round and changes nothing, until the
// round that fails nothing completes the path.
func TestTransmitterDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	file := asked(t, e, "INV-1")
	op := operationOf(t, file)
	defer func() { e.connector.err = nil }()
	for name, c := range map[string]struct {
		status, op string
		attempts   int
		bankErr    error
		dup        bool
		want       string
		commits    int // the round's transactions that commit
	}{
		"sent":      {status: "to_send", op: "queued", want: "transmitted", commits: 2},
		"duplicate": {status: "to_send", op: "queued", dup: true, want: "transmitted", commits: 1},
		"retried":   {status: "to_send", op: "queued", bankErr: errBank, want: "to_send", commits: 2},
		"failed":    {status: "to_send", op: "queued", attempts: 5, bankErr: errBank, want: "failed", commits: 2},
		"withdrawn": {status: "to_send", op: "cancelled", want: "created", commits: 1},
		"requeued":  {status: "sending", op: "queued", attempts: 1, want: "transmitted", commits: 3},
		"lapsed":    {status: "sending", op: "queued", attempts: 6, want: "failed", commits: 1},
		"unknown":   {status: "sending", op: "cancelled", attempts: 1, want: "failed", commits: 1},
	} {
		t.Run(name, func(t *testing.T) {
			reset := func() {
				// The record is insert-only; the test takes it back with triggers off.
				pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
					DELETE FROM payments.sent_bank_file WHERE bank_file_id = '%s'; END $$`, file))
				pgtest.Exec(t, `UPDATE payments.bank_file SET status = $2, attempts = $3, next_attempt_at = now() - interval '1 second', version = version + 1,
					transmit_operation_id = $4, transmit_requested_by = created_by WHERE id = $1`, file, c.status, c.attempts, op)
				pgtest.Exec(t, `UPDATE platform.operation SET status = $2, result = NULL, error = NULL, version = version + 1 WHERE id = $1`, op, c.op)
				if c.dup {
					pgtest.Exec(t, `INSERT INTO payments.sent_bank_file (tenant_id, house_bank_id, sha256, bank_file_id, sent_by)
						SELECT tenant_id, $2, sha256, id, created_by FROM payments.bank_file WHERE id = $1`, file, e.bank)
				}
			}
			e.connector.err = c.bankErr
			everyStatement(t, e, c.commits, reset, func() bool {
				status, _, _ := fileState(t, file)
				return status == c.want
			})
		})
	}
}

// everyStatement runs the transmitter with each commit of its tenant
// transactions failing, then each of their statements in turn, resetting
// the file before each round. A failed round reports the injected failure;
// a statement whose failure becomes a failed attempt (as a problem would)
// lets the round succeed, and the loop goes on until the round that fails
// nothing reaches the state done wants.
func everyStatement(t *testing.T, e env, commits int, reset func(), done func() bool) {
	t.Helper()
	pool := pgtest.Pool(t)
	run := func(wrap func(pgx.Tx) pgx.Tx) error {
		reset()
		tr := e.transmitter(t)
		tr.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil || s.TenantID == uuid.Nil {
				return tx, err
			}
			return wrap(tx), nil
		}
		_, err := tr.RunOnce(context.Background())
		return err
	}
	for k := 1; k <= commits; k++ {
		begun := 0
		err := run(func(tx pgx.Tx) pgx.Tx {
			if begun++; begun == k {
				return &pgtest.FailingTx{Tx: tx, FailCommit: true}
			}
			return tx
		})
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("commit of transaction %d: got %v, want the injected failure", k, err)
		}
	}
	for failAt := 1; failAt <= 200; failAt++ {
		count := 0
		err := run(func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: failAt, Count: &count} })
		if err != nil && !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", failAt, err)
		}
		if err == nil && count < failAt && done() {
			return
		}
	}
	t.Fatal("still failing after 200 statements")
}

// TestTransmitterRounds: a round whose begin or listing fails reports it,
// as does one whose tenant transaction cannot begin; Run logs failed
// rounds and stops with its context.
func TestTransmitterRounds(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	asked(t, e, "INV-1")
	pool := pgtest.Pool(t)
	for name, begin := range map[string]database.BeginFunc{
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		},
		"tenant": func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			if s.TenantID != uuid.Nil {
				return nil, pgtest.ErrInjected
			}
			return pool.Begin(ctx, s)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tr := e.transmitter(t)
			tr.Begin = begin
			if _, err := tr.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("got %v, want the injected failure", err)
			}
		})
	}
	var logs pgtest.SyncBuffer
	failing := e.transmitter(t)
	failing.WorkerRole, failing.Interval, failing.Log = "no_such_role", time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { failing.Run(ctx); close(done) }()
	for deadline := time.Now().Add(time.Minute); !strings.Contains(logs.String(), "bank file transmission failed"); runtime.Gosched() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("no failed round logged within a minute")
		}
	}
	cancel()
	<-done
}

// transmitScenario is what the steps of TestTransmitter share.
type transmitScenario struct{ e env }

// TestBankFileTransmissionSchema: the schema takes only the statuses of
// the contract, and a file to send or in flight names its request and
// when it is due (000102).
func TestBankFileTransmissionSchema(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	file := asked(t, e, "INV-1")
	for name, c := range map[string]struct {
		set  string
		code string
	}{
		"an unknown status":         {"status = 'lost'", "23514"},
		"to send without a request": {"transmit_operation_id = NULL", "23514"},
		"in flight without a time":  {"status = 'sending', next_attempt_at = NULL", "23514"},
		"negative attempts":         {"attempts = -1", "23514"},
		"failed without a request":  {"status = 'failed', transmit_operation_id = NULL, next_attempt_at = NULL", ""},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := pgtest.Admin(t).Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }() // the change is never kept
			_, err = tx.Exec(context.Background(), `UPDATE payments.bank_file SET `+c.set+`, version = version + 1 WHERE id = $1`, file)
			var pe *pgconn.PgError
			switch {
			case c.code == "" && err != nil:
				t.Errorf("got %v, want the change taken", err)
			case c.code != "" && (!errors.As(err, &pe) || pe.Code != c.code):
				t.Errorf("got %v, want SQLSTATE %s", err, c.code)
			}
		})
	}
}
