package integration

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Documents in a foreign document currency (ADR-0005 As built, product
// owner 2026-10-31). An SAP document whose lines are all in one document
// currency (ACDOCA RWCUR) other than its company's posts in that currency,
// as the platform posts a document of its own in it: each line's document
// amount (WSL) as its transaction amount and its company code amount
// (HSL) kept as its local amount, so an open vendor item or down payment
// is an item in its own currency that a payment or final invoice in that
// currency clears and a revaluation values, and the trial balance and the
// open payables stay SAP's in company currency. The entry records the
// rate SAP's amounts imply (sourceRate): the extract does not carry the
// document's exchange rate.
//
// A posting has one currency and a clearing clears items of one, so the
// documents SAP cleared together post in one currency or not in a foreign
// one at all: in documents mode a document posts in its foreign currency
// when every document cleared together with it, directly or through
// others, is in that currency too (clearingComponents); a component that
// mixes currencies posts in company currency, as before, and the quality
// gate refuses an open item in it rather than load it in the wrong
// currency. In balances mode, where SAP's clearings are not taken over,
// the documents holding an open item in a foreign currency post whole on
// their own beside the opening entry, as down payments do (T72c), and the
// others are part of the opening balances.

// documentCurrencies is each document's currency: the one its lines'
// RWCUR agree on, or "" when they do not, or one of them carries none. A
// document none of whose lines has a document amount (WSL) is in its
// company's currency: it has nothing in another to post.
func documentCurrencies(e *sap.Extract, t target) map[string]string {
	out, amounts := map[string]string{}, map[string]bool{}
	for _, l := range e.ACDOCA {
		key := documentKey(l)
		c, seen := out[key]
		switch {
		case !seen:
			out[key] = l.RWCUR
		case c != l.RWCUR:
			out[key] = ""
		}
		amounts[key] = amounts[key] || !t.zero(l.WSL, l.RWCUR)
	}
	for key := range out {
		if !amounts[key] {
			out[key] = t.localCurrency(key)
		}
	}
	return out
}

// supplierLine reports whether a journal line is on a supplier
// reconciliation account of its legal entity.
func (t target) supplierLine(l sap.ACDOCA) bool {
	return t.charts[l.RBUKRS][sap.Alpha(l.RACCT)].ReconciliationKind == "supplier"
}

// localCurrency is the local currency of the company of a document key.
func (t target) localCurrency(key string) string {
	company, _, _ := strings.Cut(key, "/")
	return t.entities[company].LocalCurrency
}

// hasDocumentAmount reports whether a line has, or may have, an amount in
// its document currency (WSL), read without that currency, which may be
// missing or unknown: one not zero, or not a number. A line that carries
// none has none when it is in company currency; in another currency, or
// none (an OData extract carries neither yet), it cannot be told, so an
// open item of no company code amount is refused for that (foreignItem)
// rather than left out.
func (q *qualityCheck) hasDocumentAmount(l sap.ACDOCA) bool {
	if l.WSL == "" {
		return l.RWCUR != q.currency[l.RBUKRS]
	}
	r, err := money.ParseDecimalRat(l.WSL)
	return err != nil || r.Sign() != 0
}

// zero reports whether an amount parses as zero in the currency.
func (t target) zero(amount, currency string) bool {
	m, err := t.amount(amount, currency)
	return err == nil && m == 0
}

// openDocuments are the documents holding an open supplier item: a line
// on a supplier reconciliation account SAP has not cleared, with an
// amount in either currency.
func openDocuments(e *sap.Extract, t target) map[string]bool {
	out := map[string]bool{}
	for _, l := range e.ACDOCA {
		key := documentKey(l)
		if l.AUGBL == "" && t.supplierLine(l) && (!t.zero(l.HSL, t.localCurrency(key)) || !t.zero(l.WSL, l.RWCUR)) {
			out[key] = true
		}
	}
	return out
}

// clearingComponents groups the extract's documents into those SAP
// cleared together, directly or through other documents: a clearing is
// the supplier lines of a company with one clearing document (AUGBL), as
// the load clears them (clearings). A document no clearing names is a
// component of its own. Each component's keys are sorted.
func clearingComponents(e *sap.Extract, t target) [][]string {
	parent := map[string]string{}
	find := func(k string) string {
		for parent[k] != k {
			parent[k] = parent[parent[k]]
			k = parent[k]
		}
		return k
	}
	first := map[string]string{} // clearing -> a document of it
	for _, l := range e.ACDOCA {
		key := documentKey(l)
		if _, ok := parent[key]; !ok {
			parent[key] = key
		}
		if l.AUGBL == "" || !t.supplierLine(l) {
			continue
		}
		clearing := l.RBUKRS + "/" + l.AUGBL
		if f, ok := first[clearing]; ok {
			parent[find(key)] = find(f)
		} else {
			first[clearing] = key
		}
	}
	byRoot := map[string][]string{}
	for _, key := range sortedKeys(parent) {
		byRoot[find(key)] = append(byRoot[find(key)], key)
	}
	out := make([][]string, 0, len(byRoot))
	for _, root := range sortedKeys(byRoot) {
		out = append(out, byRoot[root])
	}
	return out
}

// foreignDocuments are the documents the run posts in their foreign
// document currency, by document key, with that currency; and the
// documents it cannot post in a currency that clears with what earlier
// runs posted (clearingCurrency), with why, which the quality gate
// refuses. Earlier runs' documents (target.posted) are included in the
// first map when they were posted in a foreign currency, so a later run
// numbers their lines as the run that posted them did.
func foreignDocuments(e *sap.Extract, t target) (map[string]string, map[string]string) {
	currencies := documentCurrencies(e, t)
	foreign := func(key string) bool { c := currencies[key]; return c != "" && c != t.localCurrency(key) }
	out, conflicts := map[string]string{}, map[string]string{}
	if t.balances {
		for key := range openDocuments(e, t) {
			if foreign(key) {
				out[key] = currencies[key]
			}
		}
		return out, conflicts
	}
	for _, docs := range clearingComponents(e, t) {
		c, why := clearingCurrency(docs, currencies, t)
		for _, key := range docs {
			_, earlier := t.posted[key]
			switch {
			case why != "" && !earlier:
				conflicts[key] = why
			case why == "" && c != t.localCurrency(key):
				out[key] = c
			}
		}
	}
	return out, conflicts
}

// clearingCurrency is the currency the documents SAP cleared together post
// in: the one earlier runs posted those of them they loaded in (an item
// posted is never posted again), else their document currency when they
// all share one, else company currency, as before (a USD invoice paid in
// CAD). why says why they cannot: earlier runs posted them in different
// currencies, or in a foreign one that another of them is not in, so the
// platform could not clear them (ledger.mixed_items, a clearing that does
// not net); "" when they can.
func clearingCurrency(docs []string, currencies map[string]string, t target) (string, string) {
	var posted []string
	for _, key := range docs {
		if c, ok := t.posted[key]; ok && !slices.Contains(posted, c) {
			posted = append(posted, c)
		}
	}
	local := t.localCurrency(docs[0])
	slices.Sort(posted)
	switch {
	case len(posted) > 1:
		return "", fmt.Sprintf("earlier runs loaded the documents SAP cleared together with it in %s", strings.Join(posted, " and "))
	case len(posted) == 1 && posted[0] != local:
		for _, key := range docs {
			if _, earlier := t.posted[key]; !earlier && currencies[key] != posted[0] {
				return "", fmt.Sprintf("SAP cleared it together with documents an earlier run loaded in %s, and %s is not in %s", posted[0], key, posted[0])
			}
		}
		return posted[0], ""
	case len(posted) == 1:
		return local, ""
	}
	c := currencies[docs[0]]
	if c == "" || slices.ContainsFunc(docs, func(k string) bool { return currencies[k] != c }) {
		return local, ""
	}
	return c, ""
}

// postedCurrencies are the currencies earlier runs posted the extract's
// documents in, by document key: the transaction currency of the entry
// each one's external reference names.
func (e *Endpoints) postedCurrencies(ctx context.Context, ext *sap.Extract, r refs) (map[string]string, error) {
	keys := map[uuid.UUID]string{}
	for _, l := range ext.ACDOCA {
		if id, ok := r["ACDOCA/"+documentKey(l)]; ok {
			keys[id] = documentKey(l)
		}
	}
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	currencies, err := e.Postings.EntryCurrencies(ctx, slices.Collect(maps.Keys(keys)))
	for id, c := range currencies {
		out[keys[id]] = c
	}
	return out, err
}

// sourceRate is the rate from a foreign document's currency into its
// company's that SAP's amounts imply, the company code amount over the
// document amount of its line with the largest document amount (the
// first in SAP line order of equal ones), where SAP's rounding moves the
// rate least; ok is false when no line has a document amount. The
// extract does not carry the document's own exchange rate (an SAP
// specialist is to confirm which field would). Amounts that do not parse
// count as zero: the quality gate refuses them.
func sourceRate(lines []sap.ACDOCA, currency, local string, t target) (*big.Rat, bool) {
	sorted := slices.Clone(lines)
	slices.SortFunc(sorted, func(a, b sap.ACDOCA) int { return cmp.Compare(a.DOCLN, b.DOCLN) })
	var largest, hsl money.Minor
	for _, l := range sorted {
		if wsl, _ := t.amount(l.WSL, currency); wsl.Abs() > largest.Abs() {
			largest = wsl
			hsl, _ = t.amount(l.HSL, local)
		}
	}
	if largest == 0 {
		return nil, false
	}
	return new(big.Rat).Quo(money.Rat(hsl, t.exponents[local]), money.Rat(largest, t.exponents[currency])), true
}

// lineAmounts are what a line of a document posts: its document amount
// with its company code amount kept when the document posts in a foreign
// currency, else its company code amount. The quality gate checked both.
func (l *loader) lineAmounts(line sap.ACDOCA, local string) (money.Minor, *money.Minor) {
	hsl, _ := l.t.amount(line.HSL, local)
	foreign := l.foreign[documentKey(line)]
	if foreign == "" {
		return hsl, nil
	}
	wsl, _ := l.t.amount(line.WSL, foreign)
	return wsl, &hsl
}

// foreignItems compares the open supplier items loaded in a foreign
// document currency, ordinary items and down payments alike, per vendor
// and currency in that currency, with each currency's total: the controls
// in company currency cannot tell an item loaded with the wrong document
// amount. Shown when either side has any, so a scope without them reports
// as before.
func (rc *reconciler) foreignItems(ctx context.Context, s companyScope) error {
	terms := map[string]map[string][]money.Minor{} // currency -> vendor -> amounts
	for _, l := range rc.ext.ACDOCA {
		if l.RBUKRS != s.cc || l.LIFNR == "" || l.AUGBL != "" || !rc.recon[s.cc+"/"+l.RACCT] || l.RWCUR == "" || l.RWCUR == s.cur {
			continue
		}
		amt, err := rc.t.amount(l.WSL, l.RWCUR)
		if err != nil {
			rc.refuse("ACDOCA", documentKey(l)+"/"+l.DOCLN, "document amount (WSL): %v", err)
			continue
		}
		if terms[l.RWCUR] == nil {
			terms[l.RWCUR] = map[string][]money.Minor{}
		}
		vendor := "vendor " + sap.Alpha(l.LIFNR)
		terms[l.RWCUR][vendor] = append(terms[l.RWCUR][vendor], amt)
	}
	open, err := rc.e.Reports.OpenForeignItems(ctx, s.le, int16(rc.t.year))
	if err != nil {
		return err
	}
	platform := map[string]map[string][]money.Minor{}
	for id, sums := range open {
		key, err := rc.vendorKey(ctx, s, id)
		if err != nil {
			return err
		}
		for currency, sum := range sums {
			if platform[currency] == nil {
				platform[currency] = map[string][]money.Minor{}
			}
			platform[currency][key] = append(platform[currency][key], sum)
		}
	}
	for _, currency := range sortedKeys(unionKeys(terms, platform)) {
		rc.foreignCurrency(s, currency, terms[currency], platform[currency])
	}
	return nil
}

// foreignCurrency is foreignItems for one currency.
func (rc *reconciler) foreignCurrency(s companyScope, currency string, sapTerms, platformTerms map[string][]money.Minor) {
	format := func(v money.Minor) string { return rc.format(v, currency) }
	what := "open items in " + currency
	sapSide := rc.totals("ACDOCA", s.cc, sapTerms, func(v string) string { return "the " + what + " of " + v })
	platformSide := rc.totals("platform", s.cc, platformTerms, func(v string) string { return "the platform's " + what + " of " + v })
	d := diffs(s.cc, sapSide, platformSide, format)
	for i := range d {
		d[i].Key = currency + " items of " + d[i].Key
	}
	rc.rec.Diffs = append(rc.rec.Diffs, d...)
	sapTotal := rc.total("ACDOCA", s.cc, "the "+what+" of company "+s.cc, slices.Collect(maps.Values(sapSide)))
	platformTotal := rc.total("platform", s.cc, "the platform's "+what+" of company "+s.cc, slices.Collect(maps.Values(platformSide)))
	rc.add(s.cc, "Open items in "+currency+" total", format(sapTotal), format(platformTotal), sapTotal == platformTotal)
	rc.add(s.cc, "Open items in "+currency+": vendors with differences", "0", strconv.Itoa(len(d)), len(d) == 0)
}

// documentAmount is a line's amount in the currency its document posts
// in, and that currency: its document amount (WSL) when the document
// posts in a foreign currency (foreignDocuments), else its company code
// amount (HSL).
func (q *qualityCheck) documentAmount(l sap.ACDOCA) (money.Minor, string, error) {
	if c := q.foreign[documentKey(l)]; c != "" {
		m, err := q.t.amount(l.WSL, c)
		return m, c, err
	}
	c := q.currency[l.RBUKRS]
	m, err := q.t.amount(l.HSL, c)
	return m, c, err
}

// documentCurrencies checks the open items whose document currency (ACDOCA
// RWCUR) is not their company's, and the documents that post in a foreign
// currency (foreignDocument). An open item loads in its document currency
// when its document does; one without a document currency is refused
// (T72b's never-guess rule; an OData extract does not carry it yet), and
// so is a line of an open GR/IR position in another currency than its
// order's, company currency (a GR/IR line of an order in a foreign
// currency is grirLine's, its position taken over in that currency), and
// an open item whose document cannot post in its currency (foreignItem). An open item in company currency with a
// document amount and none in company currency is refused too: the load
// would post no line for it (review:batch31). So no open item with an
// amount in either currency is left out unreported, in either mode: in a
// document that posts in a foreign currency foreignLine refuses a line of
// a document amount alone, and in balances mode the opening entry takes
// the rest from HSL as journalPosting does. Lines SAP reports as cleared
// (AUGBL) and lines of no amount in either currency are not checked as
// items. It runs after grirChecks, which reads the positions.
func (q *qualityCheck) documentCurrencies() {
	docs := map[string][]sap.ACDOCA{}
	for _, l := range q.e.ACDOCA {
		docs[documentKey(l)] = append(docs[documentKey(l)], l)
	}
	for _, l := range q.e.ACDOCA {
		switch {
		case !q.openItem(l), q.grirForeignOrder(l): // a GR/IR line of an order in a foreign currency is grirLine's
		case l.RWCUR != q.currency[l.RBUKRS]:
			q.foreignItem(l, docs[documentKey(l)])
		case q.t.zero(l.HSL, l.RWCUR):
			q.add(SevError, "ACDOCA", documentKey(l)+"/"+l.DOCLN, "open item of SAP document %s has no amount in company currency %s (HSL), "+
				"but its document amount (WSL) is %q: the load posts a document in company currency from HSL, so it would load as no item",
				l.BELNR, l.RWCUR, l.WSL)
		}
	}
	for _, key := range sortedKeys(q.foreign) {
		if _, earlier := q.t.posted[key]; !earlier {
			q.foreignDocument(key, q.foreign[key], docs[key])
		}
	}
	for _, key := range sortedKeys(q.conflicts) {
		q.add(SevError, "ACDOCA", key, "SAP document %s cannot load in a currency its clearing can be taken over in: %s; the platform clears "+
			"items of one currency, so load the year in balances mode or have the clearing corrected", key, q.conflicts[key])
	}
}

// leftOutLines refuses a line that is not an open item, with a document
// amount (WSL) and no company code amount (HSL), in a document the load
// posts as an entry of its own in company currency: in documents mode any
// not in a foreign currency, in balances mode the down payments' documents
// not in one. The posting takes a line's amount from HSL there and leaves
// a line of none out, so its document amount would be lost without a word
// (review:batch31). No money rule gives it a value, so it is refused, as
// documentCurrencies refuses such an open item and foreignLine such a line
// of a foreign-currency document. A line that carries no WSL is not
// refused: an OData extract carries none yet, and every line of no company
// code amount in one would be refused (ADR-0005 As built); a document an
// earlier run posted is not posted again; a conflict
// is refused whole; and the other documents of balances mode are part of
// the opening entry, which carries company code amounts alone.
func (q *qualityCheck) leftOutLines() {
	downPayments := downPaymentDocs(q.e)
	for _, l := range q.e.ACDOCA {
		key := documentKey(l)
		_, earlier := q.t.posted[key]
		_, own := downPayments[key]
		switch {
		case l.WSL == "", q.foreign[key] != "", q.conflicts[key] != "", earlier, q.t.balances && !own:
		case !q.t.zero(l.HSL, q.currency[l.RBUKRS]), !q.hasDocumentAmount(l), q.openItem(l):
		default:
			// Balances mode is not offered as a way out: a document can post
			// there on its own too (a down payment's, or one whose open item
			// is in a foreign currency), where the line is refused again
			// (review round 2).
			q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "line of SAP document %s has a document amount (WSL %q, RWCUR %q) and no amount "+
				"in company currency %s (HSL): its document posts in company currency from HSL, so the load would leave the line out; "+
				"have SAP's line corrected", l.BELNR, l.WSL, l.RWCUR, q.currency[l.RBUKRS])
		}
	}
}

