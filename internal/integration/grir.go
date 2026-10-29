package integration

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Open GR/IR positions (T72f, ADR-0034 As built, 03-data-model §3.7). An
// order SAP valued through GR/IR has its receipts and invoice receipts
// offset to SAP's GR/IR clearing account, every line there naming its
// order item (ACDOCA EBELN/EBELP); an item received and not invoiced in
// full leaves an open credit. The load releases such an order valued
// through GR/IR on the platform, records its receipts without posting
// them (SAP's journal carries their value), and takes each item's open
// position over in an entry of its own (the T72c pattern): SAP's balance
// and the item's open quantity, what SAP invoiced less what it received,
// on the platform's order item, against the GR/IR account's lines that
// SAP's documents (or the opening entry) loaded without their order
// items. An invoice after cutover then clears the position, as it clears
// the platform's own, instead of booking the expense again; a closed
// order's residual is cleared by a GR/IR clearing (T72e). The quality gate
// refuses what it cannot read rather than guess (grirChecks), and the
// reconciliation compares the open positions per order item.

// grirPosition is an order item's position on SAP's GR/IR account: its
// lines' amounts in company code currency (HSL), their latest posting
// date, and the item's open quantity, invoiced less received (nil until
// grirPositions reads it).
type grirPosition struct {
	company, order, item string // BUKRS, EBELN, EBELP
	amounts              []money.Minor
	last                 string // the latest BUDAT, YYYYMMDD
	qty                  *big.Rat
}

// isGRIR reports whether a journal line is on the legal entity's GR/IR
// account (account determination gr_ir).
func (t target) isGRIR(l sap.ACDOCA) bool { return t.charts[l.RBUKRS][sap.Alpha(l.RACCT)].GRIR }

// grirPositions are the extract's GR/IR positions by EBELN/EBELP: the
// lines on a legal entity's GR/IR account that name their order item, with
// the item's received quantity (MSEG 101) and the net quantity SAP's posted
// invoice documents invoiced on it (credit memos negative; postedInvoiced).
// Lines without an order item, amounts or receipt quantities that do not
// parse and documents whose quantities cannot be told are left out: the
// quality gate refuses them (grirChecks, journalLine, postedInvoiced).
func grirPositions(e *sap.Extract, t target) map[string]*grirPosition {
	out := map[string]*grirPosition{}
	for _, l := range e.ACDOCA {
		amt, err := t.amount(l.HSL, t.entities[l.RBUKRS].LocalCurrency)
		if !t.isGRIR(l) || l.EBELN == "" || l.EBELP == "" || err != nil {
			continue
		}
		key := l.EBELN + "/" + l.EBELP
		p := out[key]
		if p == nil {
			p = &grirPosition{company: l.RBUKRS, order: l.EBELN, item: l.EBELP, qty: new(big.Rat)}
			out[key] = p
		}
		p.amounts, p.last = append(p.amounts, amt), max(p.last, l.BUDAT)
	}
	for _, m := range e.MSEG {
		if p := out[m.EBELN+"/"+m.EBELP]; p != nil && m.BWART == "101" && money.PositiveQuantity(m.MENGE) == nil {
			p.qty.Sub(p.qty, money.ParseRat(m.MENGE))
		}
	}
	for key, q := range invoicedNet(e) {
		if p := out[key]; p != nil {
			p.qty.Add(p.qty, q)
		}
	}
	return out
}

// invoicedNet is the net quantity SAP's posted invoice documents invoiced
// per order item, by EBELN/EBELP: invoices positive, credit memos
// negative, subsequent debit/credit items none. Documents whose kind or
// quantities cannot be told are left out (postedDocuments).
func invoicedNet(e *sap.Extract) map[string]*big.Rat {
	docs, _ := postedDocuments(e, "") // the issues are the quality gate's and the reconciliation's
	out := map[string]*big.Rat{}
	for _, d := range docs {
		for _, it := range d.items {
			q := money.ParseRat(it.MENGE) // a positive quantity (postedInvoiced)
			if d.kind == payables.KindCreditMemo {
				q.Neg(q)
			}
			key := it.EBELN + "/" + it.EBELP
			out[key] = new(big.Rat).Add(orZero(out[key]), q)
		}
	}
	return out
}

// balance is the position's balance in company code currency. Its amounts
// are bounded by the company's lines, which the gate checks (journal), so
// their sum is in range; a reconciliation run, without the gate, sums them
// through reconciler.total instead.
func (p *grirPosition) balance() money.Minor {
	total, _ := money.Sum(p.amounts...) // in range: see above
	return total
}

