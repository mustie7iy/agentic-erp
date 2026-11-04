package cash_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// failingTreasury is the integration port, down.
type failingTreasury struct{ cash.TreasuryChannels }

func (failingTreasury) ForCompany(context.Context, string) ([]cash.TreasuryChannel, error) {
	return nil, errors.New("connectors down")
}

func (failingTreasury) Channel(context.Context, uuid.UUID) (cash.TreasuryChannel, bool, error) {
	return cash.TreasuryChannel{}, false, errors.New("connectors down")
}

// forgetfulTreasury knows no channel by its id.
type forgetfulTreasury struct{ cash.TreasuryChannels }

func (forgetfulTreasury) Channel(context.Context, uuid.UUID) (cash.TreasuryChannel, bool, error) {
	return cash.TreasuryChannel{}, false, nil
}

// refusedTreasury refuses every channel's endpoint, as a disabled one is.
type refusedTreasury struct{ cash.TreasuryChannels }

func (refusedTreasury) Endpoint(context.Context, uuid.UUID) (sftpclient.Endpoint, error) {
	return sftpclient.Endpoint{}, problem.Conflict("integration.connector_disabled", "The treasury channel is disabled.")
}

// failingAccounts is the payments port, unable to name house banks' accounts.
type failingAccounts struct{ cash.Payments }

func (failingAccounts) HouseBankAccounts(context.Context, []uuid.UUID) (map[uuid.UUID]cash.BankAccount, error) {
	return nil, errors.New("accounts down")
}

// TestCashExportFailures: queueing, asking for, reading and listing
// exports fail with the database and with their ports.
func TestCashExportFailures(t *testing.T) {
	e := setupExport(t)
	sid := uuid.MustParse(e.take(t, "Board")["id"].(string))
	xid := uuid.MustParse(e.exports(t, sid.String())[0]["id"].(string))
	endpoints := func() *cash.Endpoints {
		c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
		c.Consistent = inRequest{}
		return c
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"take, queueing exports": func(ctx context.Context) error { _, err := endpoints().TakeSnapshot(ctx, e.le, "Again"); return err },
		"ask":                    func(ctx context.Context) error { _, err := endpoints().RequestExport(ctx, sid, e.jsonl); return err },
		"read":                   func(ctx context.Context) error { _, err := endpoints().Export(ctx, xid); return err },
		"list": func(ctx context.Context) error {
			_, _, err := endpoints().Exports(ctx, cash.ExportsQuery{Snapshot: &sid})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want it refused")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	for name, c := range map[string]struct {
		change func(*cash.Endpoints)
		fn     func(*cash.Endpoints, context.Context) error
		want   string
	}{
		"the channels down for a snapshot": {func(c *cash.Endpoints) { c.Treasury = failingTreasury{c.Treasury} },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.TakeSnapshot(ctx, e.le, "Board")
				return err
			}, "connectors down"},
		"the channels down for an export": {func(c *cash.Endpoints) { c.Treasury = failingTreasury{c.Treasury} },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.RequestExport(ctx, sid, e.jsonl)
				return err
			}, "connectors down"},
		"the channels down for a read": {func(c *cash.Endpoints) { c.Treasury = failingTreasury{c.Treasury} },
			func(c *cash.Endpoints, ctx context.Context) error { _, err := c.Export(ctx, xid); return err }, "connectors down"},
		"a channel not held": {func(c *cash.Endpoints) { c.Treasury = forgetfulTreasury{c.Treasury} },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, _, err := c.Exports(ctx, cash.ExportsQuery{})
				return err
			}, "not held by the integration context"},
		"the directory down for a read": {func(c *cash.Endpoints) { c.LegalEntities = failingEntities{} },
			func(c *cash.Endpoints, ctx context.Context) error { _, err := c.Export(ctx, xid); return err }, "directory down"},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := endpoints()
			c.change(c2)
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return c.fn(c2, ctx) })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
	e.round(t, 2) // the snapshot's; what the failing runs wrote was rolled back
}

