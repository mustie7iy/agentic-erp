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
	"github.com/jackc/pgx/v5"

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
// order's residual is cleared by a GR/IR clearing (T72e). A later run,
// such as the final delta run at cutover, tops the positions of orders an
// earlier run loaded up to SAP's (review:batch21). The quality gate
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

// takeOverGRIR takes over the GR/IR positions of the orders valued
// through GR/IR, one entry per order: the open positions of those this run
// released, and for those an earlier run loaded, what SAP's positions
// moved since the earlier runs took them over (a final delta run after
// SAP posted receipts or invoices on them; review:batch21). Each item's
// take-over moves what was taken over of it to SAP's position, so a
// position is never taken over twice, and running an extract again posts
// nothing; what the platform itself posted on the item stays beside it.
// An order an earlier run loaded valued directly is not moved onto GR/IR
// (the optional tracker leaf), and one whose run's fiscal year cannot be
// told to be this run's is left with a warning (cannotTopUp): their
// positions stay differences in the reconciliation. The entry's WE number range is the configuration's,
// which a GR/IR account requires; a period or range the ledger refuses is
// reported as the record's refusal.
//
// It comes after the receipts and takes the GR/IR locks of every order
// first (takenOver), after the goods_receipt range and before any period
// or the journal's ranges, the order in which a goods movement, an
// invoice's posting and a GR/IR clearing take them, so it never waits for
// an order's lock while holding a range another posting on the order
// waits for.
func (l *loader) takeOverGRIR(ctx context.Context) error {
	orders, err := l.grirValued(ctx)
	if err != nil {
		return err
	}
	taken, err := l.takenOver(ctx, orders)
	if err != nil {
		return err
	}
	loaded, err := l.loadedBy(ctx, orders)
	if err != nil {
		return err
	}
	years, err := l.runYears(ctx, taken, loaded)
	if err != nil {
		return err
	}
	nos := itemNos(l.ext)
	for _, order := range sortedKeys(orders) {
		items := l.grirItems(order, nos, taken[orders[order]].Items)
		if len(items) == 0 {
			continue
		}
		if why := l.cannotTopUp(order, taken[orders[order]], loaded[order], years); why != "" {
			l.issues = append(l.issues, Issue{Severity: SevWarning, Table: "EKKO", Key: order, Message: why})
			continue
		}
		key, count := order, &l.counts.GRIRPositions
		if !l.viaGRIR[order] {
			key, count = order+"/"+l.run.ID.String(), &l.counts.GRIRTopUps
		}
		in := l.grirPosting(order, items)
		done, err := l.step(ctx, "ACDOCA", "GR/IR "+order, func() error {
			entry, err := l.e.Postings.Post(ctx, in)
			if err != nil {
				return err
			}
			return l.addRef(ctx, "ledger.journal_entry", entry.ID, "GRIR", key)
		})
		if err != nil {
			return err
		}
		if done {
			*count += len(items)
		}
	}
	return nil
}

// grirValued are the platform orders, by EBELN, of the extract's orders
// SAP valued through GR/IR that are valued through GR/IR on the platform:
// those this run released, and those an earlier run loaded so.
func (l *loader) grirValued(ctx context.Context) (map[string]uuid.UUID, error) {
	out := map[string]uuid.UUID{}
	for _, order := range sortedKeys(grirOrders(l.grir)) {
		id := l.refs["EKKO/"+order]
		if l.viaGRIR[order] {
			out[order] = id
			continue
		}
		// An order this run refused has no reference: uuid.Nil names no
		// order, which is not found and so not valued through GR/IR.
		o, _, err := l.e.OrderValues.Order(ctx, id)
		if err != nil {
			return nil, err
		}
		if o.Valuation == procurement.ValuationGRIR {
			out[order] = id
		}
	}
	return out, nil
}

// takenOver locks the orders' GR/IR positions and reads what earlier runs
// took over of their items, legal entity by legal entity, by order and
// item.
func (l *loader) takenOver(ctx context.Context, orders map[string]uuid.UUID) (map[uuid.UUID]ledger.GRIRTakenOverOrder, error) {
	byCompany := map[string][]uuid.UUID{}
	for _, p := range l.grir {
		if id, ok := orders[p.order]; ok && !slices.Contains(byCompany[p.company], id) {
			byCompany[p.company] = append(byCompany[p.company], id)
		}
	}
	out := map[uuid.UUID]ledger.GRIRTakenOverOrder{}
	for _, company := range sortedKeys(byCompany) {
		taken, err := l.e.Postings.TakenOverGRIR(ctx, l.t.entities[company].ID, byCompany[company])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, taken)
	}
	return out, nil
}

// loadedBy are the runs that loaded the orders an earlier run loaded, by
// EBELN: the run their order's external reference names (000168,
// review:batch23). A reference recorded before names none, and its order
// is left out.
func (l *loader) loadedBy(ctx context.Context, orders map[string]uuid.UUID) (map[string]uuid.UUID, error) {
	var earlier []string
	for order := range orders {
		if !l.viaGRIR[order] {
			earlier = append(earlier, order)
		}
	}
	rows, _ := l.tx.Query(ctx, `SELECT external_key, migration_run_id FROM integration.external_ref
		WHERE system = $1 AND object = 'EKKO' AND external_key = ANY($2) AND migration_run_id IS NOT NULL`, l.system, earlier)
	out := map[string]uuid.UUID{}
	var order string
	var run uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&order, &run}, func() error { out[order] = run; return nil }); err != nil {
		return nil, fmt.Errorf("integration: read the migration runs that loaded %d orders: %w", len(earlier), err)
	}
	return out, nil
}

