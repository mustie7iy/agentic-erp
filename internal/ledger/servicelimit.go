package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ServicePostingLimit is the limit a service principal's journal entries
// are held to (ADR-0049 §7, product owner 2026-10-22, T61n).
const ServicePostingLimit = "service_posting_limit"

// ParkLimit is why the ERP parked an entry instead of posting it: its
// debits, in its currency, exceeded the limit, in its company's currency,
// once converted. The figures are the ones measured when it was parked.
type ParkLimit struct {
	Kind           string // ServicePostingLimit
	Debits         money.Minor
	DebitsCurrency string
	DebitsExp      int
	Limit          money.Minor
	LimitCurrency  string
	LimitExp       int
}

// Detail is the reason in words, each figure with its currency.
func (l ParkLimit) Detail() string {
	debits := l.Debits.Plain(l.DebitsExp) + " " + l.DebitsCurrency
	if l.DebitsCurrency != l.LimitCurrency {
		debits += ", converted into " + l.LimitCurrency + " at the average rate of its posting date,"
	}
	return fmt.Sprintf("Posted by a service principal, its debits of %s exceed the company's service posting limit of %s %s: "+
		"it is parked for a person to post (:post).", debits, l.Limit.Plain(l.LimitExp), l.LimitCurrency)
}

// postDirect posts a manual entry at once through the API. A person's is
// refused above its company's manual_entry_approval_limit (T35f). A service
// principal's, an integration such as the pilot's billing system, is held
// to its company's service posting limit instead and parked for a person
// above it, rather than refused, which the integration could not answer
// (T61n); parked is then the entry, and nothing is posted.
func (e *Endpoints) postDirect(ctx context.Context, in NewEntry) (je Entry, parked *ParkedEntry, err error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, nil, err
	}
	switch p.Kind {
	case reqctx.KindAgent:
		// The operation is open to service principals, so agents are refused
		// here rather than by x-agent: false, which would refuse
		// integrations too (as importBankStatements, ADR-0047).
		return Entry{}, nil, problem.Forbidden("identity.agent_not_allowed",
			"Journal entries are posted by people and integrations; an agent parks its proposal (POST /v1/parked-journal-entries).")
	case reqctx.KindService:
	default:
		je, err = e.postEntry(ctx, in, e.withinLimit)
		return je, nil, err
	}
	v, err := e.prepare(ctx, tx, in)
	if err != nil {
		return Entry{}, nil, err
	}
	over, err := e.overServiceLimit(ctx, tx, v, in)
	if err != nil {
		return Entry{}, nil, err
	}
	if over != nil {
		pe, err := parkPrepared(ctx, tx, p, v, in, over)
		if err != nil {
			return Entry{}, nil, err
		}
		return Entry{}, &pe, nil
	}
	je, err = e.postPrepared(ctx, tx, p, v, in, nil)
	return je, nil, err
}

// overServiceLimit measures a service principal's entry as approvals
// measures a person's against the manual entry approval limit: its debits,
// converted exactly into its company's currency at the average rate of its
// posting date (the rate it would post at), unrounded, so 1000.004 does not
// pass a limit of 1000.00 (F13). The limit is the company's
// service_posting_limit, its manual_entry_approval_limit when it has none
// (config.Company.ServiceLimit), and 0 when it has neither or the
// configuration lacks the company. It returns nil within the limit. A
// period that is not open refuses the entry first, as withinLimit does: a
// person could not post it either.
func (e *Endpoints) overServiceLimit(ctx context.Context, tx pgx.Tx, v prepared, in NewEntry) (*ParkLimit, error) {
	if _, _, _, err := postingPeriod(ctx, tx, v.le, v.choice.ledgers, in.PostingDate); err != nil {
		return nil, err
	}
	debits, err := debitTotal(v.lines)
	if err != nil {
		return nil, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: service posting limit of company %s: %w", v.le.Code, err)
	}
	text, exp := "0", v.exps[v.le.LocalCurrency]
	if co := doc.Company(v.le.Code); co != nil && co.ServiceLimit() != "" {
		text = co.ServiceLimit()
	}
	// Validated with the company's digits (config: the company's currency
	// is its legal entity's local currency, F14), so it reads exactly.
	limit, err := money.ParseDecimal(text, exp)
	if err != nil {
		return nil, fmt.Errorf("ledger: service posting limit of company %s: %w", v.le.Code, err)
	}
	amount, err := Rates{LegalEntities: e.LegalEntities}.ExactToCompany(ctx, v.le.ID, in.Currency, in.PostingDate, debits)
	if err != nil {
		return nil, err
	}
	if amount.Cmp(money.Rat(limit, exp)) <= 0 {
		return nil, nil
	}
	return &ParkLimit{Kind: ServicePostingLimit, Debits: debits, DebitsCurrency: in.Currency, DebitsExp: v.expTC,
		Limit: limit, LimitCurrency: v.le.LocalCurrency, LimitExp: exp}, nil
}