// foreignItem refuses an open item not in its company's currency that
// cannot load in its document currency, saying why.
func (q *qualityCheck) foreignItem(l sap.ACDOCA, lines []sap.ACDOCA) {
	key, cur := documentKey(l)+"/"+l.DOCLN, q.currency[l.RBUKRS]
	switch {
	case l.RWCUR == "":
		q.add(SevError, "ACDOCA", key, "open item of SAP document %s has no document currency (RWCUR): it is not migrated unless it is "+
			"in company %s's currency %s, which cannot be told; extract RWCUR", l.BELNR, l.RBUKRS, cur)
	case q.t.isGRIR(l):
		q.add(SevError, "ACDOCA", key, "open item of SAP document %s is in document currency %s, but its order %s is in company %s's currency %s: "+
			"an order's GR/IR positions are taken over in the order's currency, from lines in it", l.BELNR, l.RWCUR, l.EBELN, l.RBUKRS, cur)
	case q.foreign[documentKey(l)] == l.RWCUR, q.conflicts[documentKey(l)] != "":
		// loads in its document currency, or is refused as a conflict
	case slices.ContainsFunc(lines, func(o sap.ACDOCA) bool { return o.RWCUR != l.RWCUR }):
		q.add(SevError, "ACDOCA", key, "open item of SAP document %s is in document currency %s, and not every line of its document is "+
			"(RWCUR): a document posts in one currency, which cannot be told", l.BELNR, l.RWCUR)
	case !slices.ContainsFunc(lines, func(o sap.ACDOCA) bool { return !q.t.zero(o.WSL, o.RWCUR) }):
		q.add(SevError, "ACDOCA", key, "open item of SAP document %s is in document currency %s, but no line of its document has an "+
			"amount in it (WSL)", l.BELNR, l.RWCUR)
	default:
		q.add(SevError, "ACDOCA", key, "open item of SAP document %s is in document currency %s, and SAP cleared its document together "+
			"with documents in another currency or without one (AUGBL): the platform clears items of one currency, so it cannot load in %s",
			l.BELNR, l.RWCUR, l.RWCUR)
	}
}

