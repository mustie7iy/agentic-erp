package agents

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Budgets (ADR-0036): every call is held against its tenant's line and,
// for an agent, the agent's own, per calendar month (UTC). Before the call
// is forwarded what it may cost (mostOf, an estimate of its input and all
// of its output) is reserved on each line, read FOR UPDATE (ADR-0008), so
// calls in flight together do not spend past a cap by more than that
// estimate's error; after it, the actual cost is settled.
//
// Lock order, in every transaction: an agent session (recordCall), then
// the tenant's line, then the agent's. Nothing locks a session after a
// line, and the caps are read before any line is locked, so no two calls
// deadlock; an AgentBudget must take no lock.

// AgentBudget reads an agent's monthly budget in tx, in minor units of its
// currency; ok false means none (identity.AgentBudget, identity's
// published read: agents reads no identity table).
type AgentBudget func(ctx context.Context, tx pgx.Tx, agent uuid.UUID) (amount money.Minor, currency string, ok bool, err error)

// HeldRetry is how soon a call refused because calls in flight hold the
// budget may be retried: they settle at their cost, mostly far under what
// they hold, within seconds.
const HeldRetry = 10 * time.Second

// thresholds are the shares of a cap announced, each once a month.
var thresholds = []int16{80, 100}

// line is a budget line a call is held against.
type line struct {
	id    uuid.UUID
	agent *uuid.UUID   // nil: the tenant's line
	limit *money.Micro // nil: no cap
	spent money.Micro
	held  money.Micro
	shown int16 // the highest threshold announced
}

func (l line) kind() string {
	if l.agent == nil {
		return "tenant"
	}
	return "agent"
}

// reservation is what a call holds on its lines until it is settled.
type reservation struct {
	call  uuid.UUID
	month time.Time
	lines []line
}

