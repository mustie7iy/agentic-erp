package closing_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// withThreeLedgers is withSignOff with company 2000 keeping three ledgers
// created in the order 1L, 0L, 0A: their ids (and rows) sort 1L, 0L, 0A,
// their codes 0A, 0L, 1L, and postings and sign-offs lock their periods
// in neither order but the leading ledger's first, then by code: 0L, 0A,
// 1L (ADR-0058 §5).
func withThreeLedgers(doc *config.Config) {
	withSignOff(doc)
	doc.Ledgers = []config.Ledger{{Code: "1L", Name: "Tax ledger", AccountingPrinciple: "TAX"},
		{Code: config.LeadingLedger, Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "0A", Name: "Statutory", AccountingPrinciple: "HGB"}}
	doc.LedgerGroups = []config.LedgerGroup{{Code: "ALL", Name: "Every ledger", Representative: config.LeadingLedger,
		Ledgers: []string{"1L", config.LeadingLedger, "0A"}}}
	doc.Company("2000").Ledgers = []string{"1L", config.LeadingLedger, "0A"}
}

// lockOrder is the order a sign-off and a posting lock company 2000's
// three ledgers' periods in.
var lockOrder = []string{config.LeadingLedger, "0A", "1L"}

// backend runs work in a committed unit of work and sends the backend's
// pid before it starts, and work's result when it ends.
func backend(t *testing.T, e fixture.Env, work func(ctx context.Context) error) (<-chan int32, <-chan error) {
	t.Helper()
	pid, done := make(chan int32, 1), make(chan error, 1)
	go func() {
		err := errSetup
		defer func() { done <- err }()
		err = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			var n int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&n); err != nil {
				return err
			}
			pid <- n
			return work(ctx)
		})
	}()
	return pid, done
}

// errSetup is what backend reports for a transaction whose setup failed.
var errSetup = fmt.Errorf("transaction setup failed")

// TestSignOffAndPostingLockInOneOrder runs a sign-off and a posting dated
// in the period at once, on a company with three ledgers whose ids and
// codes sort differently. A third transaction holds a share lock, as a
// posting to those ledgers alone would, on the periods of every ledger
// after the first k in the lock order: the sign-off locks the first k and
// waits; the posting, to every ledger, then waits for the sign-off without
// holding any period lock the sign-off needs next. Released, the sign-off
// locks the rest and commits, and the posting reads the period locked and
// is refused: in any other order one of them would deadlock.
func TestSignOffAndPostingLockInOneOrder(t *testing.T) {
	e := fixture.NewWith(t, nil, withThreeLedgers, signPermissions...)
	h := endpoints(t)
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, TaxReturns: taxfiling.Holds{}}
	expense, bank := e.Account(t, "2000", "520000"), e.Account(t, "2000", "100000")
	for k := 1; k < len(lockOrder); k++ {
		t.Run(fmt.Sprintf("the first %d locked", k), func(t *testing.T) {
			s := signable(t, e, k)
			periods := map[string]uuid.UUID{}
			for _, x := range apitest.List(t, s.read(t), "ledgers") {
				l := x.(map[string]any)
				periods[l["ledger_code"].(string)] = uuid.MustParse(l["fiscal_period_id"].(string))
			}
			if got := slices.Sorted(maps.Keys(periods)); strings.Join(got, " ") != "0A 0L 1L" {
				t.Fatalf("covered ledgers: got %v, want 0A 0L 1L", got)
			}
			s.periods(t, "close")
			s.read(t)
			var held []uuid.UUID
			for _, code := range lockOrder[k:] {
				held = append(held, periods[code])
			}
			holder := pgtest.Admin(t)
			ctx := context.Background()
			tx, err := holder.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // released below; a failed test releases it here
			var holderPID int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			rows, _ := tx.Query(ctx, `SELECT id FROM ledger.period_status WHERE id = ANY($1) FOR SHARE`, held)
			if n, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil || len(n) != len(held) {
				t.Fatalf("hold the periods: got %v, %v", n, err)
			}

			id := uuid.MustParse(strings.TrimPrefix(s.path, "/v1/period-closes/"))
			signPID, signed := backend(t, e, func(ctx context.Context) error {
				_, err := h.SignOffPeriodClose(ctx, gen.OptPeriodCloseSignOffInput{}, gen.SignOffPeriodCloseParams{ID: id, IfMatch: s.tag})
				return err
			})
			signer := <-signPID
			waitBlocked(t, signer, holderPID, signed, "the sign-off")
			day := time.Date(2026, time.Month(k), 15, 0, 0, 0, 0, time.UTC)
			postPID, posted := backend(t, e, func(ctx context.Context) error {
				_, err := post.Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "EUR",
					SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{{AccountID: expense, Amount: 500}, {AccountID: bank, Amount: -500}}})
				return err
			})
			waitBlocked(t, <-postPID, signer, posted, "the posting")
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-signed; err != nil {
				t.Errorf("sign-off: got %v, want it committed", err)
			}
			if err := <-posted; problem.From(err).Code != "ledger.period_not_open" {
				t.Errorf("posting after the sign-off: got %v, want ledger.period_not_open", err)
			}
		})
	}
}

// waitBlocked waits until the backend pid waits for blocker's locks; the
// transaction must not finish before (done).
func waitBlocked(t *testing.T, pid, blocker int32, done <-chan error, what string) {
	t.Helper()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case err := <-done:
			t.Fatalf("%s finished with %v, want it waiting for backend %d", what, err, blocker)
		default:
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM unnest(pg_blocking_pids($1)) b WHERE b = $2`, []any{pid, blocker}, &n)
		return n > 0
	}, "%s neither waited for backend %d nor finished", what, blocker)
}
