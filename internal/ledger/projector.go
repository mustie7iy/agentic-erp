package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Projector maintains ledger.period_balance from posted entries (ADR-0006).
// Posting queues each entry in ledger.balance_projection next to its
// JournalEntryPosted event; a round claims a batch of queued entries, adds
// their lines to the balances and marks them projected in one transaction,
// so an entry is counted exactly once however often rounds fail or run
// side by side (claimed rows are skipped by other replicas).
type Projector struct {
	Pool     *database.Pool
	Role     string        // erp_projector: reads the journal and maintains balances for every tenant
	Batch    int           // entries per round; default 500
	Interval time.Duration // idle wait between rounds; default 1s
	Log      *slog.Logger
}

func (p *Projector) batch() int {
	if p.Batch <= 0 {
		return 500
	}
	return p.Batch
}

// RunOnce projects one batch and returns how many entries it projected.
func (p *Projector) RunOnce(ctx context.Context) (int, error) {
	var n int
	err := p.Pool.InTx(ctx, database.Scope{Role: p.Role}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `WITH batch AS (
				SELECT tenant_id, event_id FROM ledger.balance_projection WHERE projected_at IS NULL
				ORDER BY enqueued_at LIMIT $1 FOR UPDATE SKIP LOCKED
			), done AS (
				UPDATE ledger.balance_projection q SET projected_at = now() FROM batch b
				WHERE q.tenant_id = b.tenant_id AND q.event_id = b.event_id
				RETURNING q.tenant_id, q.fiscal_year, q.journal_entry_id
			), added AS (
				INSERT INTO ledger.period_balance AS pb (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
					debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
				SELECT l.tenant_id, l.ledger_id, l.legal_entity_id, l.account_id, l.fiscal_year, l.fiscal_period, e.currency_tc,
					sum(greatest(l.amount_tc, 0)), sum(greatest(-l.amount_tc, 0)), sum(greatest(l.amount_lc, 0)), sum(greatest(-l.amount_lc, 0)),
					sum(greatest(l.amount_gc, 0)), sum(greatest(-l.amount_gc, 0))
				FROM done d
				JOIN ledger.journal_entry e ON e.tenant_id = d.tenant_id AND e.fiscal_year = d.fiscal_year AND e.id = d.journal_entry_id
				JOIN ledger.journal_line l ON l.tenant_id = d.tenant_id AND l.fiscal_year = d.fiscal_year AND l.journal_entry_id = d.journal_entry_id
				GROUP BY 1, 2, 3, 4, 5, 6, 7 ORDER BY 1, 2, 3, 4, 5, 6, 7
				ON CONFLICT (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc) DO UPDATE SET
					debit_tc = pb.debit_tc + EXCLUDED.debit_tc, credit_tc = pb.credit_tc + EXCLUDED.credit_tc,
					debit_lc = pb.debit_lc + EXCLUDED.debit_lc, credit_lc = pb.credit_lc + EXCLUDED.credit_lc,
					debit_gc = pb.debit_gc + EXCLUDED.debit_gc, credit_gc = pb.credit_gc + EXCLUDED.credit_gc, updated_at = now()
				RETURNING 1
			) SELECT count(*) FROM done`, p.batch()).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("balance projector: %w", err)
	}
	return n, nil
}

// Run projects until ctx ends. A full batch is followed at once by the
// next; errors are logged and retried after the interval.
func (p *Projector) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		n, err := p.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			p.Log.ErrorContext(ctx, "balance projection failed", "error", err)
		}
		if err == nil && n >= p.batch() {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
