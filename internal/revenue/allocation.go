package revenue

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Active is the status of a revenue contract once it is activated: its
// transaction price is allocated and its obligations are fixed
// (ADR-0068 §7).
const Active = "active"

// OverTime is the timing of an obligation satisfied over a service
// period (ADR-0068 §2).
const OverTime = "over_time"

func init() {
	// The handlers check under the contract's row lock; the triggers are
	// the database's rules and answer alike.
	problem.RegisterConstraint("contract_fixed_once_active", notDraft)
	problem.RegisterConstraint("performance_obligation_fixed_once_active", notDraft)
}

// notDraft refuses a change of a contract that is no longer a draft.
func notDraft() *problem.Problem {
	return problem.Conflict("revenue.invalid_status", "The revenue contract is active; only a draft is changed or activated.")
}

// Amount is an API amount: a decimal string and its currency.
type Amount struct{ Value, Currency string }

// ObligationChange is what a person sets on one obligation of a draft
// contract, as a client sends it: a nil field is left as it is. A date
// pointing at nil is removed.
type ObligationChange struct {
	No          int32
	Timing      *string
	ServiceFrom **time.Time
	ServiceTo   **time.Time
	SatisfiedOn **time.Time
	Price       *Amount
}

// draft reads the contract under its row lock for a change (ADR-0008):
// one of a company the caller may act in, at the version the caller read,
// that is still a draft (409 revenue.invalid_status). The lock is taken
// by a statement of its own, so the obligations read after it are as the
// transaction that held it left them.
func draft(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Contract, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contract{}, fmt.Errorf("revenue: company access for contract %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `SELECT FROM revenue.contract WHERE id = $1 FOR UPDATE`, id); err != nil {
		return Contract{}, fmt.Errorf("revenue: lock contract %s: %w", id, err)
	}
	c, err := load(ctx, tx, id)
	if err != nil {
		return Contract{}, err
	}
	if !access.Allows(c.LegalEntityID) {
		return Contract{}, problem.NotFoundOf("revenue contract")
	}
	if err := ifMatch(c.Version); err != nil {
		return Contract{}, err
	}
	if c.Status != Draft {
		return Contract{}, notDraft()
	}
	return c, nil
}

// Update sets what a person judges on a draft contract's obligations
// (ADR-0068 §2, §7): each change names an obligation by its number, once,
// and gives its timing, its service period, the day it is satisfied on
// or its stand-alone selling price; what it does not give stays.
// Afterwards an over-time obligation has a service period, from not after
// to, and no day it is satisfied on, a point-in-time obligation has no
// service period, and every stand-alone selling price is more than zero,
// in the contract's currency. The contract's version moves; its audit record and
// RevenueContractChanged are written in the same transaction. Nothing is
// allocated before the contract is activated.
func (e *Endpoints) Update(ctx context.Context, id uuid.UUID, changes []ObligationChange, ifMatch func(int32) error) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	c, err := draft(ctx, tx, id, ifMatch)
	if err != nil {
		return Contract{}, err
	}
	changed, err := applied(c, changes)
	if err != nil {
		return Contract{}, err
	}
	numbers, timings := make([]int32, len(changed)), make([]string, len(changed))
	from, to, on, prices := make([]*time.Time, len(changed)), make([]*time.Time, len(changed)), make([]*time.Time, len(changed)), make([]int64, len(changed))
	for i, o := range changed {
		numbers[i], timings[i], from[i], to[i], on[i], prices[i] = o.No, o.Timing, o.ServiceFrom, o.ServiceTo, o.SatisfiedOn, int64(o.StandalonePrice)
	}
	if _, err := tx.Exec(ctx, `UPDATE revenue.performance_obligation o
		SET timing = n.timing, service_from = n.service_from, service_to = n.service_to, satisfied_on = n.satisfied_on, standalone_selling_price = n.price
		FROM unnest($2::int[], $3::text[], $4::date[], $5::date[], $6::date[], $7::bigint[])
			AS n (obligation_no, timing, service_from, service_to, satisfied_on, price)
		WHERE o.contract_id = $1 AND o.obligation_no = n.obligation_no`, id, numbers, timings, from, to, on, prices); err != nil {
		return Contract{}, fmt.Errorf("revenue: set the obligations of contract %s: %w", id, err)
	}
	before := Draft
	return e.changed(ctx, tx, p, c, "revenue.contract.update", Draft, &before)
}

