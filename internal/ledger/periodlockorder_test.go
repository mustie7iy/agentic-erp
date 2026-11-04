package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Statements that lock the periods of several ledgers take them in the
// one order postings and sign-offs take them: by fiscal year and period,
// the leading ledger first, then by ledger code (ADR-0058 §5, ADR-0008).
// The tests run on fixture.ThreeLedgers, whose ledgers' ids and period
// rows sort in neither that order nor the codes'.

// threeLedgers is a tenant whose company 2000 keeps fixture.ThreeLedgers'
// ledgers, all of 2026 open, and its periods of March 2026 by ledger code.
func threeLedgers(t *testing.T) (fixture.Env, map[string]uuid.UUID) {
	t.Helper()
	e := fixture.NewWith(t, nil, fixture.ThreeLedgers, fixture.ClosePermissions...)
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT l.code, ps.id FROM ledger.period_status ps JOIN ledger.ledger l ON l.id = ps.ledger_id
		WHERE ps.legal_entity_id = $1 AND ps.fiscal_year = 2026 AND ps.period = 3`, e.DE)
	periods := map[string]uuid.UUID{}
	var code string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&code, &id}, func() error { periods[code] = id; return nil }); err != nil {
		t.Fatalf("periods of March: %v", err)
	}
	if len(periods) != len(fixture.LockOrder) {
		t.Fatalf("periods of March: got %v, want one per ledger of %v", periods, fixture.LockOrder)
	}
	return e, periods
}

// holdShare holds the periods FOR SHARE in a transaction of its own, as
// a posting to their ledgers alone would, until release; it returns the
// holder's backend pid.
func holdShare(t *testing.T, periods []uuid.UUID) (pid int32, release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) }) // released by the test; a failed test releases it here
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	rows, _ := tx.Query(ctx, `SELECT id FROM ledger.period_status WHERE id = ANY($1) FOR SHARE`, periods)
	if got, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil || len(got) != len(periods) {
		t.Fatalf("hold the periods: got %v, %v", got, err)
	}
	return pid, func() {
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// TestControllingSwitchAndPostingLockInOneOrder turns cost center
// accounting on (which locks the company's periods from the current
// fiscal year on FOR UPDATE, readControlling) while a posting dated in
// March goes to every ledger. A third transaction holds one ledger's
// period of March FOR SHARE, so the switch stops at that period with
// every period before it in the one order locked; released, neither
// waits for the other in a cycle. Locked in the order of the rows, the
// switch held 1L's period while it waited for 0L's, which the posting
// had taken and held while it waited for 1L's: a deadlock (40P01).
func TestControllingSwitchAndPostingLockInOneOrder(t *testing.T) {
	for _, held := range fixture.LockOrder {
		t.Run(held+" held", func(t *testing.T) {
			e, periods := threeLedgers(t)
			doc := fixture.Document(t)
			fixture.ThreeLedgers(doc)
			doc.Company("2000").CostCenterAccounting = true
			holder, release := holdShare(t, []uuid.UUID{periods[held]})
			setup := ledger.Setup{LegalEntities: tenancy.Directory{}, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
			switchPID, switched := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return setup.Apply(ctx, doc) })
			switcher := <-switchPID
			apitest.WaitBlocked(t, switcher, holder, switched, "the switch")
			// A payment on account, so turning cost center accounting on
			// does not refuse the posting (no primary cost account).
			payables, bank := e.Account(t, "2000", "211000"), e.Account(t, "2000", "100000")
			day := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
			postPID, posted := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "EUR",
					SourceContext: "payables", SourceDocumentID: uuid.New(),
					Lines: []ledger.PostingLine{{AccountID: payables, Amount: 500, PartnerID: &e.Supplier}, {AccountID: bank, Amount: -500}}})
				return err
			})
			poster := <-postPID
			var postErr error
			postedFirst := false
			pgtest.Eventually(t, 10*time.Second, func() bool {
				select {
				case postErr = <-posted:
					postedFirst = true
					return true
				default:
					return apitest.Blocks(t, switcher, poster)
				}
			}, "the posting neither finished nor waited for the switch")
			release()
			if !postedFirst {
				postErr = <-posted
			}
			if postErr != nil {
				t.Errorf("posting: got %v, want it posted", postErr)
			}
			// The switch is refused once the year has a posting, so when
			// the posting committed first (ADR-0050 §4).
			switchErr := <-switched
			switch {
			case postedFirst && problem.From(switchErr).Code != "ledger.controlling_invalid":
				t.Errorf("switch after the posting: got %v, want ledger.controlling_invalid", switchErr)
			case !postedFirst && switchErr != nil:
				t.Errorf("switch before the posting: got %v, want it applied", switchErr)
			}
		})
	}
}

// TestMigratedClearingAndSignOffLockInOneOrder runs a migrated clearing
// dated in March and a sign-off's locks of March's periods (LockPeriod
// per ledger in the one order, as closing's sign-off takes them) at once.
// The clearing entry's row is moved behind its copies', as a heap with
// free space left by deleted rows can store them, and the clearing reads
// the rows in heap order (bitmapScans: a bitmap or sequential scan).
// Locking in that order, the clearing took 0A's period and waited for
// 0L's, which the sign-off held while it waited for 0A's: a deadlock
// (40P01). A third
// transaction holds the periods of the ledgers after the first k in the
// one order FOR SHARE, so the sign-off locks the first k and waits.
func TestMigratedClearingAndSignOffLockInOneOrder(t *testing.T) {
	for k := 1; k < len(fixture.LockOrder); k++ {
		t.Run(fixture.LockOrder[k]+" first held", func(t *testing.T) {
			e, periods := threeLedgers(t)
			pair := migratedPairOf(t, e)
			behindCopies(t, pair.payment)
			pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026
				AND period = 3`, e.DE)
			var held []uuid.UUID
			for _, code := range fixture.LockOrder[k:] {
				held = append(held, periods[code])
			}
			holder, release := holdShare(t, held)
			signPID, signed := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				for _, code := range fixture.LockOrder {
					if _, err := ledger.LockPeriod(ctx, periods[code], func(int32) error { return nil }); err != nil {
						return err
					}
				}
				return nil
			})
			signer := <-signPID
			apitest.WaitBlocked(t, signer, holder, signed, "the sign-off")
			clearPID, cleared := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				if err := bitmapScans(ctx); err != nil {
					return err
				}
				_, err := postings().Clear(ctx, e.DE, date("2026-03-31"), ledger.ItemRef{FiscalYear: 2026, EntryID: pair.payment, LineNo: 1}, pair.items())
				return err
			})
			apitest.WaitBlocked(t, <-clearPID, signer, cleared, "the clearing")
			release()
			if err := <-signed; err != nil {
				t.Errorf("sign-off: got %v, want it committed", err)
			}
			if err := <-cleared; problem.From(err).Code != "ledger.period_not_open" {
				t.Errorf("clearing after the sign-off: got %v, want ledger.period_not_open", err)
			}
		})
	}
}