// open reports whether a position has anything to take over: a balance or
// an open quantity.
func (p *grirPosition) open() bool { return p.balance() != 0 || p.qty.Sign() != 0 }

// grirOrders are the orders of the extract SAP valued through GR/IR: those
// a line on a legal entity's GR/IR account names. They load valued through
// GR/IR, whether or not a position is still open.
func grirOrders(positions map[string]*grirPosition) map[string]bool {
	out := map[string]bool{}
	for _, p := range positions {
		out[p.order] = true
	}
	return out
}

// grirChecks refuses what the take-over of GR/IR positions cannot read
// (T72b's never-guess rule): a line on the GR/IR account without its
// order item, or naming one outside the extract or of another company;
// SAP's GR/IR account (T030 WRX) when it is not the legal entity's, and a
// warning for a company whose extract has receipts but no T030 to tell
// SAP's GR/IR account by; and
// for each position, an order in another currency than the company's (the
// extract has only the company code amount, HSL), a movement other than a
// receipt or a receipt quantity that does not parse on the item, and an
// open quantity without a balance;
// and on an order valued through GR/IR, an item received without a GR/IR
// line, which an invoice after cutover would clear a position it never had.
func (q *qualityCheck) grirChecks() {
	headers, items := map[string]sap.EKKO{}, map[string]bool{}
	for _, h := range q.e.EKKO {
		headers[h.EBELN] = h
	}
	for _, i := range q.e.EKPO {
		items[i.EBELN+"/"+i.EBELP] = true
	}
	for _, l := range q.e.ACDOCA {
		q.grirLine(l, headers, items)
	}
	q.grirAccount(headers)
	movements := map[string][]sap.MSEG{}
	for _, m := range q.e.MSEG {
		movements[m.EBELN+"/"+m.EBELP] = append(movements[m.EBELN+"/"+m.EBELP], m)
	}
	q.positions = grirPositions(q.e, q.t)
	for _, key := range sortedKeys(q.positions) {
		q.grirPosition(q.positions[key], headers[q.positions[key].order], movements[key])
	}
	q.grirReceipts(grirOrders(q.positions), q.positions)
}

// grirLine checks that a line on the GR/IR account names an order item of
// the extract in the line's company: a scope of several companies holds
// their orders, and a position is taken over in its order's legal entity.
func (q *qualityCheck) grirLine(l sap.ACDOCA, headers map[string]sap.EKKO, items map[string]bool) {
	key := documentKey(l) + "/" + l.DOCLN
	account := sap.Alpha(l.RACCT)
	switch {
	case !q.t.isGRIR(l):
	case l.EBELN == "" || l.EBELP == "":
		q.add(SevError, "ACDOCA", key, "line on the GR/IR account %s without its order item (EBELN/EBELP): its position cannot be taken over "+
			"onto the platform's order item, and an invoice after cutover would book the expense again", account)
	case !items[l.EBELN+"/"+l.EBELP]:
		q.add(SevError, "ACDOCA", key, "line on the GR/IR account %s names order item %s/%s, which is not in the extract (EKPO) of the scope's companies",
			account, l.EBELN, l.EBELP)
	case headers[l.EBELN].BUKRS != l.RBUKRS:
		q.add(SevError, "ACDOCA", key, "line on the GR/IR account %s of company %s names order %s of company %s", account, l.RBUKRS, l.EBELN,
			headers[l.EBELN].BUKRS)
	}
}

// grirAccount refuses lines on SAP's GR/IR account (T030 WRX) when it is
// not the legal entity's gr_ir account: loaded on an ordinary account,
// their positions would not be taken over. A company whose extract has
// goods receipts and no T030 at all (an OData extract, which does not map
// it) is warned about instead: SAP's GR/IR account cannot be told, so its
// positions are taken over only if the legal entity's gr_ir account is
// that account.
func (q *qualityCheck) grirAccount(headers map[string]sap.EKKO) {
	posted, customized, received := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, l := range q.e.ACDOCA {
		posted[l.RBUKRS+"/"+sap.Alpha(l.RACCT)] = true
	}
	for _, r := range q.e.T030 {
		customized[r.BUKRS] = true
	}
	for _, m := range q.e.MSEG {
		received[headers[m.EBELN].BUKRS] = true
	}
	for _, c := range q.e.T001 {
		if received[c.BUKRS] && !customized[c.BUKRS] {
			q.add(SevWarning, "T030", c.BUKRS, "the extract has goods receipts of company %s but no T030 to tell SAP's GR/IR account (WRX) by: "+
				"GR/IR positions are taken over only if the legal entity's gr_ir account is SAP's; if it is not, invoices after cutover "+
				"book the expense of goods SAP received again", c.BUKRS)
		}
	}
	for _, r := range q.e.T030 {
		account := sap.Alpha(r.KONTS)
		if r.KTOSL == "WRX" && posted[r.BUKRS+"/"+account] && !q.t.charts[r.BUKRS][account].GRIR {
			q.add(SevError, "T030", r.BUKRS+"/WRX", "SAP's GR/IR account %s has lines but is not the legal entity's gr_ir account: its open positions "+
				"would not be taken over, and invoices after cutover would book the expense again; make it the gr_ir account", account)
		}
	}
}