// TestExporterFailures: a round whose begin or listing fails reports it;
// each statement of the exporter's transactions failing, and each commit,
// is reported or becomes a failed attempt, and the export is delivered
// once nothing fails; a file whose house banks cannot be named, or whose
// legal entity the directory does not hold, is a failed attempt; a claim
// that lapsed on the last attempt is made again once, and fails the
// export when it lapses again; a claim taken over, at a later attempt or
// at the same one remade, records nothing; Run logs failed rounds and
// stops with its context.
func TestExporterFailures(t *testing.T) {
	e := setupExport(t)
	sid := e.take(t, "Board")["id"].(string)
	pgtest.Exec(t, `UPDATE cash.export SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE snapshot_id = $1 AND connector_id = $2`, sid, e.csv)
	x := byChannel(t, e.exports(t, sid), e.jsonl)["id"].(string)
	reset := func() {
		pgtest.Exec(t, `UPDATE cash.export SET status = 'pending', attempts = 0, next_attempt_at = $2, file_name = NULL, sha256 = NULL, resend = NULL,
			delivered_at = NULL, last_error = NULL, version = version + 1 WHERE id = $1`, x, readAt)
	}
	pool := pgtest.Pool(t)
	run := func(change func(*cash.Endpoints), wrap func(pgx.Tx) pgx.Tx) error {
		reset()
		w := e.exporter()
		change(w.Endpoints)
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil || s.TenantID == uuid.Nil {
				return tx, err
			}
			return wrap(tx), nil
		}
		_, err := w.RunOnce(context.Background())
		return err
	}
	same := func(*cash.Endpoints) {}
	for k := 1; k <= 2; k++ {
		begun := 0
		err := run(same, func(tx pgx.Tx) pgx.Tx {
			if begun++; begun == k {
				return &pgtest.FailingTx{Tx: tx, FailCommit: true}
			}
			return tx
		})
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("commit of transaction %d: got %v, want the injected failure", k, err)
		}
	}
	// Each statement failing in turn, until a round that fails none ends
	// the export as wanted: delivered, or, with a port that fails the
	// file, a failed attempt queued, or, with the channel refused, failed.
	for name, c := range map[string]struct {
		change func(*cash.Endpoints)
		status string
	}{
		"delivered":         {same, cash.ExportDelivered},
		"a failed attempt":  {func(c *cash.Endpoints) { c.Payments = failingAccounts{c.Payments} }, cash.ExportPending},
		"a channel refused": {func(c *cash.Endpoints) { c.Treasury = refusedTreasury{c.Treasury} }, cash.ExportFailed},
	} {
		t.Run("every statement, "+name, func(t *testing.T) {
			done := false
			for failAt := 1; failAt <= 200 && !done; failAt++ {
				count := 0
				err := run(c.change, func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: failAt, Count: &count} })
				if err != nil && !errors.Is(err, pgtest.ErrInjected) {
					t.Fatalf("statement %d: got %v, want the injected failure", failAt, err)
				}
				done = err == nil && count < failAt && mustExport(t, e, x)["status"] == c.status
			}
			if !done {
				t.Fatal("still failing after 200 statements")
			}
		})
	}
	for name, change := range map[string]func(*cash.Endpoints){
		"house banks not named": func(c *cash.Endpoints) { c.Payments = failingAccounts{c.Payments} },
		"no legal entity":       func(c *cash.Endpoints) { c.LegalEntities = failingEntities{} },
		"the channels down":     func(c *cash.Endpoints) { c.Treasury = failingTreasury{c.Treasury} },
	} {
		t.Run(name, func(t *testing.T) {
			reset()
			w := e.exporter()
			change(w.Endpoints)
			if _, err := w.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := mustExport(t, e, x)
			want(t, "a failed attempt", fmt.Sprint(got["status"], " ", got["attempts"], " ", got["last_error_code"]), "pending 1 platform.internal")
		})
	}
	t.Run("the last attempt lapsed", func(t *testing.T) {
		reset()
		pgtest.Exec(t, `UPDATE cash.export SET attempts = 6, version = version + 1 WHERE id = $1`, x)
		e.round(t, 1)
		got := mustExport(t, e, x)
		want(t, "made again as the last, delivered", fmt.Sprint(got["status"], " ", got["attempts"], " ", got["last_error_code"]),
			"delivered 6 cash.export_outcome_unknown")
	})
	t.Run("the remade last attempt lapsed", func(t *testing.T) {
		reset()
		pgtest.Exec(t, `UPDATE cash.export SET attempts = 6, last_error = 'cash.export_outcome_unknown', version = version + 1 WHERE id = $1`, x)
		e.round(t, 1)
		got := mustExport(t, e, x)
		want(t, "failed, not made a third time", fmt.Sprint(got["status"], " ", got["attempts"], " ", got["last_error_code"]),
			"failed 6 cash.export_outcome_unknown")
	})
	t.Run("the last attempt lapsed, sent again and failing", func(t *testing.T) {
		reset()
		pgtest.Exec(t, `UPDATE cash.export SET attempts = 6, version = version + 1 WHERE id = $1`, x)
		e.server.FailOpen = true
		defer func() { e.server.FailOpen = false }()
		e.round(t, 1)
		got := mustExport(t, e, x)
		want(t, "failed", fmt.Sprint(got["status"], " ", got["attempts"], " ", got["last_error_code"]), "failed 6 platform.unavailable")
	})
	t.Run("a remade last attempt taking the claim over", func(t *testing.T) {
		// The claim of the sixth attempt lapses while its upload is
		// stalled, and another replica remakes it: same attempt number,
		// a later version. The stalled claimant records nothing.
		reset()
		pgtest.Exec(t, `UPDATE cash.export SET attempts = 5, version = version + 1 WHERE id = $1`, x)
		w := e.exporter()
		begun := 0
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			if s.TenantID != uuid.Nil {
				if begun++; begun == 2 {
					pgtest.Exec(t, `UPDATE cash.export SET last_error = 'cash.export_outcome_unknown', version = version + 1 WHERE id = $1`, x)
				}
			}
			return pool.Begin(ctx, s)
		}
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := mustExport(t, e, x)
		want(t, "its outcome left to the remade claim", fmt.Sprint(got["status"], " ", got["attempts"], " ", got["last_error_code"]),
			"pending 6 cash.export_outcome_unknown")
	})
	t.Run("a claim taken over", func(t *testing.T) {
		reset()
		w := e.exporter()
		begun := 0
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			if s.TenantID != uuid.Nil {
				if begun++; begun == 2 { // another replica claimed it again meanwhile
					pgtest.Exec(t, `UPDATE cash.export SET attempts = attempts + 1, version = version + 1 WHERE id = $1`, x)
				}
			}
			return pool.Begin(ctx, s)
		}
		if _, err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := mustExport(t, e, x)
		want(t, "its outcome left to the other claim", fmt.Sprint(got["status"], " ", got["attempts"]), "pending 2")
	})
	pgtest.Exec(t, `UPDATE cash.export SET status = 'failed', next_attempt_at = NULL, version = version + 1 WHERE id = $1`, x)
	for name, begin := range map[string]database.BeginFunc{
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := e.exporter()
			w.Begin = begin
			if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("got %v, want the injected failure", err)
			}
		})
	}
	var logs pgtest.SyncBuffer
	failing := e.exporter()
	failing.WorkerRole, failing.Interval, failing.Log = "no_such_role", time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { failing.Run(ctx); close(done) }()
	for deadline := time.Now().Add(time.Minute); !strings.Contains(logs.String(), "cash export failed"); runtime.Gosched() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("no failed round logged within a minute")
		}
	}
	cancel()
	<-done
}

// TestExporterUploadBound: an upload that does not end within its bound
// is a failed attempt, logged.
func TestExporterUploadBound(t *testing.T) {
	e := setupExport(t)
	e.server.Silent = true
	sid := e.take(t, "Board")["id"].(string)
	var logs pgtest.SyncBuffer
	w := e.exporter()
	w.Upload, w.Log = 50*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	x := byChannel(t, e.exports(t, sid), e.jsonl)
	want(t, "a failed attempt", fmt.Sprint(x["status"], " ", x["attempts"], " ", x["last_error_code"]), "pending 1 platform.unavailable")
	if !strings.Contains(logs.String(), "cash export attempt failed") {
		t.Errorf("log: got %q, want the attempt reported", logs.String())
	}
}
