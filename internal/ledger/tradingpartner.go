package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Trading partners on journal lines (ADR-0059 §4, T68b). A line's trading
// partner is the tenant's company on the other side of an intercompany
// transaction, as SAP's universal journal carries RASSC: entered on a
// manual, parked or template line, given by a posting context
// (PostingLine.TradingPartner, Posting.TradingPartner), copied by a
// reversal from the line it reverses and by a clearing from the item each
// line offsets; a clearing's difference lines take its items' when they all
// name the same one. It is a legal entity of the tenant other than the
// line's own company. Tenancy is another context, so no foreign key holds
// the first half (02 §2.1): the ledger asks tenancy before it posts or
// parks a line that names one, and the database holds the second
// (journal_line_trading_partner, 000216). The balance projection's trigger
// adds the lines with one to ledger.trading_partner_balance (000216), which
// consolidation reads (T68d). Reads name each trading partner by its
// company code and its name as it is now, after a journal entry's ETag is
// taken, so renaming a company changes no entry's ETag.

func init() {
	problem.RegisterConstraint("journal_line_trading_partner", func() *problem.Problem {
		return tradingPartnerInvalid("lines.trading_partner_id", "A line's trading partner is another company of the tenant, never the entry's own.", true)
	})
}

// checkTradingPartners refuses a line whose trading partner is not a legal
// entity of the tenant, or is the posting company le (422
// ledger.trading_partner_invalid on the line's field). Each trading partner
// is looked up once.
func (e *Endpoints) checkTradingPartners(ctx context.Context, le uuid.UUID, lines []Line) error {
	known := map[uuid.UUID]bool{le: false}
	for i, l := range lines {
		if l.TradingPartnerID == nil {
			continue
		}
		id := *l.TradingPartnerID
		ok, seen := known[id]
		if !seen {
			_, found, err := e.LegalEntities.Entity(ctx, id)
			if err != nil {
				return fmt.Errorf("ledger: trading partner %s of line %d: %w", id, i+1, err)
			}
			ok, known[id] = found, found
		}
		if ok {
			continue
		}
		field := fmt.Sprintf("lines[%d].trading_partner_id", i)
		if id == le {
			return tradingPartnerInvalid(field, fmt.Sprintf("Line %d names the entry's own company as its trading partner; a trading partner is another company of the tenant.", i+1), true)
		}
		return tradingPartnerInvalid(field, fmt.Sprintf("Line %d names a trading partner that is no legal entity of the tenant.", i+1), false)
	}
	return nil
}

// tradingPartnerInvalid refuses the trading partner in field: the entry's
// own company (own), or none of the tenant's.
func tradingPartnerInvalid(field, detail string, own bool) *problem.Problem {
	code := "unknown"
	if own {
		code = "own_company"
	}
	return problem.Unprocessable("ledger.trading_partner_invalid", detail,
		problem.FieldError{Field: field, Code: code, Message: "a legal entity of the tenant other than the entry's"})
}

// tradingPartnerRef is a line's trading partner as entryToAPI and
// linesToAPI give it: its id alone, until nameTradingPartners adds its code
// and name; unset for none.
func tradingPartnerRef(id *uuid.UUID) gen.OptTradingPartner {
	if id == nil {
		return gen.OptTradingPartner{}
	}
	return gen.NewOptTradingPartner(gen.TradingPartner{ID: *id})
}

// nameTradingPartners gives each trading partner its company code and its
// name as tenancy has them now, reading each legal entity once. A line's
// trading partner was a legal entity of the tenant when it was posted, and
// legal entities are never deleted, so one tenancy does not know is an
// error.
func (e *Endpoints) nameTradingPartners(ctx context.Context, partners []*gen.OptTradingPartner) error {
	names := map[uuid.UUID]tenancy.Entity{}
	for _, p := range partners {
		tp, ok := p.Get()
		if !ok {
			continue
		}
		le, seen := names[tp.ID]
		if !seen {
			var found bool
			var err error
			if le, found, err = e.LegalEntities.Entity(ctx, tp.ID); err != nil {
				return fmt.Errorf("ledger: name trading partner %s: %w", tp.ID, err)
			}
			if !found {
				return fmt.Errorf("ledger: name trading partner %s: no legal entity of the tenant has the id", tp.ID)
			}
			names[tp.ID] = le
		}
		tp.Code, tp.Name = le.Code, le.Name
		p.SetTo(tp)
	}
	return nil
}

// entryTradingPartners are the trading partners of the entries' lines, to
// name.
func entryTradingPartners(reps ...*gen.JournalEntry) []*gen.OptTradingPartner {
	var out []*gen.OptTradingPartner
	for _, rep := range reps {
		for i := range rep.Lines {
			out = append(out, &rep.Lines[i].TradingPartner)
		}
	}
	return out
}

// lineTradingPartners are the trading partners of kept lines (a parked
// entry's, a template's), to name.
func lineTradingPartners(lines []gen.ParkedJournalLine) []*gen.OptTradingPartner {
	out := make([]*gen.OptTradingPartner, len(lines))
	for i := range lines {
		out[i] = &lines[i].TradingPartner
	}
	return out
}

// sharedTradingPartner is the trading partner every item names, or nil
// when one names none or two name different ones: what a clearing's
// difference lines take (ADR-0059 §4).
func sharedTradingPartner(items []item) *uuid.UUID {
	var shared *uuid.UUID
	for i, it := range items {
		switch {
		case it.tradingPartner == nil, i > 0 && *it.tradingPartner != *shared:
			return nil
		case i == 0:
			shared = it.tradingPartner
		}
	}
	return shared
}

// withTradingPartner gives the lines that name no trading partner tp.
func withTradingPartner(lines []Line, tp *uuid.UUID) {
	for i := range lines {
		if lines[i].TradingPartnerID == nil {
			lines[i].TradingPartnerID = tp
		}
	}
}