// changed moves the contract's version, with its status, and records the
// change: it returns the contract as it is afterwards.
func (e *Endpoints) changed(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Contract, action, status string, before *string) (Contract, error) {
	if _, err := tx.Exec(ctx, `UPDATE revenue.contract SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, c.ID, status, p.ID); err != nil {
		return Contract{}, fmt.Errorf("revenue: mark contract %s %s: %w", c.ID, status, err)
	}
	after, err := load(ctx, tx, c.ID)
	if err != nil {
		return Contract{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, after.LegalEntityID) // a stored contract's legal entity exists
	if err != nil {
		return Contract{}, fmt.Errorf("revenue: company of contract %s: %w", c.ID, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{after.CustomerID})
	if err != nil {
		return Contract{}, fmt.Errorf("revenue: customer of contract %s: %w", c.ID, err)
	}
	return after, record(ctx, action, byRequest, after, le, names[after.CustomerID].Number, before)
}

// applied is the contract's obligations the changes name, as they are
// after them, in the changes' order. Every refusal names the request's
// own field: an obligation the contract does not have (422
// revenue.unknown_obligation) or named twice (422
// revenue.obligation_repeated), and what set refuses.
func applied(c Contract, changes []ObligationChange) ([]Obligation, error) {
	at := make(map[int32]int, len(c.Obligations))
	for i, o := range c.Obligations {
		at[o.No] = i
	}
	seen := map[int32]bool{}
	out := make([]Obligation, len(changes))
	for i, ch := range changes {
		field := fmt.Sprintf("obligations[%d]", i)
		j, ok := at[ch.No]
		if !ok {
			return nil, problem.Invalid("revenue.unknown_obligation", field+".obligation_no",
				fmt.Sprintf("The contract has no obligation %d.", ch.No))
		}
		if seen[ch.No] {
			return nil, problem.Invalid("revenue.obligation_repeated", field+".obligation_no",
				fmt.Sprintf("Obligation %d is named twice; a request names an obligation once.", ch.No))
		}
		seen[ch.No] = true
		o, err := set(c, c.Obligations[j], ch, field)
		if err != nil {
			return nil, err
		}
		out[i] = o
	}
	return out, nil
}

// set is the obligation with the change applied, checked (ADR-0068 §7):
// a timing the context knows (422 revenue.unknown_timing); a stand-alone
// selling price in the contract's currency (422
// revenue.currency_mismatch) and more than zero (422
// revenue.price_not_positive); an over-time obligation with both days of
// its service period (422 revenue.service_period_required), from not
// after to (422 revenue.invalid_service_period) and without a day it is
// satisfied on (422 revenue.satisfied_on_not_allowed); a point-in-time
// obligation without a service period (422
// revenue.service_period_not_allowed). Which timing an obligation has,
// and the day a point-in-time obligation is satisfied on when a person
// names one (ADR-0068 §8), are the person's judgement and are not
// checked.
func set(c Contract, o Obligation, ch ObligationChange, field string) (Obligation, error) {
	if ch.Timing != nil {
		if *ch.Timing != PointInTime && *ch.Timing != OverTime {
			return Obligation{}, problem.Invalid("revenue.unknown_timing", field+".timing", "An obligation is satisfied at a point in time (point_in_time) or over time (over_time).")
		}
		o.Timing = *ch.Timing
	}
	if ch.ServiceFrom != nil {
		o.ServiceFrom = *ch.ServiceFrom
	}
	if ch.ServiceTo != nil {
		o.ServiceTo = *ch.ServiceTo
	}
	if ch.SatisfiedOn != nil {
		o.SatisfiedOn = *ch.SatisfiedOn
	}
	if ch.Price != nil {
		price, err := priced(c, *ch.Price, field+".standalone_selling_price")
		if err != nil {
			return Obligation{}, err
		}
		o.StandalonePrice = price
	}
	return o, period(o, field)
}

// priced reads a stand-alone selling price: an amount of the contract's
// currency, more than zero.
func priced(c Contract, a Amount, field string) (money.Minor, error) {
	if a.Currency != c.Currency {
		return 0, problem.Invalid("revenue.currency_mismatch", field+".currency", "Every amount of a revenue contract is in its currency, "+c.Currency+".")
	}
	price, err := currency.ParseAmount(a.Value, c.Currency, c.Exponent, field+".amount")
	if err != nil {
		return 0, fmt.Errorf("revenue: stand-alone selling price: %w", err)
	}
	if price <= 0 {
		return 0, problem.Invalid("revenue.price_not_positive", field+".amount", "A stand-alone selling price is more than zero.")
	}
	return price, nil
}

// period holds the obligation's service period to its timing.
func period(o Obligation, field string) error {
	if o.Timing != OverTime {
		// In the request's order of the two days, so the same request is refused by the same field.
		for _, d := range []struct {
			name string
			day  *time.Time
		}{{"service_from", o.ServiceFrom}, {"service_to", o.ServiceTo}} {
			if d.day != nil {
				return problem.Invalid("revenue.service_period_not_allowed", field+"."+d.name,
					fmt.Sprintf("Obligation %d is satisfied at a point in time and has no service period; set timing to over_time, or send the service dates as null.", o.No))
			}
		}
		return nil
	}
	if o.SatisfiedOn != nil {
		return problem.Invalid("revenue.satisfied_on_not_allowed", field+".satisfied_on",
			fmt.Sprintf("Obligation %d is satisfied over time, over its service period, and has no day it is satisfied on; set timing to point_in_time, or send satisfied_on as null.", o.No))
	}
	if o.ServiceFrom == nil {
		return problem.Invalid("revenue.service_period_required", field+".service_from",
			fmt.Sprintf("Obligation %d is satisfied over time and needs its service period, service_from and service_to.", o.No))
	}
	if o.ServiceTo == nil {
		return problem.Invalid("revenue.service_period_required", field+".service_to",
			fmt.Sprintf("Obligation %d is satisfied over time and needs its service period, service_from and service_to.", o.No))
	}
	if o.ServiceFrom.After(*o.ServiceTo) {
		return problem.Invalid("revenue.invalid_service_period", field+".service_to",
			fmt.Sprintf("Obligation %d's service period ends %s, before it starts on %s.", o.No, o.ServiceTo.Format(time.DateOnly), o.ServiceFrom.Format(time.DateOnly)))
	}
	return nil
}

// Activate activates a draft contract, once (ADR-0068 §7): its
// transaction price is allocated to its obligations in proportion to
// their stand-alone selling prices (allocate), each obligation keeps its
// share, and the contract is active, its obligations and their amounts
// fixed. The contract's row is read FOR UPDATE and only a draft is
// activated (ADR-0008): a second activation waits for the first and is
// then refused. The shares, the new status, its audit record and
// RevenueContractChanged are written in the request's transaction.
// Nothing is posted.
func (e *Endpoints) Activate(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	c, err := draft(ctx, tx, id, ifMatch)
	if err != nil {
		return Contract{}, err
	}
	numbers, prices := make([]int32, len(c.Obligations)), make([]money.Minor, len(c.Obligations))
	for i, o := range c.Obligations {
		numbers[i], prices[i] = o.No, o.StandalonePrice
	}
	shares := allocate(c.TransactionPrice, prices)
	amounts := make([]int64, len(shares))
	for i, s := range shares {
		amounts[i] = int64(s)
	}
	if _, err := tx.Exec(ctx, `UPDATE revenue.performance_obligation o SET allocated_amount = n.amount
		FROM unnest($2::int[], $3::bigint[]) AS n (obligation_no, amount)
		WHERE o.contract_id = $1 AND o.obligation_no = n.obligation_no`, id, numbers, amounts); err != nil {
		return Contract{}, fmt.Errorf("revenue: allocate the transaction price of contract %s: %w", id, err)
	}
	before := Draft
	return e.changed(ctx, tx, p, c, "revenue.contract.activate", Active, &before)
}

// allocate shares the transaction price among obligations in proportion
// to their stand-alone selling prices (ADR-0068 §7; IFRS 15 as §2's
// statement 4 takes it, not yet confirmed): an obligation's exact share
// is price × its stand-alone selling price ÷ the sum of the prices, and
// money.Allocate rounds the shares so that they add up to the price to
// the minor unit, a remaining unit going to the largest remainder, the
// earlier obligation first. The same prices always give the same shares.
// With prices that add up to the transaction price, each share is its
// price. The arithmetic is exact: big integers, no floating point
// (ADR-0005).
func allocate(price money.Minor, prices []money.Minor) []money.Minor {
	total := new(big.Int)
	for _, p := range prices {
		total.Add(total, big.NewInt(int64(p)))
	}
	exact := make([]*big.Rat, len(prices))
	for i, p := range prices {
		exact[i] = new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(price)), big.NewInt(int64(p))), total)
	}
	// The exact shares add up to the price, an amount, and none is above it: cannot fail.
	shares, _ := money.Allocate(exact)
	return shares
}