// monthOf is the first day of now's month, UTC.
func monthOf(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// reserve holds most against p's lines for one call, or refuses it with
// 429 agents.budget_exhausted, writing the refusal's audit entry in tx so
// that it stands although the call does not.
func (g *Gateway) reserve(ctx context.Context, tx pgx.Tx, p reqctx.Principal, pol Policy, most money.Micro, now time.Time) (reservation, *problem.Problem, error) {
	res := reservation{call: uuid.Must(uuid.NewV7()), month: monthOf(now)}
	limits, err := g.limits(ctx, tx, p, pol)
	if err != nil {
		return res, nil, err
	}
	for _, agent := range linesOf(p) {
		l, err := lockLine(ctx, tx, p.TenantID, agent, res.month, now)
		if err != nil {
			return res, nil, err
		}
		l.limit = limits[l.kind()]
		if l.limit != nil && l.spent.Add(l.held).Add(most) > *l.limit {
			spent := l.spent.Add(most) > *l.limit // not only held by calls in flight
			return res, refusal(res.month, now, spent), audit.Write(ctx, tx, p, reqctx.TraceID(ctx), now, audit.Entry{Action: "agents.budget.refuse",
				ObjectType: "agents.spend", ObjectID: l.id, Outcome: "refused",
				Detail: map[string]any{"line": l.kind(), "month": res.month.Format("2006-01"), "held": !spent}})
		}
		res.lines = append(res.lines, l)
	}
	for _, l := range res.lines {
		if _, err := tx.Exec(ctx, `INSERT INTO agents.model_reservation (tenant_id, id, spend_id, amount_micro, reserved_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, p.TenantID, res.call, l.id, most, now, now.Add(g.timeout()+WriteMargin)); err != nil {
			return res, nil, fmt.Errorf("agents: reserve on %s line %s: %w", l.kind(), l.id, err)
		}
	}
	return res, nil, nil
}

// linesOf are the lines p's calls are held against, in lock order: the
// tenant's (nil), then an agent's own.
func linesOf(p reqctx.Principal) []*uuid.UUID {
	if p.Kind == reqctx.KindAgent {
		return []*uuid.UUID{nil, &p.ID}
	}
	return []*uuid.UUID{nil}
}

// limits are the caps of p's lines: the tenant's (its AI policy's), and
// an agent's monthly budget. A budget in another currency than the
// prices' cannot be compared with costs, and refuses the agent's calls
// rather than leaving them uncapped.
func (g *Gateway) limits(ctx context.Context, tx pgx.Tx, p reqctx.Principal, pol Policy) (map[string]*money.Micro, error) {
	out := map[string]*money.Micro{}
	if pol.MonthlyCap != nil {
		limit := money.MicroOf(*pol.MonthlyCap)
		out["tenant"] = &limit
	}
	if p.Kind != reqctx.KindAgent || g.AgentBudget == nil {
		return out, nil
	}
	budget, cur, ok, err := g.AgentBudget(ctx, tx, p.ID)
	switch {
	case err != nil:
		return nil, fmt.Errorf("agents: read the budget of agent %s: %w", p.ID, err)
	case !ok:
		return out, nil
	case cur != PriceCurrency:
		return nil, problem.Unprocessable("agents.budget_currency", "The agent's monthly budget is in "+cur+"; model costs are in "+PriceCurrency+
			", so the budget cannot be held. Set it in "+PriceCurrency+".")
	}
	limit := money.MicroOf(budget)
	out["agent"] = &limit
	return out, nil
}

// lockLine reads a line FOR UPDATE, creating it on its month's first call,
// with what its calls in flight hold: their unexpired, unsettled
// reservations.
func lockLine(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, agent *uuid.UUID, month, now time.Time) (line, error) {
	l := line{agent: agent}
	if _, err := tx.Exec(ctx, `INSERT INTO agents.spend (tenant_id, id, principal_id, month, currency) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT ON CONSTRAINT spend_line_key DO NOTHING`, tenant, uuid.Must(uuid.NewV7()), agent, month, PriceCurrency); err != nil {
		return l, fmt.Errorf("agents: open the %s budget line of %s: %w", l.kind(), month.Format("2006-01"), err)
	}
	if err := tx.QueryRow(ctx, `SELECT id, spent_micro, threshold FROM agents.spend WHERE month = $1 AND principal_id IS NOT DISTINCT FROM $2 FOR UPDATE`,
		month, agent).Scan(&l.id, &l.spent, &l.shown); err != nil {
		return l, fmt.Errorf("agents: lock the %s budget line of %s: %w", l.kind(), month.Format("2006-01"), err)
	}
	if err := tx.QueryRow(ctx, `SELECT least(coalesce(sum(amount_micro), 0), 9223372036854775807)::bigint FROM agents.model_reservation
		WHERE spend_id = $1 AND settled_at IS NULL AND expires_at > $2`, l.id, now).Scan(&l.held); err != nil {
		return l, fmt.Errorf("agents: read what is held on budget line %s: %w", l.id, err)
	}
	return l, nil
}

// refusal is 429 agents.budget_exhausted: to be retried when the budget
// renews, at the next month, when it is spent; soon, when calls in flight
// hold it and may leave room once they settle.
func refusal(month, now time.Time, spent bool) *problem.Problem {
	next := month.AddDate(0, 1, 0)
	if !spent {
		p := problem.New(http.StatusTooManyRequests, "agents.budget_exhausted", "Model budget exhausted",
			"The monthly model budget is held by calls in flight; retry once they finish.")
		p.RetryAfter = int(HeldRetry / time.Second)
		return p
	}
	p := problem.New(http.StatusTooManyRequests, "agents.budget_exhausted", "Model budget exhausted",
		"The call does not fit what is left of the monthly model budget; it renews on "+next.Format(time.DateOnly)+".")
	p.RetryAfter = int((next.Sub(now) + time.Second - 1) / time.Second)
	return p
}

// settle replaces what the call held on each line with what it cost, and
// announces each threshold of a cap the line reaches for the first time
// this month. It runs once per call, in the transaction that records it.
func settle(ctx context.Context, tx pgx.Tx, p reqctx.Principal, res reservation, cost money.Micro, now time.Time) error {
	for _, l := range res.lines {
		if err := tx.QueryRow(ctx, `SELECT spent_micro, threshold FROM agents.spend WHERE id = $1 FOR UPDATE`, l.id).Scan(&l.spent, &l.shown); err != nil {
			return fmt.Errorf("agents: lock budget line %s: %w", l.id, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agents.model_reservation SET settled_at = $3, version = version + 1 WHERE id = $1 AND spend_id = $2`,
			res.call, l.id, now); err != nil {
			return fmt.Errorf("agents: settle the reservation on budget line %s: %w", l.id, err)
		}
		l.spent = l.spent.Add(cost)
		if err := announce(ctx, tx, p, res.month, &l, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents.spend SET spent_micro = $2, threshold = $3, version = version + 1 WHERE id = $1`,
			l.id, l.spent, l.shown); err != nil {
			return fmt.Errorf("agents: add the call to budget line %s: %w", l.id, err)
		}
	}
	return nil
}

// announce publishes BudgetThresholdReached, with its audit entry, for
// each threshold of the line's cap reached and not yet announced.
func announce(ctx context.Context, tx pgx.Tx, p reqctx.Principal, month time.Time, l *line, now time.Time) error {
	if l.limit == nil {
		return nil
	}
	for _, t := range thresholds {
		if t <= l.shown || !l.spent.Reaches(*l.limit, int64(t)) {
			continue
		}
		exp, err := currency.Exponent(ctx, tx, PriceCurrency, nil)
		if err != nil {
			return fmt.Errorf("agents: announce budget threshold: %w", err)
		}
		e := outbox.Event{Context: "agents", Name: "budget_threshold_reached", AggregateType: "agents.spend", AggregateID: l.id,
			Payload: map[string]any{"line": l.kind(), "agent_id": l.agent, "month": month.Format("2006-01"), "threshold": t,
				"limit": money.EventAmount(l.limit.Minor(), PriceCurrency, exp), "spent": money.EventAmount(l.spent.Minor(), PriceCurrency, exp)}}
		if _, err := outbox.Write(ctx, tx, p.TenantID, p.ID, reqctx.TraceID(ctx), e); err != nil {
			return fmt.Errorf("agents: announce budget threshold: %w", err)
		}
		if err := audit.Write(ctx, tx, p, reqctx.TraceID(ctx), now, audit.Entry{Action: "agents.budget.threshold", ObjectType: "agents.spend",
			ObjectID: l.id, Outcome: "succeeded", Detail: map[string]any{"line": l.kind(), "month": month.Format("2006-01"), "threshold": t}}); err != nil {
			return err
		}
		l.shown = t
	}
	return nil
}
