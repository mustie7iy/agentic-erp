package integration

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// unit is the unit of measure of migrated order items: the extract does
// not carry SAP's order unit (EKPO-MEINS).
const unit = "EA"

// refs maps SAP keys to the platform records loaded for them, by
// object/key (LFA1/0000100001, ACDOCA/1000/2026/0100000002).
type refs map[string]uuid.UUID

func loadRefs(ctx context.Context, tx pgx.Tx, system string) (refs, error) {
	rows, _ := tx.Query(ctx, `SELECT object, external_key, entity_id FROM integration.external_ref WHERE system = $1`, system)
	out := refs{}
	var object, key string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&object, &key, &id}, func() error { out[object+"/"+key] = id; return nil }); err != nil {
		return out, fmt.Errorf("integration: read the external references of system %s: %w", system, err)
	}
	return out, nil
}

// loader loads one run's extract through the application services.
type loader struct {
	e       *Endpoints
	tx      pgx.Tx
	p       reqctx.Principal
	run     Run
	system  string
	ext     *sap.Extract
	t       target
	refs    refs
	today   time.Time
	issues  []Issue
	counts  map[string]int
	vendors map[string]uuid.UUID // LIFNR -> partner
	blocks  []uuid.UUID
	entries map[string]loadedEntry // ACDOCA document key -> entry posted by this run
}

type loadedEntry struct {
	ref   ledger.ItemRef // line 0: the entry
	lines map[string]int32
	date  time.Time
}

func (l *loader) issue(table, key, message string) {
	l.issues = append(l.issues, Issue{Severity: SevError, Table: table, Key: key, Message: message})
}

// step runs one record's load in a savepoint. A problem the services
// answer with (a refused record) is reported and rolled back; any other
// error stops the load.
func (l *loader) step(ctx context.Context, table, key string, fn func() error) (bool, error) {
	if _, err := l.tx.Exec(ctx, `SAVEPOINT item`); err != nil {
		return false, fmt.Errorf("integration: open the savepoint for %s %s: %w", table, key, err)
	}
	if err := fn(); err != nil {
		p := problem.From(err)
		if p.Status >= 500 {
			return false, err
		}
		if _, err := l.tx.Exec(ctx, `ROLLBACK TO SAVEPOINT item`); err != nil {
			return false, fmt.Errorf("integration: roll back the refused %s %s: %w", table, key, err)
		}
		l.issue(table, key, refusal(p))
		return false, nil
	}
	if _, err := l.tx.Exec(ctx, `RELEASE SAVEPOINT item`); err != nil {
		return false, fmt.Errorf("integration: release the savepoint for %s %s: %w", table, key, err)
	}
	return true, nil
}