// grirPosition checks one order item's position and its movements.
func (q *qualityCheck) grirPosition(p *grirPosition, h sap.EKKO, movements []sap.MSEG) {
	key := p.order + "/" + p.item
	if h.EBELN != "" && h.WAERS != q.currency[p.company] { // an order outside the extract is grirLine's
		q.add(SevError, "EKKO", p.order, "order %s is in %s and has GR/IR positions, which the extract holds in company code currency %s only (HSL); "+
			"they are not converted", p.order, h.WAERS, q.currency[p.company])
	}
	for _, m := range movements {
		if m.BWART != "101" {
			q.add(SevError, "MSEG", m.MBLNR+"/"+m.MJAHR+"/"+m.ZEILE, "movement type %s on order item %s, which has a GR/IR position: its received "+
				"quantity, which counts receipts (101) only, cannot be read", m.BWART, key)
		} else if err := money.PositiveQuantity(m.MENGE); err != nil {
			q.add(SevError, "MSEG", m.MBLNR+"/"+m.MJAHR+"/"+m.ZEILE, "%v; the received quantity of order item %s cannot be read", err, key)
		}
	}
	if p.balance() == 0 && p.qty.Sign() != 0 {
		q.add(SevError, "ACDOCA", key, "order item %s has an open GR/IR quantity of %s and no balance on the GR/IR account; it cannot be taken over",
			key, plainQuantity(p.qty))
	}
}

// grirReceipts refuses an item received on an order valued through GR/IR
// without a line on the GR/IR account.
func (q *qualityCheck) grirReceipts(orders map[string]bool, positions map[string]*grirPosition) {
	seen := map[string]bool{}
	for _, m := range q.e.MSEG {
		key := m.EBELN + "/" + m.EBELP
		if orders[m.EBELN] && positions[key] == nil && !seen[key] {
			seen[key] = true
			q.add(SevError, "EKPO", key, "item %s was received in SAP without a GR/IR line, while its order's other items post GR/IR: the platform "+
				"values an order through GR/IR whole, so an invoice after cutover would clear a position the item never had", key)
		}
	}
}