// migratedPairOf posts a supplier invoice and its payment of March to
// every ledger of company 2000, as a migration loads them before it
// clears them.
func migratedPairOf(t *testing.T, e fixture.Env) migratedPair {
	t.Helper()
	bank, expense, ap := e.Account(t, "2000", "100000"), e.Account(t, "2000", "520000"), e.Account(t, "2000", "211000")
	sup := e.Supplier.String()
	inv, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.DE, "2026-03-01", "EUR", line{expense.String(), "10.00", ""}, line{ap.String(), "-10.00", sup}), "")
	pay, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.DE, "2026-03-20", "EUR", line{ap.String(), "10.00", sup}, line{bank.String(), "-10.00", ""}), "")
	return migratedPair{uuid.MustParse(inv["id"].(string)), uuid.MustParse(pay["id"].(string))}
}

// bitmapScans makes the request's transaction read rows through bitmap
// or sequential scans, in heap order, as the planner reads a posting
// group through its id and posting group indexes once the tenant has more
// entries than a test posts; with few, it reads the tenant's entries
// through an index whose order happens to be the entries' creation order.
func bitmapScans(ctx context.Context) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SET LOCAL enable_indexscan = off`)
	return err
}

// behindCopies stores the first entry of a posting group again, behind
// its copies in the heap (the triggers off, as pgtest.Unguarded does),
// until a scan of the group reads a copy first. Once is not always
// enough: an insert goes to a page the free space map offers, and once
// autovacuum has recorded free space in the package database's busy
// partition (the other tests' rows, under load), that page can come
// before the copies' (seen at load 25). Each move is in the one
// transaction, so no old version's space is reused, and the backend keeps
// filling the page it chose; a full page leaves the map (its true free
// space recorded), so the moves reach a page after the copies, or a new
// one, after at most as many as the free space before them holds.
func behindCopies(t *testing.T, first uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	const most = 10_000 // moves: far more than a test database's free space before a page holds
	moves := 0
	if err := pgx.BeginFunc(ctx, pgtest.Admin(t), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		for head := first; head == first; moves++ {
			if moves == most {
				return fmt.Errorf("first entry of the group still first in the heap after %d moves", most)
			}
			if _, err := tx.Exec(ctx, `WITH d AS (DELETE FROM ledger.journal_entry WHERE fiscal_year = 2026 AND id = $1 RETURNING *)
				INSERT INTO ledger.journal_entry SELECT * FROM d`, first); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT id FROM ledger.journal_entry WHERE fiscal_year = 2026 AND (id = $1 OR posting_group_id = $1)
				ORDER BY ctid LIMIT 1`, first).Scan(&head); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store the first entry behind its copies: %v", err)
	}
	t.Logf("first entry behind its copies after %d moves", moves)
}