func (l *loader) addRef(ctx context.Context, entityType string, id uuid.UUID, object, key string) error {
	l.refs[object+"/"+key] = id
	_, err := l.tx.Exec(ctx, `INSERT INTO integration.external_ref (tenant_id, entity_type, entity_id, system, object, external_key, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, l.p.TenantID, entityType, id, l.system, object, key, l.p.ID)
	if err != nil {
		return fmt.Errorf("integration: record the external reference %s/%s: %w", object, key, err)
	}
	return nil
}

func (l *loader) plain(minor money.Minor, currency string) string {
	return minor.Plain(l.t.exponents[currency])
}

// load brings the extract over: suppliers, purchase orders, goods
// receipts, parked invoices, then the journal (every document, or one
// opening entry per company) with the clearings SAP recorded, and last the
// posting blocks, which would refuse the postings before them.
//
// The load is one transaction that posts every document and then clears
// them one clearing at a time, so it would take the ledger's entry locks
// after its number-range locks and in several sets; it takes all of the
// tenant's entry locks first instead (Postings.LockAllEntries), so it never
// deadlocks with a payment proposal's reservation or a reversal.
func (l *loader) load(ctx context.Context) error {
	if err := l.e.Postings.LockAllEntries(ctx); err != nil {
		return err
	}
	for _, fn := range []func(context.Context) error{l.partners, l.orders, l.receipts, l.invoices, l.journal, l.clearings, l.blocked} {
		if err := fn(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (l *loader) partners(ctx context.Context) error {
	companies := map[string][]sap.LFB1{}
	for _, c := range l.ext.LFB1 {
		companies[c.LIFNR] = append(companies[c.LIFNR], c)
	}
	banks := map[string]sap.LFBK{}
	for _, b := range l.ext.LFBK {
		if _, ok := banks[b.LIFNR]; !ok {
			banks[b.LIFNR] = b
		}
	}
	vendors := slices.Clone(l.ext.LFA1)
	slices.SortFunc(vendors, func(a, b sap.LFA1) int { return cmp.Compare(a.LIFNR, b.LIFNR) })
	reason := "Migration run " + l.run.ID.String()
	for _, v := range vendors {
		var in []partner.SupplierCompanyInput
		for _, c := range companies[v.LIFNR] {
			in = append(in, partner.SupplierCompanyInput{LegalEntityID: l.t.entities[c.BUKRS].ID,
				ReconciliationAccountID: l.t.charts[c.BUKRS][sap.Alpha(c.AKONT)].ID, PaymentTermCode: c.ZTERM})
		}
		var err error
		if id, ok := l.refs["LFA1/"+v.LIFNR]; ok {
			err = l.extendSupplier(ctx, v, id, in, reason)
		} else {
			err = l.createSupplier(ctx, newSupplier(v, in, banks, reason, l.today), v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// extendSupplier sets an earlier run's supplier up in the company codes it
// is not yet set up in.
func (l *loader) extendSupplier(ctx context.Context, v sap.LFA1, id uuid.UUID, in []partner.SupplierCompanyInput, reason string) error {
	l.vendors[v.LIFNR] = id
	var missing []partner.SupplierCompanyInput
	for _, c := range in {
		// A partner not found has SetUp false, so the company is requested
		// from AddCompanies, which refuses an unknown partner.
		s, _, err := l.e.Suppliers.Supplier(ctx, id, c.LegalEntityID)
		if err != nil {
			return err
		}
		if !s.SetUp {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	done, err := l.step(ctx, "LFB1", v.LIFNR, func() error { return l.e.Partners.AddCompanies(ctx, id, missing, reason) })
	if err != nil {
		return err
	}
	if done {
		l.counts["suppliers_extended"]++
	}
	return nil
}

// newSupplier is the partner a vendor master becomes, with its tax number
// and its first bank account.
func newSupplier(v sap.LFA1, in []partner.SupplierCompanyInput, banks map[string]sap.LFBK, reason string, today time.Time) partner.NewPartner {
	np := partner.NewPartner{Kind: "organization", Name: v.NAME1, CountryCode: v.LAND1, Roles: []string{"supplier"},
		SupplierCompanies: in, Reason: reason}
	if tax := strings.TrimSpace(v.STCD1); tax != "" {
		np.TaxIdentifiers = []partner.TaxInput{{Scheme: v.LAND1 + "_TAX", Value: tax}}
	}
	if b, ok := banks[v.LIFNR]; ok {
		np.BankAccounts = []partner.BankInput{supplierBank(v, b, today)}
	}
	return np
}

// createSupplier creates the partner for a vendor this run sees first, and
// remembers it for the posting block that comes last.
func (l *loader) createSupplier(ctx context.Context, np partner.NewPartner, v sap.LFA1) error {
	var id uuid.UUID
	done, err := l.step(ctx, "LFA1", v.LIFNR, func() error {
		var err error
		if id, err = l.e.Partners.Create(ctx, np); err != nil {
			return err
		}
		return l.addRef(ctx, "partner.business_partner", id, "LFA1", v.LIFNR)
	})
	if err != nil {
		return err
	}
	if done {
		l.vendors[v.LIFNR] = id
		l.counts["suppliers"]++
		if v.SPERR == "X" {
			l.blocks = append(l.blocks, id)
		}
	}
	return nil
}

// itemNos numbers each order's items 1..n in SAP item order.
func (l *loader) itemNos() map[string]int32 {
	items := slices.Clone(l.ext.EKPO)
	slices.SortFunc(items, func(a, b sap.EKPO) int { return cmp.Compare(a.EBELN+"/"+a.EBELP, b.EBELN+"/"+b.EBELP) })
	out := map[string]int32{}
	next := map[string]int32{}
	for _, it := range items {
		next[it.EBELN]++
		out[it.EBELN+"/"+it.EBELP] = next[it.EBELN]
	}
	return out
}

func (l *loader) orders(ctx context.Context) error {
	items := map[string][]sap.EKPO{}
	for _, it := range l.ext.EKPO {
		items[it.EBELN] = append(items[it.EBELN], it)
	}
	heads := slices.Clone(l.ext.EKKO)
	slices.SortFunc(heads, func(a, b sap.EKKO) int { return cmp.Compare(a.EBELN, b.EBELN) })
	for _, h := range heads {
		if _, ok := l.refs["EKKO/"+h.EBELN]; ok {
			continue
		}
		date, _ := sapDate(h.BEDAT) // checked by the quality gate
		in := procurement.NewOrder{LegalEntityID: l.t.entities[h.BUKRS].ID, SupplierID: l.vendors[h.LIFNR], Currency: h.WAERS, OrderDate: date}
		lines := items[h.EBELN]
		slices.SortFunc(lines, func(a, b sap.EKPO) int { return cmp.Compare(a.EBELP, b.EBELP) })
		for _, it := range lines {
			price, _ := l.t.amount(it.NETPR, h.WAERS) // checked by the quality gate (item)
			priceUnit, _ := strconv.Atoi(it.PEINH)    // the gate admits only a positive number or empty; empty fails here and max makes it 1
			description := cmp.Or(it.TXZ01, it.MATNR, "Item "+it.EBELP)
			in.Items = append(in.Items, procurement.ItemInput{Description: description, Quantity: it.MENGE, Unit: unit,
				Price: l.plain(price, h.WAERS), Currency: h.WAERS, PriceUnit: int32(max(priceUnit, 1)), AccountID: l.t.charts[h.BUKRS][sap.Alpha(it.SAKTO)].ID})
		}
		done, err := l.step(ctx, "EKKO", h.EBELN, func() error {
			o, err := l.e.Orders.CreateOrder(ctx, in)
			if err != nil {
				return err
			}
			if _, err := l.e.Orders.ReleaseLoadedOrder(ctx, o.ID, func(int32) error { return nil }); err != nil {
				return err
			}
			return l.addRef(ctx, "procurement.purchase_order", o.ID, "EKKO", h.EBELN)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts["purchase_orders"]++
		}
	}
	return nil
}

func (l *loader) receipts(ctx context.Context) error {
	company := map[string]string{}
	for _, h := range l.ext.EKKO {
		company[h.EBELN] = h.BUKRS
	}
	nos := l.itemNos()
	docs := map[string][]sap.MSEG{}
	for _, m := range l.ext.MSEG {
		if m.BWART == "101" {
			docs[m.MBLNR+"/"+m.MJAHR] = append(docs[m.MBLNR+"/"+m.MJAHR], m)
		}
	}
	for _, key := range sortedKeys(docs) {
		if _, ok := l.refs["MSEG/"+key]; ok {
			continue
		}
		lines := docs[key]
		slices.SortFunc(lines, func(a, b sap.MSEG) int { return cmp.Compare(a.ZEILE, b.ZEILE) })
		date, err := sapDate(lines[0].BUDAT)
		if err != nil {
			l.issue("MSEG", key, err.Error())
			continue
		}
		ref := lines[0].MBLNR
		in := procurement.ReceiptInput{LegalEntityID: l.t.entities[company[lines[0].EBELN]].ID, DocumentDate: date, PostingDate: date, Reference: &ref}
		for _, m := range lines {
			in.Items = append(in.Items, procurement.ReceiptItem{PurchaseOrderID: l.refs["EKKO/"+m.EBELN], POItemNo: nos[m.EBELN+"/"+m.EBELP],
				Quantity: m.MENGE, Unit: unit})
		}
		done, err := l.step(ctx, "MSEG", key, func() error {
			g, err := l.e.Orders.PostReceipt(ctx, in)
			if err != nil {
				return err
			}
			return l.addRef(ctx, "procurement.goods_receipt", g.ID, "MSEG", key)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts["goods_receipts"]++
		}
	}
	return nil
}

func (l *loader) invoices(ctx context.Context) error {
	nos := l.itemNos()
	items := map[string][]sap.RSEG{}
	for _, it := range l.ext.RSEG {
		items[it.BELNR+"/"+it.GJAHR] = append(items[it.BELNR+"/"+it.GJAHR], it)
	}
	dups := duplicateInvoices(l.ext)
	heads := slices.Clone(l.ext.RBKP)
	slices.SortFunc(heads, func(a, b sap.RBKP) int { return cmp.Compare(a.BELNR+"/"+a.GJAHR, b.BELNR+"/"+b.GJAHR) })
	for _, h := range heads {
		key := h.BELNR + "/" + h.GJAHR
		if _, ok := l.refs["RBKP/"+key]; ok || h.RBSTAT != "A" || dups[key] != "" {
			continue
		}
		date, _ := sapDate(h.BLDAT)              // checked by the quality gate
		gross, _ := l.t.amount(h.RMWWR, h.WAERS) // checked by the quality gate
		tax, _ := l.t.amount(h.WMWST1, h.WAERS)  // checked by the quality gate
		in := payables.Input{LegalEntityID: l.t.entities[h.BUKRS].ID, SupplierID: l.vendors[h.LIFNR], SupplierReference: cmp.Or(h.XBLNR, h.BELNR),
			InvoiceDate: date, Gross: payables.Amount{Value: l.plain(gross, h.WAERS), Currency: h.WAERS},
			Tax: payables.Amount{Value: l.plain(tax, h.WAERS), Currency: h.WAERS}}
		lines := items[key]
		slices.SortFunc(lines, func(a, b sap.RSEG) int { return cmp.Compare(a.BUZEI, b.BUZEI) })
		for _, it := range lines {
			po, no := l.refs["EKKO/"+it.EBELN], nos[it.EBELN+"/"+it.EBELP]
			amt, _ := l.t.amount(it.WRBTR, h.WAERS) // checked by the quality gate
			if in.PurchaseOrderID == nil {
				in.PurchaseOrderID = &po
			}
			in.Items = append(in.Items, payables.ItemInput{PurchaseOrderID: &po, POItemNo: &no, Quantity: it.MENGE, Unit: unit,
				Amount: payables.Amount{Value: l.plain(amt, h.WAERS), Currency: h.WAERS}})
		}
		done, err := l.step(ctx, "RBKP", key, func() error {
			v, err := l.e.Invoices.CreateInvoice(ctx, in, "migration")
			if err != nil {
				return err
			}
			return l.addRef(ctx, "payables.supplier_invoice", v.ID, "RBKP", key)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts["supplier_invoices"]++
		}
	}
	return nil
}

// reconciliation reports whether the account is a vendor reconciliation
// account in the company.
func (l *loader) reconciliation(company, account string) bool {
	return l.t.charts[company][sap.Alpha(account)].ReconciliationKind == "supplier"
}

func documentKey(l sap.ACDOCA) string { return l.RBUKRS + "/" + l.GJAHR + "/" + l.BELNR }

func (l *loader) journal(ctx context.Context) error {
	if l.t.balances {
		return l.openingEntries(ctx)
	}
	docs := map[string][]sap.ACDOCA{}
	for _, line := range l.ext.ACDOCA {
		docs[documentKey(line)] = append(docs[documentKey(line)], line)
	}
	currency := map[string]string{}
	for _, c := range l.ext.T001 {
		currency[c.BUKRS] = c.WAERS
	}
	for _, key := range sortedKeys(docs) {
		if _, ok := l.refs["ACDOCA/"+key]; ok {
			continue
		}
		if err := l.journalDocument(ctx, key, docs[key], currency); err != nil {
			return err
		}
	}
	return nil
}

// journalDocument posts one SAP document as a journal entry and keeps its
// line numbers for the clearings that follow.
func (l *loader) journalDocument(ctx context.Context, key string, lines []sap.ACDOCA, currency map[string]string) error {
	in, posted := l.journalPosting(lines, currency)
	done, err := l.step(ctx, "ACDOCA", key, func() error {
		entry, err := l.e.Postings.Post(ctx, in)
		if err != nil {
			return err
		}
		posted.ref = ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID}
		return l.addRef(ctx, "ledger.journal_entry", entry.ID, "ACDOCA", key)
	})
	if err != nil {
		return err
	}
	if done {
		l.entries[key] = posted
		l.counts["journal_entries"]++
	}
	return nil
}

// journalPosting is the posting of one SAP document's lines, in SAP line
// order; zero lines are left out, so it also maps each SAP line to its
// platform line number.
func (l *loader) journalPosting(lines []sap.ACDOCA, currency map[string]string) (ledger.Posting, loadedEntry) {
	slices.SortFunc(lines, func(a, b sap.ACDOCA) int { return cmp.Compare(a.DOCLN, b.DOCLN) })
	first := lines[0]
	date, _ := sapDate(first.BUDAT) // checked by the quality gate
	text := "SAP document " + first.BELNR
	in := ledger.Posting{LegalEntityID: l.t.entities[first.RBUKRS].ID, DocumentType: first.BLART, DocumentDate: date, PostingDate: date,
		Currency: currency[first.RBUKRS], HeaderText: &text, SourceContext: "integration", SourceDocumentID: l.run.ID}
	if first.XBLNR != "" {
		in.Reference = &first.XBLNR
	}
	posted := loadedEntry{lines: map[string]int32{}, date: date}
	for _, line := range lines {
		amt, _ := l.t.amount(line.HSL, in.Currency) // checked by the quality gate (journal)
		if amt == 0 {
			continue
		}
		pl := ledger.PostingLine{AccountID: l.t.charts[line.RBUKRS][sap.Alpha(line.RACCT)].ID, Amount: amt}
		if l.reconciliation(line.RBUKRS, line.RACCT) {
			partnerID := l.vendors[line.LIFNR]
			pl.PartnerID = &partnerID
		}
		in.Lines = append(in.Lines, pl)
		posted.lines[line.DOCLN] = int32(len(in.Lines))
	}
	return in, posted
}

// openingEntries posts one opening entry per company: the balance of each
// account, with the open vendor items kept individually so they can still
// be paid; cleared vendor items net to zero and are left out.
func (l *loader) openingEntries(ctx context.Context) error {
	for _, c := range l.ext.T001 {
		key := "OB/" + c.BUKRS + "/" + strconv.Itoa(int(l.t.year))
		if _, ok := l.refs["ACDOCA/"+key]; ok {
			continue
		}
		if err := l.openingEntry(ctx, c, key); err != nil {
			return err
		}
	}
	return nil
}

func (l *loader) openingEntry(ctx context.Context, c sap.T001, key string) error {
	lines, last, cleared := l.openingLines(c)
	if cleared != 0 {
		l.issue("ACDOCA", c.BUKRS, "cleared vendor items do not net to zero; the clearing data is inconsistent")
		return nil
	}
	if len(lines) == 0 {
		return nil
	}
	date, _ := sapDate(last) // the latest of the lines' dates, each checked by the quality gate
	text := "Opening balances from SAP " + strconv.Itoa(int(l.t.year))
	in := ledger.Posting{LegalEntityID: l.t.entities[c.BUKRS].ID, DocumentType: "OB", DocumentDate: date, PostingDate: date, Currency: c.WAERS,
		HeaderText: &text, SourceContext: "integration", SourceDocumentID: l.run.ID, Lines: lines}
	done, err := l.step(ctx, "ACDOCA", key, func() error {
		entry, err := l.e.Postings.Post(ctx, in)
		if err != nil {
			return err
		}
		return l.addRef(ctx, "ledger.journal_entry", entry.ID, "ACDOCA", key)
	})
	if err != nil {
		return err
	}
	if done {
		l.counts["journal_entries"]++
	}
	return nil
}

// openingLines are a company's opening entry lines: account balances in
// account order, then the open vendor items. It also returns the latest
// posting date, the entry's date, and the sum of the cleared vendor items,
// which must be zero.
func (l *loader) openingLines(c sap.T001) ([]ledger.PostingLine, string, money.Minor) {
	balances := map[string]money.Minor{}
	var open []ledger.PostingLine
	var cleared money.Minor
	var last string
	for _, line := range l.ext.ACDOCA {
		if line.RBUKRS != c.BUKRS {
			continue
		}
		// Parsed and in range: the quality gate refuses an extract whose
		// company lines add up to more than an amount holds (journal), which
		// bounds the balances and cleared sum here.
		amt, _ := l.t.amount(line.HSL, c.WAERS)
		last = max(last, line.BUDAT)
		account := sap.Alpha(line.RACCT)
		switch {
		case !l.reconciliation(c.BUKRS, line.RACCT):
			balances[account] += amt
		case line.AUGBL != "":
			cleared += amt
		case amt != 0:
			partnerID := l.vendors[line.LIFNR]
			text := "Open item " + cmp.Or(line.XBLNR, line.BELNR) + " (SAP " + line.BELNR + ")"
			open = append(open, ledger.PostingLine{AccountID: l.t.charts[c.BUKRS][account].ID, Amount: amt, PartnerID: &partnerID, Text: &text})
		}
	}
	var lines []ledger.PostingLine
	for _, account := range sortedKeys(balances) {
		if balances[account] != 0 {
			lines = append(lines, ledger.PostingLine{AccountID: l.t.charts[c.BUKRS][account].ID, Amount: balances[account]})
		}
	}
	return append(lines, open...), last, cleared
}

// clearings clears the vendor items SAP cleared, by the migrated clearing
// document, for clearings whose documents this run loaded.
func (l *loader) clearings(ctx context.Context) error {
	if l.t.balances {
		return nil
	}
	groups := map[string][]sap.ACDOCA{}
	for _, line := range l.ext.ACDOCA {
		if line.AUGBL != "" && l.reconciliation(line.RBUKRS, line.RACCT) {
			groups[line.RBUKRS+"/"+line.AUGBL] = append(groups[line.RBUKRS+"/"+line.AUGBL], line)
		}
	}
	for _, key := range sortedKeys(groups) {
		items, by, complete := l.clearingItems(groups[key])
		if !complete {
			continue
		}
		done, err := l.step(ctx, "ACDOCA", "clearing "+key, func() error {
			_, err := l.e.Postings.Clear(ctx, l.t.entities[groups[key][0].RBUKRS].ID, by.date, by.ref, items)
			return err
		})
		if err != nil {
			return err
		}
		if done {
			l.counts["clearings"]++
		}
	}
	return nil
}

// clearingItems are the platform items of one SAP clearing and the entry
// that clears them: the clearing document, or the latest one when the
// clearing document is not in the scope. It reports false when a document
// of the clearing was not posted by this run.
func (l *loader) clearingItems(group []sap.ACDOCA) ([]ledger.ItemRef, *loadedEntry, bool) {
	var items []ledger.ItemRef
	var by, latest *loadedEntry
	for _, line := range group {
		e, ok := l.entries[documentKey(line)]
		if !ok {
			return nil, nil, false // loaded by an earlier run, or refused
		}
		if line.BELNR == line.AUGBL {
			by = &e
		}
		if latest == nil || e.date.After(latest.date) {
			latest = &e
		}
		ref := e.ref
		ref.LineNo = e.lines[line.DOCLN]
		items = append(items, ref)
	}
	if by == nil { // the clearing document is not in the scope: the latest one clears
		by = latest
	}
	return items, by, true
}

func (l *loader) blocked(ctx context.Context) error {
	for _, id := range l.blocks {
		done, err := l.step(ctx, "LFA1", id.String(), func() error { return l.e.Partners.Block(ctx, id, "Posting-blocked in SAP") })
		if err != nil {
			return err
		}
		if done {
			l.counts["suppliers_blocked"]++
		}
	}
	return nil
}

// refusal is the issue of a refused row: the problem's code and detail,
// and each field error, which names what refused it (a field, a rule) and
// repeats no value.
func refusal(p *problem.Problem) string {
	msg := p.Code + ": " + p.Detail
	for _, f := range p.Errors {
		msg += " " + f.Field + ": " + f.Message
	}
	return msg
}