// takeOverGRIR takes over the open GR/IR positions of the orders this run
// released valued through GR/IR, one entry per order (its GRIR/<EBELN>
// reference): an order an earlier run loaded is not released again, so
// its positions are never taken over twice, and the ledger refuses a
// position taken over onto an item that has one. The entry's WE number
// range is the configuration's, which a GR/IR account requires; a period
// or range the ledger refuses is reported as the record's refusal. It
// comes after the receipts: the first order's GR/IR lock is taken after
// the goods_receipt range, as a goods movement takes them, and before the
// journal's ranges. Each later order's lock is taken while the run holds
// the WE range and the period of the take-over before it, the other way
// round from a posting's order; that cannot deadlock, as every order taken
// over here was created by this run and is invisible to other transactions
// until the load commits, so no one else can hold or wait for its lock.
func (l *loader) takeOverGRIR(ctx context.Context) error {
	nos := itemNos(l.ext)
	byOrder := map[string][]*grirPosition{}
	for _, p := range l.grir {
		if l.viaGRIR[p.order] && p.open() {
			byOrder[p.order] = append(byOrder[p.order], p)
		}
	}
	for _, order := range sortedKeys(byOrder) {
		in := l.grirPosting(order, byOrder[order], nos)
		done, err := l.step(ctx, "ACDOCA", "GR/IR "+order, func() error {
			entry, err := l.e.Postings.Post(ctx, in)
			if err != nil {
				return err
			}
			return l.addRef(ctx, "ledger.journal_entry", entry.ID, "GRIR", order)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts["gr_ir_positions"] += len(in.GRIR)
		}
	}
	return nil
}

// grirPosting is the take-over entry of an order's open positions, in item
// order, dated the latest of their lines.
func (l *loader) grirPosting(order string, positions []*grirPosition, nos map[string]int32) ledger.Posting {
	slices.SortFunc(positions, func(a, b *grirPosition) int { return cmp.Compare(a.item, b.item) })
	company := positions[0].company
	cur := l.t.entities[company].LocalCurrency
	var last string
	for _, p := range positions {
		last = max(last, p.last)
	}
	date, _ := sapDate(last) // each line's date is checked by the quality gate
	text := "GR/IR positions of SAP order " + order
	ref := order
	in := ledger.Posting{LegalEntityID: l.t.entities[company].ID, DocumentType: procurement.WEDocumentType, DocumentDate: date, PostingDate: date,
		Currency: cur, Reference: &ref, HeaderText: &text, SourceContext: "integration", SourceDocumentID: l.run.ID}
	for _, p := range positions {
		balance := p.balance()
		item := "SAP " + p.order + "/" + p.item
		in.GRIR = append(in.GRIR, ledger.GRIRItem{PurchaseOrderID: l.refs["EKKO/"+order], POItemNo: nos[p.order+"/"+p.item], Quantity: plainQuantity(p.qty),
			Unit: unit, TakesOver: &balance, Text: &item})
	}
	return in
}

// grir compares the open GR/IR positions per order item of the company,
// SAP's from the extract (grirPositions) and the platform's of the orders
// loaded, by balance in local currency and open quantity, with their
// total. Shown when either side has a position, so a scope without them
// reports as before.
func (rc *reconciler) grir(ctx context.Context, s companyScope) error {
	sapSide := map[string]string{}
	var sapTotal []money.Minor
	positions := grirPositions(rc.ext, rc.t)
	for _, key := range sortedKeys(positions) {
		p := positions[key]
		if p.company != s.cc || !p.open() {
			continue
		}
		total := rc.total("ACDOCA", s.cc+"/"+key, "the GR/IR lines of order item "+key, p.amounts)
		sapSide["GR/IR of order item "+key] = grirValue(s.format(total), p.qty)
		sapTotal = append(sapTotal, total)
	}
	platform, platformTotal, err := rc.platformGRIR(ctx, s)
	if err != nil || len(sapSide) == 0 && len(platform) == 0 {
		return err
	}
	n := 0
	for _, key := range sortedKeys(unionKeys(sapSide, platform)) {
		if sapSide[key] != platform[key] {
			n++
			rc.rec.Diffs = append(rc.rec.Diffs, Diff{CompanyCode: s.cc, Key: key, SAP: sapSide[key], Platform: platform[key]})
		}
	}
	sapSum := rc.total("ACDOCA", s.cc, "the open GR/IR positions of company "+s.cc, sapTotal)
	rc.add(s.cc, "Open GR/IR total", s.format(sapSum), s.format(platformTotal), sapSum == platformTotal)
	rc.add(s.cc, "Open GR/IR: order items with differences", "0", strconv.Itoa(n), n == 0)
	return nil
}

// platformGRIR is the platform's side of grir: the open positions of the
// company's orders the scope loaded, keyed as SAP's, and their total.
func (rc *reconciler) platformGRIR(ctx context.Context, s companyScope) (map[string]string, money.Minor, error) {
	var ids []uuid.UUID
	ebeln := map[uuid.UUID]string{}
	for _, h := range rc.ext.EKKO {
		if id, ok := rc.r["EKKO/"+h.EBELN]; ok && h.BUKRS == s.cc {
			ids, ebeln[id] = append(ids, id), h.EBELN
		}
	}
	open, err := rc.e.Reports.OpenGRIR(ctx, s.le, ids)
	if err != nil {
		return nil, 0, err
	}
	ebelp := itemsByNo(rc.ext)
	out := map[string]string{}
	var amounts []money.Minor
	for id, items := range open {
		for no, p := range items {
			order := ebeln[id]
			out["GR/IR of order item "+order+"/"+ebelp[order+"/"+strconv.Itoa(int(no))]] = grirValue(s.format(p.LC), money.ParseRat(p.Quantity))
			amounts = append(amounts, p.LC)
		}
	}
	return out, rc.total("platform", s.cc, "the platform's open GR/IR positions of company "+s.cc, amounts), nil
}

// grirValue renders a position as the reconciliation compares it.
func grirValue(balance string, qty *big.Rat) string {
	return fmt.Sprintf("%s, quantity %s", balance, plainQuantity(qty))
}

// itemsByNo maps EBELN/item number, as the load numbers an order's items
// (itemNos), back to SAP's EBELP.
func itemsByNo(e *sap.Extract) map[string]string {
	out := map[string]string{}
	for key, no := range itemNos(e) {
		order, item, _ := strings.Cut(key, "/")
		out[order+"/"+strconv.Itoa(int(no))] = item
	}
	return out
}
