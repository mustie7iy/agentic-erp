package integration

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// SAP's trading partner on migrated intercompany documents (ADR-0059 §4
// and As built, the migration leaf; the SAP specialist, po:2026-11-09, A2).
// A vendor that is a company of the group names it in its master
// (LFA1-VBUND [H]), and SAP gives the vendor's lines the same trading
// partner (ACDOCA-RASSC [H]) but does not guarantee it on a document's
// other lines [M]. So the load takes it from two places. It links the
// supplier it creates to the legal entity the vendor's trading partner is
// (partner's group company), and the ledger derives every posting's
// trading partners from its suppliers and gives a document's other lines
// its vendor line's, as it does for every posting on the platform
// (ledger.deriveTradingPartners). And a line the load posts without a
// supplier is posted with the trading partner it names itself (RASSC),
// when that is a legal entity other than the line's company
// (target.ownPartner; po:2026-11-10): so an intercompany document without
// a vendor line, a loan or a recharge between G/L accounts, carries its
// partner, and a line SAP left without one still takes its document's
// vendor line's. A balances run's opening entry groups each account's
// balance by the trading partner its lines name the same way
// (loader.openingBalances), so an intercompany G/L balance arrives with
// its partner; its lines that name none take none, the entry gathering
// lines of no one document (ledger.Posting.Collective). A vendor line keeps its supplier's: the platform derives
// a partner line's trading partner from its business partner and refuses
// another. A VBUND or RASSC is a company ID, not a company code: the
// company code assigned to it (T001-RCOMP, read from every company code of
// the source, sap.Extract.CompanyCodes) names the legal entity, by its
// code. One that names none (no company code, several, or a company code
// that is no legal entity of the tenant) is a warning of the quality gate,
// never a refusal: a group has companies that are not on the platform, and
// the supplier then loads unlinked and the line without a trading partner
// of its own, as a consolidation run lists them. The reconciliation
// compares the open payables per trading partner and the other accounts'
// balances per account and trading partner.

// groupCompany is what a trading partner's company ID names on the
// platform: the company code T001 assigns to it and that code's legal
// entity, or why it names none.
type groupCompany struct {
	code string
	id   uuid.UUID
	why  string
}

