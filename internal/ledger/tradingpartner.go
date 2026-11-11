package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
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
// (journal_line_trading_partner, 000219). The balance projection's trigger
// adds the lines with one to ledger.trading_partner_balance (000219), which
// consolidation reads (T68d). Reads name each trading partner by its
// company code and its name as it is now, to a caller with access to that
// company only, after a journal entry's ETag is taken, so neither renaming
// a company nor who reads it changes an entry's ETag.

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

// deriveTradingPartners gives the lines of an entry about to be posted
// their trading partners from their business partners (ADR-0059 §4,
// T68c), in the one place every posting but a reversal passes (postAll):
// a line naming a partner that is a group company takes that company,
// unless it is the posting company le (no intercompany transaction); a
// line naming another trading partner than its partner's group company is
// refused (422 ledger.trading_partner_conflict); a line that names one of
// its own, matching or with a partner linked to none, keeps it (T68b).
// Lines offsetting open items keep their items' (Line.offsets), so a
// clearing still nets each item's trading partner when its partner was
// linked or unlinked since. Then, when every partner line names the same
// trading partner, the entry's other lines that name none take it too, so
// an intercompany invoice's expense and tax lines carry its supplier
// line's; not in a collective entry (propagate false, Posting.Collective),
// whose other lines belong to no partner line. Each partner is asked once.
func (e *Endpoints) deriveTradingPartners(ctx context.Context, le uuid.UUID, lines []Line, propagate bool) error {
	groups := map[uuid.UUID]*uuid.UUID{}
	for i := range lines {
		l := &lines[i]
		if l.PartnerID == nil || l.offsets {
			continue
		}
		group, seen := groups[*l.PartnerID]
		if !seen {
			var err error
			if group, err = e.Partners.GroupCompany(ctx, *l.PartnerID); err != nil {
				return fmt.Errorf("ledger: group company of partner %s on line %d: %w", *l.PartnerID, i+1, err)
			}
			groups[*l.PartnerID] = group
		}
		switch {
		case group == nil:
		case l.TradingPartnerID == nil:
			if *group != le {
				l.TradingPartnerID = group
			}
		case *l.TradingPartnerID != *group:
			return problem.Unprocessable("ledger.trading_partner_conflict",
				fmt.Sprintf("Line %d names a trading partner other than the company its business partner is; leave it out to take the partner's.", i+1),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].trading_partner_id", i), Code: "conflict",
					Message: "the business partner's group company, or none"})
		}
	}
	if propagate {
		propagateTradingPartner(lines)
	}
	return nil
}

// propagateTradingPartner gives the lines naming no partner and no
// trading partner, and offsetting no item, the trading partner every
// partner line names, when they all name the same one (ADR-0059 §4).
func propagateTradingPartner(lines []Line) {
	var shared *uuid.UUID
	for _, l := range lines {
		switch {
		case l.PartnerID == nil:
		case l.TradingPartnerID == nil, shared != nil && *shared != *l.TradingPartnerID:
			return
		default:
			shared = l.TradingPartnerID
		}
	}
	if shared == nil {
		return
	}
	for i := range lines {
		if lines[i].PartnerID == nil && lines[i].TradingPartnerID == nil && !lines[i].offsets {
			lines[i].TradingPartnerID = shared
		}
	}
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

// nameTradingPartners gives each trading partner the caller may act in
// (ADR-0010) its company code and its name as tenancy has them now,
// reading each legal entity once; one outside the caller's access keeps
// its id alone (product owner 2026-11-05, ADR-0059 As built): the line is
// a record of the entry's company, the other company's master data is
// not the caller's to read. The caller's access is read only when a
// trading partner is there to name. A line's trading partner was a legal
// entity of the tenant when it was posted, and legal entities are never
// deleted, so one tenancy does not know is an error.
func (e *Endpoints) nameTradingPartners(ctx context.Context, partners []*gen.OptTradingPartner) error {
	var access *authz.Access
	names := map[uuid.UUID]tenancy.Entity{}
	for _, p := range partners {
		tp, ok := p.Get()
		if !ok {
			continue
		}
		if access == nil {
			a, err := authz.Companies(ctx)
			if err != nil {
				return fmt.Errorf("ledger: name trading partners: %w", err)
			}
			access = &a
		}
		if !access.Allows(tp.ID) {
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
		tp.Code, tp.Name = gen.NewOptString(le.Code), gen.NewOptString(le.Name)
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