// foreignDocument checks a document that posts in a foreign currency: a
// currency the platform knows (ref.currency), each line's document amount
// (WSL) parsing in it and agreeing with its company code amount (HSL),
// the lines' document amounts in range together and balancing, a rate
// SAP's amounts imply that a journal entry can record
// (ledger.RecordableRate), and in balances mode, where it loads on its own, what
// ownDocument checks (a down payment's document is downPayments').
func (q *qualityCheck) foreignDocument(key, cur string, lines []sap.ACDOCA) {
	if _, ok := q.t.exponents[cur]; !ok {
		q.add(SevError, "ACDOCA", key, "document currency %s (RWCUR) is not a currency the platform knows; the document cannot load in it", cur)
		return
	}
	local := q.t.localCurrency(key)
	reported := len(q.out)
	amounts := []money.Minor{}
	for _, l := range lines {
		amounts = append(amounts, q.foreignLine(l, cur, local))
	}
	if len(q.out) > reported {
		return // a line's amounts are wrong, which the document's would repeat
	}
	sizes := make([]money.Minor, len(amounts))
	for i, a := range amounts {
		sizes[i] = a.Abs() // parsed amounts are never -2^63
	}
	if _, err := money.Sum(sizes...); err != nil {
		q.add(SevError, "ACDOCA", key, "the document amounts (WSL) of the document add up to more than an amount holds")
		return
	}
	if total, _ := money.Sum(amounts...); total != 0 { // bounded by the sizes
		q.add(SevError, "ACDOCA", key, "document does not balance in its document currency %s (WSL, off by %d minor units)", cur, int64(total))
		return
	}
	if rate, ok := sourceRate(lines, cur, local, q.t); !ok || !ledger.RecordableRate(rate) {
		q.add(SevError, "ACDOCA", key, "the rate from %s to %s that the document's amounts imply is not above zero to the 12 decimals a journal "+
			"entry records, or not below 10^16", cur, local)
	}
	if q.t.balances && len(openDownPayments(lines)) == 0 {
		q.ownDocument(key, "the document in "+cur, lines)
	}
}

