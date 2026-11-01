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
	counts  LoadedCounts
	vendors map[string]uuid.UUID // LIFNR -> partner
	blocks  []block
	entries map[string]loadedEntry // ACDOCA document key -> entry posted by this run
	// downPayments are the documents holding an open down payment, which
	// load as entries of their own in either mode (downPaymentDocs).
	downPayments map[string][]sap.ACDOCA
	// grir are the extract's GR/IR positions by EBELN/EBELP, and viaGRIR
	// the orders this run released valued through GR/IR, whose open
	// positions it takes over (T72f, takeOverGRIR).
	grir    map[string]*grirPosition
	viaGRIR map[string]bool
	// foreign are the documents posted in their foreign document currency,
	// with that currency (foreignDocuments).
	foreign map[string]string
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

// addRef records the platform record loaded for an SAP key, and the run
// that loaded it, by which a later run tells the fiscal year of the run
// that loaded an order (cannotTopUp).
func (l *loader) addRef(ctx context.Context, entityType string, id uuid.UUID, object, key string) error {
	l.refs[object+"/"+key] = id
	_, err := l.tx.Exec(ctx, `INSERT INTO integration.external_ref (tenant_id, entity_type, entity_id, system, object, external_key, created_by,
		migration_run_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, l.p.TenantID, entityType, id, l.system, object, key, l.p.ID, l.run.ID)
	if err != nil {
		return fmt.Errorf("integration: record the external reference %s/%s: %w", object, key, err)
	}
	return nil
}

func (l *loader) plain(minor money.Minor, currency string) string {
	return minor.Plain(l.t.exponents[currency])
}

// load brings the extract over: suppliers, purchase orders, the
// quantities posted invoices invoiced, goods receipts, the open GR/IR
// positions of the orders SAP valued through GR/IR, parked invoices,
// then the journal (every document, or one
// opening entry per company) with the clearings SAP recorded, and last the
// posting blocks, which would refuse the postings before them.
//
// The load is one transaction that posts every document and then clears
// them one clearing at a time, so it would take the ledger's entry locks
// after its number-range locks and in several sets; it takes all of the
// tenant's entry locks first instead (Postings.LockAllEntries), so it never
// deadlocks with a payment proposal's reservation or a reversal. The
// take-over of invoiced quantities and the receipts lock the items of
// orders an earlier run may have loaded, and a goods movement (POST
// /v1/goods-receipts, its reversal) or an invoice's posting takes those
// items first and then the gapless number ranges (goods_receipt, then the
// orders' GR/IR locks and journal_we), which the receipts and the journal
// hold until the load commits. So the load takes every one of those item
// locks at once, in the order a goods movement takes them
// (lockOrderItems), before the take-over and the first receipt; it never
// waits for an order item while it holds a range. Before it the load
// holds only the partner and purchase order ranges, which nothing that
// locks order items takes, and its shared entry bucket locks, which
// nothing takes exclusively after order items (a goods movement's
// reversal posts a new entry, an invoice's reversal locks no item).
// Suppliers given payment methods take the partner context's method lock
// before the load draws from any configuration range, the reverse of an
// activation's order; the run holds the active configuration from before
// either (prepare, Settings.Hold), so an activation waits for the load.
func (l *loader) load(ctx context.Context) error {
	if err := l.e.Postings.LockAllEntries(ctx); err != nil {
		return err
	}
	l.downPayments, l.foreign = downPaymentDocs(l.ext), foreignDocuments(l.ext, l.t)
	l.grir, l.viaGRIR = grirPositions(l.ext, l.t), map[string]bool{}
	for _, fn := range []func(context.Context) error{l.partners, l.orders, l.lockOrderItems, l.invoiced, l.receipts, l.takeOverGRIR, l.invoices, l.journal, l.clearings,
		l.blocked} {
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
	banks := firstBanks(l.ext)
	vendors := slices.Clone(l.ext.LFA1)
	slices.SortFunc(vendors, func(a, b sap.LFA1) int { return cmp.Compare(a.LIFNR, b.LIFNR) })
	reason := "Migration run " + l.run.ID.String()
	for _, v := range vendors {
		var in []partner.SupplierCompanyInput
		for _, c := range companies[v.LIFNR] {
			in = append(in, partner.SupplierCompanyInput{LegalEntityID: l.t.entities[c.BUKRS].ID,
				ReconciliationAccountID: l.t.charts[c.BUKRS][sap.Alpha(c.AKONT)].ID, PaymentTermCode: c.ZTERM, PaymentMethods: paymentMethods(c.ZWELS)})
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
		l.counts.SuppliersExtended++
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
		l.counts.Suppliers++
		if v.SPERR == "X" {
			l.blocks = append(l.blocks, block{lifnr: v.LIFNR, partner: id})
		}
	}
	return nil
}

// itemNos numbers each order's items 1..n in SAP item order.
func itemNos(e *sap.Extract) map[string]int32 {
	items := slices.Clone(e.EKPO)
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
	via := grirOrders(l.grir)
	for _, h := range heads {
		if _, ok := l.refs["EKKO/"+h.EBELN]; ok {
			continue
		}
		valuation := procurement.ValuationDirect
		if via[h.EBELN] {
			valuation = procurement.ValuationGRIR
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
			if _, err := l.e.Orders.ReleaseLoadedOrder(ctx, o.ID, func(int32) error { return nil }, valuation); err != nil {
				return err
			}
			return l.addRef(ctx, "procurement.purchase_order", o.ID, "EKKO", h.EBELN)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts.PurchaseOrders++
			l.viaGRIR[h.EBELN] = valuation == procurement.ValuationGRIR
		}
	}
	return nil
}

// receipts records the goods receipts not yet loaded, under the order item
// locks lockOrderItems took.
func (l *loader) receipts(ctx context.Context) error {
	company := map[string]string{}
	for _, h := range l.ext.EKKO {
		company[h.EBELN] = h.BUKRS
	}
	nos := itemNos(l.ext)
	docs := receiptDocs(l.ext)
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
			g, err := l.e.Orders.PostLoadedReceipt(ctx, in)
			if err != nil {
				return err
			}
			return l.addRef(ctx, "procurement.goods_receipt", g.ID, "MSEG", key)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts.GoodsReceipts++
		}
	}
	return nil
}

// invoices enters the parked SAP invoice documents, each as its kind
// (parkedKinds, T72b), so a credit memo is never a positive invoice that a
// payment run pays.
func (l *loader) invoices(ctx context.Context) error {
	nos := itemNos(l.ext)
	items := invoiceItems(l.ext)
	kinds := parkedKinds(l.ext)
	heads := slices.Clone(l.ext.RBKP)
	slices.SortFunc(heads, func(a, b sap.RBKP) int { return cmp.Compare(a.BELNR+"/"+a.GJAHR, b.BELNR+"/"+b.GJAHR) })
	for _, h := range heads {
		key := h.BELNR + "/" + h.GJAHR
		kind, ok := kinds[key]
		if _, loaded := l.refs["RBKP/"+key]; loaded || !ok {
			continue
		}
		in := l.invoiceInput(h, kind, items[key], nos)
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
			l.counts.SupplierInvoices++
		}
	}
	return nil
}

// invoiced takes over what SAP's posted invoice documents invoiced and
// credited on the orders on the platform (T72b2), so that matching a
// migrated credit memo or subsequent debit/credit, or a later invoice,
// counts it. A document is taken over whole, once (its RBKP reference of
// the source system, which also names it to payables):
// it waits until every order of the extract it names is loaded, and its
// items on orders outside the extract, none of the platform's, are left
// out. Documents the quality gate refused are not taken over.
func (l *loader) invoiced(ctx context.Context) error {
	nos := itemNos(l.ext)
	inExtract := map[string]bool{}
	for _, h := range l.ext.EKKO {
		inExtract[h.EBELN] = true
	}
	docs, _ := postedDocuments(l.ext, "") // the issues are the quality gate's, which stopped the load on any
	for _, d := range docs {
		if _, loaded := l.refs["RBKP/"+d.key]; loaded {
			continue
		}
		in, ready := l.migratedInvoice(d, nos, inExtract)
		if !ready || len(in.Items) == 0 {
			continue
		}
		done, err := l.step(ctx, "RBKP", d.key, func() error {
			id, err := l.e.Invoices.TakeOverInvoiced(ctx, in)
			if err != nil {
				return err
			}
			return l.addRef(ctx, "payables.migrated_invoice", id, "RBKP", d.key)
		})
		if err != nil {
			return err
		}
		if done {
			l.counts.PostedInvoicesInvoiced++
		}
	}
	return nil
}

// migratedInvoice is what a posted document takes over, and whether every
// order of the extract it names is on the platform.
func (l *loader) migratedInvoice(d postedDocument, nos map[string]int32, inExtract map[string]bool) (payables.MigratedInvoice, bool) {
	in := payables.MigratedInvoice{Reference: l.system + " " + d.key, Kind: d.kind}
	for _, it := range d.items {
		if !inExtract[it.EBELN] {
			continue
		}
		po, ok := l.refs["EKKO/"+it.EBELN]
		if !ok {
			return in, false
		}
		in.Items = append(in.Items, payables.MigratedItem{ItemNo: it.no, PurchaseOrderID: po, POItemNo: nos[it.EBELN+"/"+it.EBELP], Quantity: it.MENGE})
	}
	return in, true
}

// invoiceInput is the payables document of one parked SAP document of the
// kind. A subsequent debit or credit moves no quantity, so its items are
// order items of quantity 0, as payables keeps them (03 §3.9), whatever
// quantity SAP proposed for them; amounts stay as SAP holds them, not
// negative, the kind giving the direction.
func (l *loader) invoiceInput(h sap.RBKP, kind string, lines []sap.RSEG, nos map[string]int32) payables.Input {
	date, _ := sapDate(h.BLDAT)              // checked by the quality gate
	gross, _ := l.t.amount(h.RMWWR, h.WAERS) // checked by the quality gate
	tax, _ := l.t.amount(h.WMWST1, h.WAERS)  // checked by the quality gate
	in := payables.Input{LegalEntityID: l.t.entities[h.BUKRS].ID, SupplierID: l.vendors[h.LIFNR], SupplierReference: cmp.Or(h.XBLNR, h.BELNR),
		InvoiceDate: date, Gross: payables.Amount{Value: l.plain(gross, h.WAERS), Currency: h.WAERS},
		Tax: payables.Amount{Value: l.plain(tax, h.WAERS), Currency: h.WAERS}, Kind: kind}
	slices.SortFunc(lines, func(a, b sap.RSEG) int { return cmp.Compare(a.BUZEI, b.BUZEI) })
	for _, it := range lines {
		po, no := l.refs["EKKO/"+it.EBELN], nos[it.EBELN+"/"+it.EBELP]
		amt, _ := l.t.amount(it.WRBTR, h.WAERS) // checked by the quality gate
		if in.PurchaseOrderID == nil {
			in.PurchaseOrderID = &po
		}
		qty := it.MENGE
		if kind == payables.KindSubsequentDebit || kind == payables.KindSubsequentCredit {
			qty = "0"
		}
		in.Items = append(in.Items, payables.ItemInput{PurchaseOrderID: &po, POItemNo: &no, Quantity: qty, Unit: unit,
			Amount: payables.Amount{Value: l.plain(amt, h.WAERS), Currency: h.WAERS}})
	}
	return in
}

// reconciliation reports whether the account is a vendor reconciliation
// account in the company.
func (l *loader) reconciliation(company, account string) bool {
	return l.t.charts[company][sap.Alpha(account)].ReconciliationKind == "supplier"
}

func documentKey(l sap.ACDOCA) string { return l.RBUKRS + "/" + l.GJAHR + "/" + l.BELNR }

// ownEntry reports whether a document posts as an entry of its own in
// balances mode, beside the opening entry: one holding an open down
// payment, or an open item in a foreign document currency.
func (l *loader) ownEntry(key string) bool { return l.downPayments[key] != nil || l.foreign[key] != "" }

// journal posts the extract's documents, each as a journal entry, or in
// balances mode one opening entry per company and the documents that post
// on their own (ownEntry), which the opening entry leaves out: a down
// payment's as the platform posts a down payment, an entry of its own
// whose tax the final invoice's clearing reverses (T72c), and one holding
// an open item in a foreign currency in that currency (foreignDocuments).
func (l *loader) journal(ctx context.Context) error {
	if l.t.balances {
		if err := l.openingEntries(ctx); err != nil {
			return err
		}
	}
	docs := map[string][]sap.ACDOCA{}
	for _, line := range l.ext.ACDOCA {
		if key := documentKey(line); !l.t.balances || l.ownEntry(key) {
			docs[key] = append(docs[key], line)
		}
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
// line numbers for the clearings that follow. A document holding an open
// down payment is also taken over as a paid down payment request, in the
// same savepoint, so a refusal of either leaves neither. The quality gate
// refuses every take-over refusal that comes from the extract first, so
// the shared savepoint is defence in depth (TestTakeOverDownPayment shows
// the refusal propagating).
func (l *loader) journalDocument(ctx context.Context, key string, lines []sap.ACDOCA, currency map[string]string) error {
	in, posted := l.journalPosting(lines, currency)
	_, downPayment := l.downPayments[key]
	done, err := l.step(ctx, "ACDOCA", key, func() error {
		entry, err := l.e.Postings.Post(ctx, in)
		if err != nil {
			return err
		}
		posted.ref = ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID}
		if err := l.addRef(ctx, "ledger.journal_entry", entry.ID, "ACDOCA", key); err != nil {
			return err
		}
		if downPayment {
			return l.takeOverDownPayment(ctx, key, lines, entry)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if done {
		l.entries[key] = posted
		l.counts.JournalEntries++
		if downPayment {
			l.counts.DownPayments++
		}
	}
	return nil
}

// journalPosting is the posting of one SAP document's lines, in SAP line
// order, in company code currency or in its foreign document currency
// (foreignDocuments) at the rate SAP's amounts imply; lines of no company
// code amount are left out (in a foreign currency they have no document
// amount either, which the quality gate checks), so it also maps each SAP
// line to its platform line number, the same in every run.
func (l *loader) journalPosting(lines []sap.ACDOCA, currency map[string]string) (ledger.Posting, loadedEntry) {
	slices.SortFunc(lines, func(a, b sap.ACDOCA) int { return cmp.Compare(a.DOCLN, b.DOCLN) })
	first := lines[0]
	date, _ := sapDate(first.BUDAT) // checked by the quality gate
	text := "SAP document " + first.BELNR
	local := currency[first.RBUKRS]
	in := ledger.Posting{LegalEntityID: l.t.entities[first.RBUKRS].ID, DocumentType: first.BLART, DocumentDate: date, PostingDate: date,
		Currency: local, HeaderText: &text, SourceContext: "integration", SourceDocumentID: l.run.ID}
	if foreign := l.foreign[documentKey(first)]; foreign != "" {
		in.Currency = foreign
		in.Rate, _ = sourceRate(lines, foreign, local, l.t) // the quality gate refuses a document without one
	}
	if first.XBLNR != "" {
		in.Reference = &first.XBLNR
	}
	posted := loadedEntry{lines: map[string]int32{}, date: date}
	var dp *sap.ACDOCA
	if open := openDownPayments(lines); len(open) == 1 {
		dp = &open[0]
	}
	for _, line := range lines {
		amt, lc := l.lineAmounts(line, local) // checked by the quality gate (journal, foreignDocument)
		if lc == nil && amt == 0 || lc != nil && *lc == 0 {
			continue
		}
		pl := ledger.PostingLine{AccountID: l.t.charts[line.RBUKRS][sap.Alpha(line.RACCT)].ID, Amount: amt, LC: lc}
		if l.reconciliation(line.RBUKRS, line.RACCT) {
			partnerID := l.vendors[line.LIFNR]
			pl.PartnerID = &partnerID
		}
		downPaymentPosting(&pl, line, dp)
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
		l.counts.JournalEntries++
	}
	return nil
}

// openingLines are a company's opening entry lines: account balances in
// account order, then the open vendor items; the documents holding an
// open down payment are left out, posted on their own (journal). It also
// returns the latest
// posting date, the entry's date, and the sum of the cleared vendor items,
// which must be zero.
func (l *loader) openingLines(c sap.T001) ([]ledger.PostingLine, string, money.Minor) {
	balances := map[string]money.Minor{}
	var open []ledger.PostingLine
	var cleared money.Minor
	var last string
	for _, line := range l.ext.ACDOCA {
		if line.RBUKRS != c.BUKRS || l.ownEntry(documentKey(line)) {
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
// document, for clearings whose documents are on the platform: posted by
// this run, or by an earlier run of the fiscal year (review:batch22), so
// a delta run finds the open item an earlier run loaded and clears it
// with its own. A clearing whose documents earlier runs posted and
// cleared together already is left as it is, so the same extract again
// records nothing.
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
	earlier, err := l.earlierEntries(ctx, groups)
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(groups) {
		items, by, ours, complete := l.clearingItems(groups[key], earlier)
		if !complete {
			continue
		}
		if !ours {
			done, err := l.e.Postings.ClearedTogether(ctx, items)
			if err != nil {
				return err
			}
			if done {
				continue
			}
		}
		if err := l.clear(ctx, key, groups[key][0].RBUKRS, by, items); err != nil {
			return err
		}
	}
	return nil
}

// clear records one SAP clearing on the platform.
func (l *loader) clear(ctx context.Context, key, company string, by *loadedEntry, items []ledger.ItemRef) error {
	done, err := l.step(ctx, "ACDOCA", "clearing "+key, func() error {
		_, err := l.e.Postings.Clear(ctx, l.t.entities[company].ID, by.date, by.ref, items)
		return err
	})
	if done {
		l.counts.Clearings++
	}
	return err
}

// earlierEntries are the entries earlier runs posted for the documents of
// the clearings, by ACDOCA document key, with their line numbers as the
// earlier run numbered them (journalPosting, from the same SAP lines: a
// posted SAP document's lines do not change).
func (l *loader) earlierEntries(ctx context.Context, groups map[string][]sap.ACDOCA) (map[string]loadedEntry, error) {
	docs := map[string][]sap.ACDOCA{}
	var ids []uuid.UUID
	for _, group := range groups {
		for _, line := range group {
			key := documentKey(line)
			id, loaded := l.refs["ACDOCA/"+key]
			_, ours := l.entries[key]
			if _, seen := docs[key]; ours || !loaded || seen {
				continue
			}
			docs[key] = nil
			ids = append(ids, id)
		}
	}
	out := map[string]loadedEntry{}
	if len(ids) == 0 {
		return out, nil
	}
	years, err := l.e.Postings.EntryYears(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, line := range l.ext.ACDOCA {
		if _, ok := docs[documentKey(line)]; ok {
			docs[documentKey(line)] = append(docs[documentKey(line)], line)
		}
	}
	currency := map[string]string{}
	for _, c := range l.ext.T001 {
		currency[c.BUKRS] = c.WAERS
	}
	for key, lines := range docs {
		_, e := l.journalPosting(lines, currency)
		id := l.refs["ACDOCA/"+key]
		e.ref = ledger.ItemRef{FiscalYear: years[id], EntryID: id}
		out[key] = e
	}
	return out, nil
}

// clearingItems are the platform items of one SAP clearing and the entry
// that clears them: the clearing document, or the latest one when the
// clearing document is not in the scope. It reports whether this run
// posted any of the clearing's documents, and false for complete when a
// document of the clearing is on the platform from neither this run nor
// an earlier one (refused).
func (l *loader) clearingItems(group []sap.ACDOCA, earlier map[string]loadedEntry) (items []ledger.ItemRef, by *loadedEntry, ours, complete bool) {
	var latest *loadedEntry
	for _, line := range group {
		e, mine := l.entries[documentKey(line)]
		if !mine {
			var ok bool
			if e, ok = earlier[documentKey(line)]; !ok {
				return nil, nil, false, false // refused
			}
		}
		ours = ours || mine
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
	return items, by, ours, true
}

// block is a supplier the load blocks for posting once everything else
// loaded, by the vendor's number, which a refusal is reported under, and
// its partner.
type block struct {
	lifnr   string
	partner uuid.UUID
}

func (l *loader) blocked(ctx context.Context) error {
	for _, b := range l.blocks {
		done, err := l.step(ctx, "LFA1", b.lifnr, func() error { return l.e.Partners.Block(ctx, b.partner, "Posting-blocked in SAP") })
		if err != nil {
			return err
		}
		if done {
			l.counts.SuppliersBlocked++
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
