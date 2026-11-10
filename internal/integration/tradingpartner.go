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
// other lines [M]. So the load does not post RASSC line by line: it links
// the supplier it creates to the legal entity the vendor's trading
// partner is (partner's group company), and the ledger derives every
// posting's trading partners from its suppliers and gives a document's
// other lines its vendor line's, as it does for every posting on the
// platform (ledger.deriveTradingPartners). A VBUND is a company ID, not a
// company code: the company code assigned to it (T001-RCOMP, read from
// every company code of the source, sap.Extract.CompanyCodes) names the
// legal entity, by its code. One that names none (no company code, several,
// or a company code that is no legal entity of the tenant) is a warning of
// the quality gate, never a refusal: a group has companies that are not on
// the platform, and the supplier then loads unlinked, its documents
// without a trading partner, as a consolidation run lists them. The gate
// reads RASSC only to report where SAP's lines say something else than
// the load gives them, and the reconciliation compares the open payables
// per trading partner.

// groupCompany is what a trading partner's company ID names on the
// platform: the company code T001 assigns to it and that code's legal
// entity, or why it names none.
type groupCompany struct {
	code string
	id   uuid.UUID
	why  string
}

// groupCompanies resolves the trading partners the extract's vendors
// name, each once, by company ID.
func (e *Endpoints) groupCompanies(ctx context.Context, ext *sap.Extract) (map[string]groupCompany, error) {
	out := map[string]groupCompany{}
	for _, v := range ext.LFA1 {
		id := sap.CompanyID(v.VBUND)
		if _, seen := out[id]; seen || id == "" {
			continue
		}
		gc, err := e.groupCompany(ctx, ext.CompanyCodes[id])
		if err != nil {
			return nil, fmt.Errorf("integration: the legal entity of trading partner %s: %w", v.VBUND, err)
		}
		out[id] = gc
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
// trading partners: an extract that cannot carry them, each vendor whose
// trading partner names no legal entity, and the journal lines whose own
// trading partner the load does not give them (lineTradingPartners).
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

// lineTradingPartners reads the lines' own trading partner (ACDOCA-RASSC),
// which the load does not post, and warns where it differs from what the
// load gives the line: per company code and vendor, the vendor lines
// carrying another one than the vendor's master, which load with the
// master's (a vendor linked, relinked or unlinked since the posting); and
// per company code, the lines carrying one in documents without a vendor
// line, which load with none. An extract whose lines carry none at all
// (it may not hold the field) reports nothing.
func (q *qualityCheck) lineTradingPartners() {
	if !slices.ContainsFunc(q.e.ACDOCA, func(l sap.ACDOCA) bool { return sap.CompanyID(l.RASSC) != "" }) {
		return
	}
	differing := map[string]*partnerLines{}
	withVendor, lines, documents := map[string]bool{}, map[string]int{}, map[string]map[string]bool{}
	for _, l := range q.e.ACDOCA {
		if l.LIFNR != "" {
			withVendor[documentKey(l)] = true
		}
	}
	for _, l := range q.e.ACDOCA {
		own := sap.CompanyID(l.RASSC)
		switch {
		case l.LIFNR != "":
			if own != sap.CompanyID(q.vendor[l.LIFNR].VBUND) {
				key := l.RBUKRS + "/" + l.LIFNR
				if differing[key] == nil {
					differing[key] = &partnerLines{company: l.RBUKRS, vendor: l.LIFNR, others: map[string]int{}}
				}
				differing[key].others[strings.TrimSpace(l.RASSC)]++
			}
		case own != "" && !withVendor[documentKey(l)]:
			if documents[l.RBUKRS] == nil {
				documents[l.RBUKRS] = map[string]bool{}
			}
			lines[l.RBUKRS]++
			documents[l.RBUKRS][documentKey(l)] = true
		}
	}
	for _, key := range sortedKeys(differing) {
		q.differingVendorLines(*differing[key])
	}
	for _, cc := range sortedKeys(lines) {
		q.add(SevWarning, "ACDOCA", cc, "journal lines of company %s that carry a trading partner (ACDOCA-RASSC) in documents without a vendor line "+
			"load without one: lines %d, documents %d. The load derives a document's trading partner from its vendor line, so a consolidation run "+
			"lists their balances as no_trading_partner until they are reposted with one", cc, lines[cc], len(documents[cc]))
	}
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
// no vendor of the extract names, by its id alone, as the API shows a
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
