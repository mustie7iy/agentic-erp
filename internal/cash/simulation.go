package cash

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The derivation simulation (ADR-0051 §4, T53e3): a change of the
// liquidity item derivation rules is tier 2 and approved on evidence that
// lists the open flows whose items it moves. The configuration context
// runs it with a change request's simulation (configuration.Simulators,
// adapted in internal/app, as cash imports no context), as the person who
// asked for it, and shows each reader of the request the companies they
// may access alone (Disclose).

// SimulationKey is the simulation detail's key for what classifying each
// company's open flows under the proposed derivation found.
const SimulationKey = "liquidity_items_by_company"

// maxMovedListed bounds the moved flows listed per company; the count
// and the totals by item cover them all.
const maxMovedListed = 100

// Simulation classifies the open flows of the liquidity forecast under the
// active and the proposed derivation.
type Simulation struct {
	Endpoints *Endpoints
}

// SimulationResult is what the simulation found, in the shape of a
// change request's simulation evidence: whether it passed, the summary and
// the detail, under SimulationKey.
type SimulationResult struct {
	Passed  bool
	Summary string
	Detail  map[string]any
}

// CompanyMoves is what classifying one company's open flows found,
// recorded per company so each reader is shown the companies they may
// access alone (Disclose): how many flows the proposal classifies
// differently, the first of them by day (at most 100), and per currency
// and item what those flows give it before and after.
type CompanyMoves struct {
	Company       string      `json:"company"`
	LegalEntityID uuid.UUID   `json:"legal_entity_id"`
	Moved         int         `json:"moved"`
	Flows         []MovedFlow `json:"flows"`
	Totals        []ItemTotal `json:"totals"`
}

// MovedFlow is an open flow the proposal classifies differently: its
// currency, day, source, certainty and amount, and its shares by item
// before and after. It names no document and no supplier, as the
// forecast does not (cash.forecast.read).
type MovedFlow struct {
	ValueDate string      `json:"value_date"`
	Source    string      `json:"source"`
	Certainty string      `json:"certainty"`
	Amount    gen.Money   `json:"amount"`
	Base      []ShareJSON `json:"base"`
	Proposed  []ShareJSON `json:"proposed"`
}

// ShareJSON is a flow's share of a liquidity item ("" for none).
type ShareJSON struct {
	Item   string    `json:"liquidity_item"`
	Amount gen.Money `json:"amount"`
}

// ItemTotal is what the moved flows of a currency give an item before
// and after the proposal.
type ItemTotal struct {
	Item     string    `json:"liquidity_item"`
	Base     gen.Money `json:"base"`
	Proposed gen.Money `json:"proposed"`
}

// recordedSummary is the simulation's summary as recorded: no count, since
// counts over every company would tell a reader of narrower access that
// other companies' flows move.
const recordedSummary = "Open flows of the liquidity forecast classified under the proposed liquidity item derivation."

// Simulate classifies the open flows of each company of the active
// document the requester may act in, which must hold cash.forecast.read;
// a proposal that leaves the derivation as it is records nothing. The
// flows are the forecast's (every source, every certainty, after its last
// bucket too), dated by the active document's settings.
func (s Simulation) Simulate(ctx context.Context, base, proposed *config.Config) (SimulationResult, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return SimulationResult{}, err
	}
	if reflect.DeepEqual(base.Cash.Derivation, proposed.Cash.Derivation) {
		return SimulationResult{Passed: true}, nil
	}
	if err := authz.Require(ctx, "cash.forecast.read"); err != nil {
		return SimulationResult{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return SimulationResult{}, err
	}
	companies := []CompanyMoves{}
	for _, co := range base.Companies {
		le, ok, err := s.Endpoints.LegalEntities.ByCode(ctx, co.Code)
		if err != nil {
			return SimulationResult{}, fmt.Errorf("cash: legal entity of company %s: %w", co.Code, err)
		}
		if !ok || !access.Allows(le) {
			continue
		}
		moves, err := s.company(ctx, tx, base, proposed, co.Code, le)
		if err != nil {
			return SimulationResult{}, err
		}
		companies = append(companies, moves)
	}
	return SimulationResult{Passed: true, Summary: recordedSummary, Detail: map[string]any{SimulationKey: companies}}, nil
}

// company classifies the company's open flows before and after.
func (s Simulation) company(ctx context.Context, tx pgx.Tx, base, proposed *config.Config, code string, le uuid.UUID) (CompanyMoves, error) {
	out := CompanyMoves{Company: code, LegalEntityID: le, Flows: []MovedFlow{}, Totals: []ItemTotal{}}
	read, _, err := s.Endpoints.readDate(base, code)
	if err != nil {
		return out, err
	}
	r := reader{doc: base, company: code, settings: companyCash(base, code), read: read, sums: &sums{}}
	bs := buckets(read, r.settings.BucketsOrDefault())
	flows, _, err := s.Endpoints.forecastFlows(ctx, tx, r, le, bs[len(bs)-1].End)
	if err != nil {
		return out, err
	}
	var moved []movedFlow
	was, is := newRules(base.Cash.Derivation), newRules(proposed.Cash.Derivation)
	for _, f := range flows {
		shares, err := classifyUnder(r.sums, f, was, is)
		if err != nil {
			return out, err
		}
		if !maps.Equal(byItem(shares[0]), byItem(shares[1])) {
			moved = append(moved, movedFlow{datedFlow: f, was: shares[0], is: shares[1]})
		}
	}
	if r.sums.overflow {
		return out, errForecastTooLarge()
	}
	slices.SortStableFunc(moved, func(a, b movedFlow) int {
		return cmp.Or(a.date.Compare(b.date), cmp.Compare(a.currency, b.currency), cmp.Compare(a.source, b.source))
	})
	return r.moves(ctx, tx, out, moved)
}

