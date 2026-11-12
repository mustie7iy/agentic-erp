package integration

import (
	"context"
	"maps"
	"slices"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The reconciliation of account balances by trading partner (ADR-0059 As
// built, the migration leaf; po:2026-11-10). The open payables by trading
// partner prove the vendor lines (reconciler.tradingPartners); this
// control proves every other line: the lines the load posts without a
// supplier, per account and trading partner, SAP's under the trading
// partner the load gives each line, its own (ACDOCA-RASSC, the SAP
// specialist's A2 [H]; target.ownPartner) or else its document's vendor
// lines' (derivedPartners), against the platform's journal lines as they
// carry one, which a consolidation run reads. In balances mode it proves
// the opening entry's balances by trading partner.

// givenPartner is the key of the trading partner the load gives a line it
// posts without a supplier, and whether it gives one: the one the line
// names itself, or else the one its document's vendor lines share (none
// in a balances run's opening entry, derivedPartners).
func (rc *reconciler) givenPartner(l sap.ACDOCA) (string, bool) {
	if gc, ok := rc.t.ownPartner(l); ok {
		return "trading partner " + gc.code, true
	}
	if id := rc.derived[documentKey(l)]; id != "" {
		return "trading partner " + rc.t.partners[id].code, true
	}
	return "", false
}

// sapPartnerBalances adds up the company's journal lines the load posts
// without a supplier and with a trading partner, per "account <number>
// <trading partner key>". A line whose amount does not parse is left
// out: the journal's control refused it (reconciler.journal).
func (rc *reconciler) sapPartnerBalances(s companyScope) map[string]money.Minor {
	terms := map[string][]money.Minor{}
	for _, l := range rc.ext.ACDOCA {
		if l.RBUKRS != s.cc || rc.t.supplierLine(l) {
			continue
		}
		partner, ok := rc.givenPartner(l)
		if !ok {
			continue
		}
		if amt, err := rc.t.amount(l.HSL, s.cur); err == nil {
			key := "account " + sap.Alpha(l.RACCT) + " " + partner
			terms[key] = append(terms[key], amt)
		}
	}
	return rc.totals("ACDOCA", s.cc, terms, func(k string) string { return "the lines of " + k })
}

// partnerBalances compares the balances by account and trading partner of
// the accounts that are no supplier reconciliation accounts. The checks
// are shown when either side has any, so a scope without trading partners
// reports as before; a balance that nets to zero on both sides is no
// difference.
func (rc *reconciler) partnerBalances(ctx context.Context, s companyScope) error {
	sapSide := rc.sapPartnerBalances(s)
	balances, err := rc.e.Reports.BalancesByTradingPartner(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	if len(sapSide) == 0 && len(balances) == 0 {
		return nil
	}
	platform := map[string]money.Minor{}
	for _, b := range balances {
		platform["account "+b.Account+" "+rc.tradingPartnerKey(b.TradingPartner)] = b.Amount
	}
	d := diffs(s.cc, sapSide, platform, s.format)
	rc.rec.Diffs = append(rc.rec.Diffs, d...)
	sapTotal := rc.total("ACDOCA", s.cc, "the account balances by trading partner of company "+s.cc, slices.Collect(maps.Values(sapSide)))
	platformTotal := rc.total("platform", s.cc, "the platform's account balances by trading partner of company "+s.cc, slices.Collect(maps.Values(platform)))
	rc.add(s.cc, "Account balances by trading partner total", s.format(sapTotal), s.format(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, "Account balances by trading partner: balances with differences", "0", strconv.Itoa(len(d)), len(d) == 0)
	return nil
}