// runYears are the fiscal years of the runs that loaded the orders or whose
// take-overs the orders hold, by run.
func (l *loader) runYears(ctx context.Context, taken map[uuid.UUID]ledger.GRIRTakenOverOrder, loaded map[string]uuid.UUID) (map[uuid.UUID]int32, error) {
	runs := slices.Collect(maps.Values(loaded))
	for _, o := range taken {
		runs = append(runs, o.Sources...)
	}
	rows, _ := l.tx.Query(ctx, `SELECT id, (scope->>'fiscal_year')::int FROM integration.migration_run WHERE id = ANY($1)`, runs)
	out := map[uuid.UUID]int32{}
	var id uuid.UUID
	var year int32
	if _, err := pgx.ForEachRow(rows, []any{&id, &year}, func() error { out[id] = year; return nil }); err != nil {
		return nil, fmt.Errorf("integration: read the fiscal years of %d migration runs: %w", len(runs), err)
	}
	return out, nil
}

// cannotTopUp says why the run leaves the GR/IR positions of an order an
// earlier run loaded as they are, or "" when it tops them up. A run's
// extract carries its fiscal year's journal and invoices only
// (sap.Source), while receipts are not limited to it, so it can tell SAP's
// position of an order only when the run that loaded it (loader, uuid.Nil
// when its reference predates 000168) and every take-over on it are runs
// of its own fiscal year; topping up from another year's would move the
// position wrongly, the GR/IR account still netting to zero and the
// reconciliation, from the same extract, agreeing (review:batch21, round
// 1). So an order loaded with no position open is topped up by the run
// that loaded it (review:batch23). One whose loading run is not recorded
// and that has no take-over to tell a run's year by is left too. Either
// stays a difference in the reconciliation, as before.
func (l *loader) cannotTopUp(order string, o ledger.GRIRTakenOverOrder, loader uuid.UUID, years map[uuid.UUID]int32) string {
	if l.viaGRIR[order] {
		return ""
	}
	if loader == uuid.Nil && len(o.Sources) == 0 {
		return fmt.Sprintf("order %s, which an earlier run loaded with no GR/IR position open before migration runs were recorded on what "+
			"they load, has neither its run nor a take-over to tell the fiscal year of its run by: its GR/IR positions are not topped up, and "+
			"the reconciliation shows them as differences", order)
	}
	for _, run := range o.Sources {
		if year, ok := years[run]; ok && year != l.run.Scope.FiscalYear {
			return fmt.Sprintf("order %s's GR/IR positions were taken over by a run of fiscal year %d: this run's extract holds fiscal year %d's "+
				"journal only, so it cannot tell SAP's position and does not top it up", order, year, l.run.Scope.FiscalYear)
		}
	}
	if year, ok := years[loader]; ok && year != l.run.Scope.FiscalYear {
		return fmt.Sprintf("order %s was loaded by a run of fiscal year %d: this run's extract holds fiscal year %d's journal only, so it "+
			"cannot tell SAP's position and does not top it up", order, year, l.run.Scope.FiscalYear)
	}
	return ""
}

// grirItem is an order item whose take-over moves: SAP's position on it,
// its open quantity and balance (zero when it has no GR/IR line), and
// what earlier runs took over of it (nil when nothing).
type grirItem struct {
	ebelp   string
	no      int32
	qty     *big.Rat
	balance money.Minor
	taken   *ledger.GRIRTakenOver
}

// grirItems are the order's items, in item order, whose position SAP
// holds differs from what earlier runs took over of it (taken, by item
// number): every open one of an order this run released.
func (l *loader) grirItems(order string, nos map[string]int32, taken map[int32]ledger.GRIRTakenOver) []grirItem {
	var out []grirItem
	for _, it := range l.ext.EKPO {
		key := order + "/" + it.EBELP
		if it.EBELN != order {
			continue
		}
		item := grirItem{ebelp: it.EBELP, no: nos[key], qty: new(big.Rat)}
		if p := l.grir[key]; p != nil {
			item.qty, item.balance = p.qty, p.balance()
		}
		had, hadBalance := new(big.Rat), money.Minor(0)
		if t, ok := taken[item.no]; ok {
			item.taken, had, hadBalance = &t, money.ParseRat(t.Quantity), t.Balance
		}
		if had.Cmp(item.qty) != 0 || hadBalance != item.balance {
			out = append(out, item)
		}
	}
	slices.SortFunc(out, func(a, b grirItem) int { return cmp.Compare(a.ebelp, b.ebelp) })
	return out
}

// grirPosting is the take-over entry of an order's items, in item order,
// dated the latest of SAP's GR/IR lines on the order.
func (l *loader) grirPosting(order string, items []grirItem) ledger.Posting {
	var company, last string
	for _, p := range l.grir {
		if p.order == order {
			company, last = p.company, max(last, p.last)
		}
	}
	cur := l.t.entities[company].LocalCurrency
	date, _ := sapDate(last) // each line's date is checked by the quality gate
	text := "GR/IR positions of SAP order " + order
	ref := order
	in := ledger.Posting{LegalEntityID: l.t.entities[company].ID, DocumentType: procurement.WEDocumentType, DocumentDate: date, PostingDate: date,
		Currency: cur, Reference: &ref, HeaderText: &text, SourceContext: "integration", SourceDocumentID: l.run.ID}
	for _, it := range items {
		balance := it.balance
		item := "SAP " + order + "/" + it.ebelp
		in.GRIR = append(in.GRIR, ledger.GRIRItem{PurchaseOrderID: l.refs["EKKO/"+order], POItemNo: it.no, Quantity: plainQuantity(it.qty),
			Unit: unit, TakesOver: &balance, TakenOver: it.taken, Text: &item})
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