// foreignLine checks a line of a document that posts in a foreign
// currency, or a GR/IR line of an order in one (grirForeignLine), and
// returns its document amount. A line may have a company code amount
// without a document amount (an exchange difference SAP posts in local
// currency alone), but a vendor line may not, being an item in its
// document currency, nor a GR/IR line of such an order, part of a
// position taken over in that currency; and no line may have a document
// amount without a company code amount, or the two of opposite signs.
func (q *qualityCheck) foreignLine(l sap.ACDOCA, cur, local string) money.Minor {
	key := documentKey(l) + "/" + l.DOCLN
	wsl, err := q.t.amount(l.WSL, cur)
	if err != nil {
		q.add(SevError, "ACDOCA", key, "document amount (WSL): %v", err)
		return 0
	}
	hsl, err := q.t.amount(l.HSL, local)
	switch {
	case err != nil: // reported by journalLine
	case hsl == 0 && wsl != 0:
		q.add(SevError, "ACDOCA", key, "line has %s %s in its document currency (WSL) and no amount in company currency (HSL)", l.WSL, cur)
	case wsl == 0 && hsl != 0 && q.t.supplierLine(l):
		q.add(SevError, "ACDOCA", key, "vendor line has no amount in its document currency %s (WSL), so it would be an item without one", cur)
	case wsl == 0 && hsl != 0 && q.grirForeignOrder(l):
		q.add(SevError, "ACDOCA", key, "GR/IR line of order item %s/%s has no amount in its order's currency %s (WSL): the position is taken over "+
			"in %s, where the line would have no value", l.EBELN, l.EBELP, cur, cur)
	case wsl != 0 && hsl != 0 && (wsl < 0) != (hsl < 0):
		q.add(SevError, "ACDOCA", key, "line's amounts in document currency (WSL %s %s) and company currency (HSL %s %s) have opposite signs",
			l.WSL, cur, l.HSL, local)
	}
	return wsl
}
