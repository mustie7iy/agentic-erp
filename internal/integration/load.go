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

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
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
		l.issue(table, key, p.Code+": "+p.Detail)
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

func (l *loader) plain(minor int64, currency string) string {
	return money.Minor(minor).Plain(l.t.exponents[currency])
}

// load brings the extract over: suppliers, purchase orders, goods
// receipts, parked invoices, then the journal (every document, or one
// opening entry per company) with the clearings SAP recorded, and last the
// posting blocks, which would refuse the postings before them.
func (l *loader) load(ctx context.Context) error {
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
				ReconciliationAccountID: l.t.charts[c.BUKRS][alpha(c.AKONT)].ID, PaymentTermCode: c.ZTERM})
		}
		if id, ok := l.refs["LFA1/"+v.LIFNR]; ok {
			l.vendors[v.LIFNR] = id
			var missing []partner.SupplierCompanyInput
			for _, c := range in {
				s, _, err := l.e.Suppliers.Supplier(ctx, id, c.LegalEntityID)
				if err != nil {
					return err
				}
				if !s.SetUp {
					missing = append(missing, c)
				}
			}
			if len(missing) == 0 {
				continue
			}
			done, err := l.step(ctx, "LFB1", v.LIFNR, func() error { return l.e.Partners.AddCompanies(ctx, id, missing, reason) })
			if err != nil {
				return err
			}
			if done {
				l.counts["suppliers_extended"]++
			}
			continue
		}
		np := partner.NewPartner{Kind: "organization", Name: v.NAME1, CountryCode: v.LAND1, Roles: []string{"supplier"},
			SupplierCompanies: in, Reason: reason}
		if tax := strings.TrimSpace(v.STCD1); tax != "" {
			np.TaxIdentifiers = []partner.TaxInput{{Scheme: v.LAND1 + "_TAX", Value: tax}}
		}
		if b, ok := banks[v.LIFNR]; ok {
			scheme := "domestic"
			if bankfile.ValidIBAN(b.BANKN) {
				scheme = "iban"
			}
			np.BankAccounts = []partner.BankInput{{Purpose: "payments", AccountNumber: b.BANKN, AccountScheme: scheme, BankCountryCode: b.BANKS,
				HolderName: v.NAME1, ValidFrom: l.today}}
		}
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
			price, _ := l.t.amount(it.NETPR, h.WAERS)
			priceUnit, _ := strconv.Atoi(it.PEINH)
			description := cmp.Or(it.TXZ01, it.MATNR, "Item "+it.EBELP)
			in.Items = append(in.Items, procurement.ItemInput{Description: description, Quantity: it.MENGE, Unit: unit,
				Price: l.plain(price, h.WAERS), Currency: h.WAERS, PriceUnit: int32(max(priceUnit, 1)), AccountID: l.t.charts[h.BUKRS][alpha(it.SAKTO)].ID})
		}
		done, err := l.step(ctx, "EKKO", h.EBELN, func() error {
			o, err := l.e.Orders.CreateOrder(ctx, in)
			if err != nil {
				return err
			}
			if _, err := l.e.Orders.ReleaseOrder(ctx, o.ID, func(int32) error { return nil }); err != nil {
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
		date, _ := sapDate(h.BLDAT) // checked by the quality gate
		gross, _ := l.t.amount(h.RMWWR, h.WAERS)
		tax, _ := l.t.amount(h.WMWST1, h.WAERS)
		in := payables.Input{LegalEntityID: l.t.entities[h.BUKRS].ID, SupplierID: l.vendors[h.LIFNR], SupplierReference: cmp.Or(h.XBLNR, h.BELNR),
			InvoiceDate: date, Gross: payables.Amount{Value: l.plain(gross, h.WAERS), Currency: h.WAERS},
			Tax: payables.Amount{Value: l.plain(tax, h.WAERS), Currency: h.WAERS}}
		lines := items[key]
		slices.SortFunc(lines, func(a, b sap.RSEG) int { return cmp.Compare(a.BUZEI, b.BUZEI) })
		for _, it := range lines {
			po, no := l.refs["EKKO/"+it.EBELN], nos[it.EBELN+"/"+it.EBELP]
			amt, _ := l.t.amount(it.WRBTR, h.WAERS)
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
	return l.t.charts[company][alpha(account)].ReconciliationKind == "supplier"
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
		lines := docs[key]
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
			amt, _ := l.t.amount(line.HSL, in.Currency)
			if amt == 0 {
				continue
			}
			pl := ledger.PostingLine{AccountID: l.t.charts[line.RBUKRS][alpha(line.RACCT)].ID, Amount: amt}
			if l.reconciliation(line.RBUKRS, line.RACCT) {
				partnerID := l.vendors[line.LIFNR]
				pl.PartnerID = &partnerID
			}
			in.Lines = append(in.Lines, pl)
			posted.lines[line.DOCLN] = int32(len(in.Lines))
		}
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
	}
	return nil
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
		balances := map[string]int64{}
		var open []ledger.PostingLine
		var cleared int64
		var last string
		for _, line := range l.ext.ACDOCA {
			if line.RBUKRS != c.BUKRS {
				continue
			}
			amt, _ := l.t.amount(line.HSL, c.WAERS)
			last = max(last, line.BUDAT)
			account := alpha(line.RACCT)
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
		if cleared != 0 {
			l.issue("ACDOCA", c.BUKRS, "cleared vendor items do not net to zero; the clearing data is inconsistent")
			continue
		}
		var lines []ledger.PostingLine
		for _, account := range sortedKeys(balances) {
			if balances[account] != 0 {
				lines = append(lines, ledger.PostingLine{AccountID: l.t.charts[c.BUKRS][account].ID, Amount: balances[account]})
			}
		}
		lines = append(lines, open...)
		if len(lines) == 0 {
			continue
		}
		date, _ := sapDate(last)
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
	}
	return nil
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
		var items []ledger.ItemRef
		var by, latest *loadedEntry
		complete := true
		for _, line := range groups[key] {
			e, ok := l.entries[documentKey(line)]
			if !ok {
				complete = false // loaded by an earlier run, or refused
				break
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
		if !complete {
			continue
		}
		if by == nil { // the clearing document is not in the scope: the latest one clears
			by = latest
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