// groupCompanies resolves the trading partners the extract names, each
// once, by company ID: its vendors' (LFA1-VBUND) and its journal lines'
// (ACDOCA-RASSC).
func (e *Endpoints) groupCompanies(ctx context.Context, ext *sap.Extract) (map[string]groupCompany, error) {
	out := map[string]groupCompany{}
	resolve := func(field, raw string) error {
		id := sap.CompanyID(raw)
		if _, seen := out[id]; seen || id == "" {
			return nil
		}
		gc, err := e.groupCompany(ctx, ext.CompanyCodes[id])
		if err != nil {
			return fmt.Errorf("integration: the legal entity of trading partner %s (%s): %w", strings.TrimSpace(raw), field, err)
		}
		out[id] = gc
		return nil
	}
	for _, v := range ext.LFA1 {
		if err := resolve("LFA1-VBUND", v.VBUND); err != nil {
			return nil, err
		}
	}
	for _, l := range ext.ACDOCA {
		if err := resolve("ACDOCA-RASSC", l.RASSC); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// groupCompany is the legal entity of the one company code assigned to a
// company ID. The legal entity is taken whatever its status and the
// caller's access to it, as partner takes a group company (ADR-0059 As
// built, T68c).
func (e *Endpoints) groupCompany(ctx context.Context, codes []string) (groupCompany, error) {
	switch len(codes) {
	case 0:
		return groupCompany{why: "is the company ID of no company code in the extract (T001-RCOMP)"}, nil
	case 1:
	default:
		return groupCompany{why: "is the company ID of several company codes (" + strings.Join(codes, ", ") + "), which no one legal entity is"}, nil
	}
	le, ok, err := e.LegalEntities.EntityByCode(ctx, codes[0])
	if err != nil {
		return groupCompany{}, err
	}
	if !ok {
		return groupCompany{code: codes[0], why: "is company code " + codes[0] + ", which is no legal entity of the tenant"}, nil
	}
	return groupCompany{code: codes[0], id: le.ID}, nil
}

// groupCompanyOf is the legal entity a vendor's trading partner is, nil
// when the vendor names none or one that is none of the tenant's.
func (t target) groupCompanyOf(v sap.LFA1) *uuid.UUID {
	gc, ok := t.partners[sap.CompanyID(v.VBUND)]
	if !ok || gc.why != "" {
		return nil
	}
	return &gc.id
}

// tradingPartners reports what the load does not take over of SAP's
// trading partners: an extract that cannot carry its vendors', each vendor
// whose trading partner names no legal entity, and the journal lines whose
// own trading partner the load does not give them (lineTradingPartners).
func (q *qualityCheck) tradingPartners() {
	if q.e.NoTradingPartners && len(q.e.LFA1) > 0 {
		q.add(SevWarning, "LFA1", "VBUND", "the extract carries no trading partner: the OData mapping reads neither LFA1-VBUND nor ACDOCA-RASSC. "+
			"No supplier is linked to a company of the group, so the documents of intercompany vendors load without a trading partner; "+
			"a file extract carries both")
	}
	for _, v := range q.e.LFA1 {
		if gc, ok := q.t.partners[sap.CompanyID(v.VBUND)]; ok && gc.why != "" {
			q.add(SevWarning, "LFA1", v.LIFNR, "vendor %q names trading partner %s (LFA1-VBUND), which %s: the supplier loads linked to no "+
				"company of the group and its documents without a trading partner", v.NAME1, strings.TrimSpace(v.VBUND), gc.why)
		}
	}
	q.lineTradingPartners()
}

// partnerLines counts, for one vendor in one company code, the vendor
// lines whose own trading partner (ACDOCA-RASSC) is not its master's, by
// that trading partner ("" for none).
type partnerLines struct {
	company, vendor string
	others          map[string]int
}

// lineTradingPartners reads the lines' own trading partner (ACDOCA-RASSC)
// and warns where the load gives the line another or none: per company
// code and vendor, the vendor lines carrying another one than the
// vendor's master, which load with the master's (a vendor linked,
// relinked or unlinked since the posting); and the other lines whose own
// the load cannot post (otherLines). An extract whose lines carry none at
// all (it may not hold the field) reports nothing.
func (q *qualityCheck) lineTradingPartners() {
	if !slices.ContainsFunc(q.e.ACDOCA, func(l sap.ACDOCA) bool { return sap.CompanyID(l.RASSC) != "" }) {
		return
	}
	differing := map[string]*partnerLines{}
	for _, l := range q.e.ACDOCA {
		if !q.vendorLine(l) || sap.CompanyID(l.RASSC) == sap.CompanyID(q.vendor[l.LIFNR].VBUND) {
			continue
		}
		key := l.RBUKRS + "/" + l.LIFNR
		if differing[key] == nil {
			differing[key] = &partnerLines{company: l.RBUKRS, vendor: l.LIFNR, others: map[string]int{}}
		}
		differing[key].others[strings.TrimSpace(l.RASSC)]++
	}
	for _, key := range sortedKeys(differing) {
		q.differingVendorLines(*differing[key])
	}
	q.otherLines()
}

// vendorLine reports whether the load posts the line with its vendor's
// supplier: a line naming a vendor on a supplier reconciliation account
// (journalPosting, openingLines). A vendor named on another line is not
// posted, so the line takes its trading partner as a line without one.
func (q *qualityCheck) vendorLine(l sap.ACDOCA) bool {
	return l.LIFNR != "" && q.t.charts[l.RBUKRS][sap.Alpha(l.RACCT)].ReconciliationKind == "supplier"
}

// lineCount counts lines and the documents they are in.
type lineCount struct {
	lines     int
	documents map[string]bool
}

func (c *lineCount) add(document string) {
	if c.documents == nil {
		c.documents = map[string]bool{}
	}
	c.lines++
	c.documents[document] = true
}

// noCompanyCode is why a company ID no company code of the extract is
// assigned to names no legal entity.
const noCompanyCode = "is the company ID of no company code in the extract (T001-RCOMP)"

// ownPartner is the legal entity a journal line names as its trading
// partner itself (ACDOCA-RASSC), which the load posts a line without a
// supplier with, in a document's entry or in an opening entry's balance
// (po:2026-11-10); or, not ok, why it does not (why): the
// company ID names no legal entity of the tenant, or the line's own
// company, which is no trading partner of its lines (the ledger's rule);
// why is empty for a line that names none.
func (t target) ownPartner(l sap.ACDOCA) (gc groupCompany, ok bool) {
	id := sap.CompanyID(l.RASSC)
	if id == "" {
		return groupCompany{}, false
	}
	gc, ok = t.partners[id]
	switch {
	case !ok:
		return groupCompany{why: noCompanyCode}, false
	case gc.why != "":
		return gc, false
	case gc.id == t.entities[l.RBUKRS].ID:
		return groupCompany{code: gc.code, why: "is the line's own company, which is no trading partner of its own lines"}, false
	}
	return gc, true
}

// otherLines warns of the lines the load posts without a supplier that
// name a trading partner in SAP which the load does not give them: per
// company code and trading partner, those naming one that is no legal
// entity of the tenant or the line's own company, which load without a
// trading partner of their own. A line naming a legal entity loads with
// it, in a document's entry or in an opening entry's balance, and one SAP
// left without loads with its document's vendor lines' where the document
// posts as an entry (A2): neither is reported.
func (q *qualityCheck) otherLines() {
	counts, why := map[string]*lineCount{}, map[string]string{}
	for _, l := range q.e.ACDOCA {
		gc, ok := q.t.ownPartner(l)
		if q.t.supplierLine(l) || ok || gc.why == "" {
			continue
		}
		key := l.RBUKRS + "/" + strings.TrimSpace(l.RASSC)
		if counts[key] == nil {
			counts[key], why[key] = &lineCount{}, gc.why
		}
		counts[key].add(documentKey(l))
	}
	// What such a line loads with, as the run's mode has it: a balances
	// run's opening entry gives its account balances none of a document's.
	outcome := "with the one their document's vendor lines share, or without one where it has none or those name different ones"
	if q.t.balances {
		outcome = "without one in the opening entry's balances, and in a document that posts on its own (a down payment's, an item in a foreign " +
			"currency) with the one its vendor lines share, or without one where those name different ones or none"
	}
	for _, key := range sortedKeys(counts) {
		cc, partner, _ := strings.Cut(key, "/")
		q.add(SevWarning, "ACDOCA", key, "journal lines of company %s name trading partner %s (ACDOCA-RASSC), which %s: lines %d, documents %d. "+
			"They load without a trading partner of their own: %s, and a consolidation run lists a balance without one as no_trading_partner",
			cc, partner, why[key], counts[key].lines, len(counts[key].documents), outcome)
	}
}

// derivedPartners are, per document, the company ID of the trading
// partner the ledger gives the lines naming no vendor and no trading
// partner of their own, "" for none: the one every vendor line's supplier
// is linked to, another company than the document's (the ledger's rule,
// deriveTradingPartners). In balances mode only a document that posts as
// an entry of its own (downPaymentDocs, foreign) gives its other lines
// one: the opening entry's balances take none (ledger.Posting.Collective).
// It is a prediction from SAP's vendor masters, for the SAP side of the
// reconciliation, not the ledger's rule to the letter: the ledger reads
// the group company the platform's supplier has, which for a supplier an
// earlier run loaded may be another than its VBUND now names, and the
// reconciliation shows that difference.
func derivedPartners(e *sap.Extract, t target, foreign map[string]string) map[string]string {
	derived, withVendor := map[string]string{}, map[string]bool{}
	own, vbund := downPaymentDocs(e), map[string]string{}
	for _, v := range e.LFA1 {
		vbund[v.LIFNR] = sap.CompanyID(v.VBUND)
	}
	for _, l := range e.ACDOCA {
		if l.LIFNR == "" || !t.supplierLine(l) {
			continue
		}
		key, id := documentKey(l), vbund[l.LIFNR]
		if gc, ok := t.partners[id]; !ok || gc.why != "" || gc.id == t.entities[l.RBUKRS].ID || t.balances && own[key] == nil && foreign[key] == "" {
			id = ""
		}
		if seen := withVendor[key]; seen && derived[key] != id {
			id = ""
		}
		derived[key], withVendor[key] = id, true
	}
	return derived
}

// differingVendorLines warns of one vendor's lines in a company code whose
// own trading partner is not its master's.
func (q *qualityCheck) differingVendorLines(p partnerLines) {
	var others []string
	total := 0
	for _, own := range sortedKeys(p.others) {
		name := own
		if own == "" {
			name = "none"
		}
		others = append(others, name+" on "+strconv.Itoa(p.others[own]))
		total += p.others[own]
	}
	master := strings.TrimSpace(q.vendor[p.vendor].VBUND)
	if master == "" {
		master = "none"
	}
	q.add(SevWarning, "ACDOCA", p.company+"/"+p.vendor, "vendor lines of vendor %s %q in company %s carry another trading partner (ACDOCA-RASSC) "+
		"than the vendor's master (LFA1-VBUND %s): lines %d (%s). They load with the master's: the platform derives a line's trading partner "+
		"from its business partner", p.vendor, q.vendor[p.vendor].NAME1, p.company, master, total, strings.Join(others, ", "))
}

// tradingPartnerKey names a trading partner in a reconciliation: by the
// SAP company code its company ID is assigned to, or, for a legal entity
// the extract does not name, by its id alone, as the API shows a
// trading partner to a caller who may not act in that company (product
// owner 2026-11-05).
func (rc *reconciler) tradingPartnerKey(id uuid.UUID) string {
	for _, gc := range rc.t.partners {
		if gc.why == "" && gc.id == id {
			return "trading partner " + gc.code
		}
	}
	return "trading partner " + id.String()
}

// sapTradingPartner is the key of the trading partner the load gives an
// open item of the vendor in the company, and whether it gives one: the
// vendor's, unless it is the company itself, which is no intercompany
// item (the ledger's rule).
func (rc *reconciler) sapTradingPartner(s companyScope, lifnr string) (string, bool) {
	gc, ok := rc.t.partners[rc.partnerOf[lifnr]]
	if !ok || gc.why != "" || gc.id == s.le {
		return "", false
	}
	return "trading partner " + gc.code, true
}

// tradingPartners compares the open payables per trading partner,
// ordinary items and down payments together: SAP's as the load derives
// them, each open vendor item under its vendor's trading partner, and the
// platform's as the journal lines carry them, which a consolidation run
// reads. The checks are shown when either side has any, so a scope without
// intercompany vendors reports as before.
func (rc *reconciler) tradingPartners(ctx context.Context, s companyScope, sapOpen map[string]money.Minor) error {
	open, err := rc.e.Reports.OpenPayablesByTradingPartner(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	if len(sapOpen) == 0 && len(open) == 0 {
		return nil
	}
	platform := map[string]money.Minor{}
	for id, amt := range open {
		platform[rc.tradingPartnerKey(id)] = amt
	}
	d := diffs(s.cc, sapOpen, platform, s.format)
	rc.rec.Diffs = append(rc.rec.Diffs, d...)
	sapTotal := rc.total("ACDOCA", s.cc, "the open payables by trading partner of company "+s.cc, slices.Collect(maps.Values(sapOpen)))
	platformTotal := rc.total("platform", s.cc, "the platform's open payables by trading partner of company "+s.cc, slices.Collect(maps.Values(platform)))
	rc.add(s.cc, "Open payables by trading partner total", s.format(sapTotal), s.format(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, "Open payables by trading partner: trading partners with differences", "0", strconv.Itoa(len(d)), len(d) == 0)
	return nil
}
