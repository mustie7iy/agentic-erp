package agents

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// usageLine is one principal's calls in a month, from the ledger.
type usageLine struct {
	principal                                     uuid.UUID
	calls, input, output, write, read, redactions int64
	cost                                          money.Micro
}

// GetAiUsage implements GET /v1/ai-usage: the month's model calls per
// principal, from the ledger (agents.model_call, ADR-0036). Costs are
// summed exact and rounded once, per principal and in total.
func (e *Endpoints) GetAiUsage(ctx context.Context, params gen.GetAiUsageParams) (gen.GetAiUsageRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	month := monthOf(clock.Now(e.Now))
	if m, ok := params.Month.Get(); ok {
		month, _ = time.Parse("2006-01", m) // the contract's pattern admits only valid months
	}
	lines, err := usageIn(ctx, tx, month)
	if err != nil {
		return nil, err
	}
	pol, err := ReadPolicy(ctx, tx)
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, PriceCurrency, nil)
	if err != nil {
		return nil, fmt.Errorf("agents: AI usage: %w", err)
	}
	out := gen.AiUsage{Month: month.Format("2006-01"), Principals: []gen.AiUsagePrincipal{}}
	var total money.Micro
	for _, l := range lines {
		out.Calls += l.calls
		total = total.Add(l.cost)
		out.Principals = append(out.Principals, gen.AiUsagePrincipal{PrincipalID: l.principal, Calls: l.calls, InputTokens: l.input,
			OutputTokens: l.output, CacheWriteTokens: l.write, CacheReadTokens: l.read, Redactions: l.redactions,
			Cost: apitypes.Money(l.cost.Minor(), PriceCurrency, exp)})
	}
	out.Cost = apitypes.Money(total.Minor(), PriceCurrency, exp)
	if pol.MonthlyCap != nil {
		out.MonthlyCap.SetTo(apitypes.Money(*pol.MonthlyCap, PriceCurrency, exp))
	}
	return &gen.AiUsageHeaders{Response: out}, nil
}

// usageIn sums the month's calls per principal, the most costly first. A
// sum past what an int64 holds is held at that most, as costs are.
func usageIn(ctx context.Context, tx pgx.Tx, month time.Time) ([]usageLine, error) {
	rows, _ := tx.Query(ctx, `SELECT principal_id, count(*), sum(input_tokens), sum(output_tokens), sum(cache_write_tokens), sum(cache_read_tokens),
			sum(redactions), LEAST(sum(cost_micro), 9223372036854775807)::bigint AS cost
		FROM agents.model_call WHERE called_at >= $1 AND called_at < $2
		GROUP BY principal_id ORDER BY cost DESC, principal_id`, month, month.AddDate(0, 1, 0))
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (usageLine, error) {
		var l usageLine
		err := r.Scan(&l.principal, &l.calls, &l.input, &l.output, &l.write, &l.read, &l.redactions, &l.cost)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("agents: AI usage in %s: %w", month.Format("2006-01"), err)
	}
	return lines, nil
}