// classifyUnder is the flow's shares under each of the derivations.
func classifyUnder(s *sums, f datedFlow, ds ...*rules) ([][]Share, error) {
	out := make([][]Share, len(ds))
	for i, d := range ds {
		var err error
		if out[i], err = classify(d, s, f.amount, f.parts); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// movedFlow is a flow with its shares before and after.
type movedFlow struct {
	datedFlow
	was, is []Share
}

// byItem is the shares keyed by item, those of nothing left out.
func byItem(ss []Share) map[string]money.Minor {
	out := make(map[string]money.Minor, len(ss))
	for _, s := range ss {
		if s.Amount != 0 {
			out[s.Item] = s.Amount
		}
	}
	return out
}

// moves records the moved flows: their count, the first listed, and per
// currency and item what they give it before and after.
func (r reader) moves(ctx context.Context, tx pgx.Tx, out CompanyMoves, moved []movedFlow) (CompanyMoves, error) {
	out.Moved = len(moved)
	exps := map[string]int{}
	type key struct{ currency, item string }
	totals := map[key]*[2]money.Minor{}
	for i, m := range moved {
		exp, ok := exps[m.currency]
		if !ok {
			var err error
			if exp, err = currency.Exponent(ctx, tx, m.currency, fmt.Errorf("cash: currency %s of a forecast flow is not known", m.currency)); err != nil {
				return out, err
			}
			exps[m.currency] = exp
		}
		for side, ss := range [][]Share{m.was, m.is} {
			for _, sh := range ss {
				k := key{m.currency, sh.Item}
				if totals[k] == nil {
					totals[k] = &[2]money.Minor{}
				}
				totals[k][side] = r.sums.add(totals[k][side], sh.Amount)
			}
		}
		if i < maxMovedListed {
			out.Flows = append(out.Flows, MovedFlow{ValueDate: m.date.Format("2006-01-02"), Source: m.source, Certainty: m.certainty,
				Amount: apitypes.Money(m.amount, m.currency, exp), Base: sharesJSON(m.was, m.currency, exp), Proposed: sharesJSON(m.is, m.currency, exp)})
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(totals), func(a, b key) int { return cmp.Or(cmp.Compare(a.currency, b.currency), cmp.Compare(a.item, b.item)) }) {
		t := totals[k]
		out.Totals = append(out.Totals, ItemTotal{Item: k.item, Base: apitypes.Money(t[0], k.currency, exps[k.currency]),
			Proposed: apitypes.Money(t[1], k.currency, exps[k.currency])})
	}
	if r.sums.overflow {
		return out, errForecastTooLarge()
	}
	return out, nil
}

// sharesJSON shows shares in their currency.
func sharesJSON(ss []Share, cur string, exp int) []ShareJSON {
	out := make([]ShareJSON, len(ss))
	for i, s := range ss {
		out[i] = ShareJSON{Item: s.Item, Amount: apitypes.Money(s.Amount, cur, exp)}
	}
	return out
}

// Disclose is a simulation's detail and summary as a reader with the
// access is shown them: the companies they may access alone, with the
// summary's count over those (ADR-0010, as the ledger's assignment
// simulation does). A detail without SimulationKey is another context's,
// returned as it is. It fails closed: a reader of no company is shown no
// company and a count of zero.
func Disclose(detail json.RawMessage, summary string, access authz.Access) (json.RawMessage, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(detail, &fields); err != nil {
		return nil, "", fmt.Errorf("cash: simulated liquidity items: %w", err)
	}
	if fields[SimulationKey] == nil {
		return detail, summary, nil
	}
	var companies []CompanyMoves
	if err := json.Unmarshal(fields[SimulationKey], &companies); err != nil {
		return nil, "", fmt.Errorf("cash: simulated liquidity items: %w", err)
	}
	shown, moved := []CompanyMoves{}, 0
	for _, c := range companies {
		if access.Allows(c.LegalEntityID) {
			shown, moved = append(shown, c), moved+c.Moved
		}
	}
	// A slice of structs of strings and numbers marshals without fail, and
	// so does a map of raw values just read.
	fields[SimulationKey], _ = json.Marshal(shown)
	out, _ := json.Marshal(fields)
	return out, strings.Replace(summary, recordedSummary, fmt.Sprintf(
		"Of the open flows of the liquidity forecast of the companies shown (%d), %d would be classified under other liquidity items by the proposed derivation.",
		len(shown), moved), 1), nil
}
