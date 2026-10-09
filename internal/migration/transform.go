package migration

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Mode selects how financial history is brought across.
type Mode string

const (
	// ModeDocuments replays every Universal Journal document. Full audit
	// trail in the new system; best for the current fiscal year.
	ModeDocuments Mode = "documents"
	// ModeBalances loads one opening entry per company: account balances,
	// with open vendor items kept individually so they can still be paid.
	ModeBalances Mode = "balances"
)

// Dataset is the transformed, not-yet-loaded result.
type Dataset struct {
	Companies []*core.Company
	Accounts  []*core.GLAccount
	Vendors   []*core.Vendor
	POs       []*core.PurchaseOrder
	Receipts  []*core.GoodsReceipt
	Invoices  []*core.SupplierInvoice
	Entries   []*core.JournalEntry
}

// SAP document identifiers are kept in IDs so every migrated record can be
// traced back to its source document.
func entryID(cc, gjahr, belnr string) string { return fmt.Sprintf("SAP-%s-%s-%s", cc, gjahr, belnr) }
func invoiceID(belnr, gjahr string) string   { return fmt.Sprintf("SAPIV-%s-%s", belnr, gjahr) }

// Transform maps an extract onto the core model. Records that cannot be
// mapped are reported as issues instead of silently dropped.
func Transform(e *sap.Extract, m Mapping, mode Mode) (*Dataset, []Issue) {
	ds := &Dataset{}
	var issues []Issue
	fail := func(table, key string, err error) {
		issues = append(issues, Issue{Severity: SevError, Table: table, Key: key, Message: err.Error()})
	}

	currency := map[string]string{}
	for _, c := range e.T001 {
		currency[c.BUKRS] = c.WAERS
		ds.Companies = append(ds.Companies, &core.Company{Code: c.BUKRS, Name: c.BUTXT, Currency: c.WAERS})
	}

	recon := map[string]bool{}
	for _, a := range e.SKB1 {
		num := alpha(a.SAKNR)
		typ, err := m.accountType(num)
		if err != nil {
			fail("SKB1", a.BUKRS+"/"+a.SAKNR, err)
			continue
		}
		ds.Accounts = append(ds.Accounts, &core.GLAccount{
			CompanyCode: a.BUKRS, Number: num, Name: a.TXT50, Type: typ, VendorRecon: a.MITKZ == "K",
		})
		if a.MITKZ == "K" {
			recon[a.BUKRS+"/"+num] = true
		}
	}

	banks := map[string]string{}
	for _, b := range e.LFBK {
		banks[b.LIFNR] = b.BANKS + ":" + b.BANKN
	}
	vendors := map[string]*core.Vendor{}
	for _, v := range e.LFA1 {
		cv := &core.Vendor{
			ID: alpha(v.LIFNR), Name: v.NAME1, Country: v.LAND1, TaxID: v.STCD1,
			BankAccount: banks[v.LIFNR], APAccount: map[string]string{}, Blocked: v.SPERR == "X",
		}
		vendors[v.LIFNR] = cv
		ds.Vendors = append(ds.Vendors, cv)
	}
	for _, b := range e.LFB1 {
		v, ok := vendors[b.LIFNR]
		if !ok {
			fail("LFB1", b.LIFNR, fmt.Errorf("no vendor master"))
			continue
		}
		v.APAccount[b.BUKRS] = alpha(b.AKONT)
		days, err := m.paymentDays(b.ZTERM)
		if err != nil {
			fail("LFB1", b.LIFNR, err)
		}
		v.PaymentTermsDays = days
	}

	pos := map[string]*core.PurchaseOrder{}
	for _, h := range e.EKKO {
		date, err := isoDate(h.BEDAT)
		if err != nil {
			fail("EKKO", h.EBELN, err)
			continue
		}
		po := &core.PurchaseOrder{ID: h.EBELN, CompanyCode: h.BUKRS, VendorID: alpha(h.LIFNR), Currency: h.WAERS, OrderDate: date}
		pos[h.EBELN] = po
		ds.POs = append(ds.POs, po)
	}
	for _, i := range e.EKPO {
		po, ok := pos[i.EBELN]
		if !ok {
			fail("EKPO", i.EBELN+"/"+i.EBELP, fmt.Errorf("item without header"))
			continue
		}
		line, err := poLine(i, po.Currency, m)
		if err != nil {
			fail("EKPO", i.EBELN+"/"+i.EBELP, err)
			continue
		}
		po.Lines = append(po.Lines, line)
	}

	for _, g := range e.MSEG {
		if g.BWART != "101" && g.BWART != "102" {
			continue
		}
		key := g.MBLNR + "/" + g.MJAHR + "/" + g.ZEILE
		qty, err1 := money.ParseQty(g.MENGE)
		date, err2 := isoDate(g.BUDAT)
		lineNo, err3 := strconv.Atoi(g.EBELP)
		if err := firstErr(err1, err2, err3); err != nil {
			fail("MSEG", key, err)
			continue
		}
		if g.BWART == "102" {
			qty = -qty
		}
		ds.Receipts = append(ds.Receipts, &core.GoodsReceipt{
			ID: "SAPGR-" + g.MBLNR + "-" + g.MJAHR + "-" + g.ZEILE, POID: g.EBELN, POLineNo: lineNo, Quantity: qty, PostingDate: date,
		})
	}

	invIssues := transformInvoices(e, m, ds)
	issues = append(issues, invIssues...)

	switch mode {
	case ModeBalances:
		issues = append(issues, openingBalances(e, m, currency, recon, ds)...)
	default:
		issues = append(issues, replayDocuments(e, m, currency, ds)...)
	}
	return ds, issues
}

func poLine(i sap.EKPO, currency string, m Mapping) (core.POLine, error) {
	lineNo, err1 := strconv.Atoi(i.EBELP)
	qty, err2 := money.ParseQty(i.MENGE)
	price, err3 := m.parseAmount(i.NETPR, currency)
	peinh := int64(1)
	var err4 error
	if i.PEINH != "" {
		peinh, err4 = strconv.ParseInt(i.PEINH, 10, 64)
	}
	if err := firstErr(err1, err2, err3, err4); err != nil {
		return core.POLine{}, err
	}
	return core.POLine{
		LineNo: lineNo, Material: i.MATNR, Description: i.TXZ01, Quantity: qty,
		UnitPrice: price, PriceUnit: peinh, GLAccount: alpha(i.SAKTO),
	}, nil
}

func transformInvoices(e *sap.Extract, m Mapping, ds *Dataset) []Issue {
	var issues []Issue
	items := map[string][]sap.RSEG{}
	for _, it := range e.RSEG {
		k := it.BELNR + "/" + it.GJAHR
		items[k] = append(items[k], it)
	}
	for _, h := range e.RBKP {
		key := h.BELNR + "/" + h.GJAHR
		var status core.InvoiceStatus
		switch h.RBSTAT {
		case "A":
			status = core.InvoiceParked
		case "5":
			status = core.InvoicePosted
		default:
			continue // reported by CheckQuality
		}
		gross, err1 := m.parseAmount(h.RMWWR, h.WAERS)
		tax, err2 := m.parseAmount(h.WMWST1, h.WAERS)
		date, err3 := isoDate(h.BLDAT)
		if err := firstErr(err1, err2, err3); err != nil {
			issues = append(issues, Issue{SevError, "RBKP", key, err.Error()})
			continue
		}
		inv := &core.SupplierInvoice{
			ID: invoiceID(h.BELNR, h.GJAHR), CompanyCode: h.BUKRS, VendorID: alpha(h.LIFNR),
			VendorReference: h.XBLNR, InvoiceDate: date, Currency: h.WAERS,
			TaxAmount: tax, GrossAmount: gross, Status: status, Source: "sap:RBKP:" + key,
		}
		its := items[key]
		slices.SortFunc(its, func(a, b sap.RSEG) int { return cmp.Compare(a.BUZEI, b.BUZEI) })
		for _, it := range its {
			qty, err1 := money.ParseQty(it.MENGE)
			amt, err2 := m.parseAmount(it.WRBTR, h.WAERS)
			lineNo, err3 := strconv.Atoi(it.EBELP)
			if err := firstErr(err1, err2, err3); err != nil {
				issues = append(issues, Issue{SevError, "RSEG", key + "/" + it.BUZEI, err.Error()})
				continue
			}
			if inv.POID == "" {
				inv.POID = it.EBELN
			} else if inv.POID != it.EBELN {
				issues = append(issues, Issue{SevWarning, "RSEG", key, "invoice references several POs; matching uses " + inv.POID})
			}
			inv.Lines = append(inv.Lines, core.InvoiceLine{POLineNo: lineNo, Quantity: qty, Amount: amt})
		}
		ds.Invoices = append(ds.Invoices, inv)
	}
	return issues
}

type docKey struct{ cc, gjahr, belnr string }

func groupDocuments(e *sap.Extract) (map[docKey][]sap.ACDOCA, []docKey) {
	docs := map[docKey][]sap.ACDOCA{}
	var order []docKey
	for _, l := range e.ACDOCA {
		k := docKey{l.RBUKRS, l.GJAHR, l.BELNR}
		if _, seen := docs[k]; !seen {
			order = append(order, k)
		}
		docs[k] = append(docs[k], l)
	}
	slices.SortFunc(order, func(a, b docKey) int {
		return cmp.Compare(a.cc+a.gjahr+a.belnr, b.cc+b.gjahr+b.belnr)
	})
	return docs, order
}

func replayDocuments(e *sap.Extract, m Mapping, currency map[string]string, ds *Dataset) []Issue {
	var issues []Issue
	docs, order := groupDocuments(e)
	for _, k := range order {
		lines := docs[k]
		slices.SortFunc(lines, func(a, b sap.ACDOCA) int { return cmp.Compare(a.DOCLN, b.DOCLN) })
		date, err := isoDate(lines[0].BUDAT)
		if err != nil {
			issues = append(issues, Issue{SevError, "ACDOCA", k.cc + "/" + k.belnr, err.Error()})
			continue
		}
		je := &core.JournalEntry{
			ID: entryID(k.cc, k.gjahr, k.belnr), CompanyCode: k.cc, PostingDate: date,
			DocumentType: lines[0].BLART, Reference: lines[0].XBLNR,
			Source: fmt.Sprintf("sap:%s/%s/%s", k.cc, k.gjahr, k.belnr),
		}
		ok := true
		for _, l := range lines {
			amt, err := m.parseAmount(l.HSL, currency[k.cc])
			if err != nil {
				issues = append(issues, Issue{SevError, "ACDOCA", k.cc + "/" + k.belnr + "/" + l.DOCLN, err.Error()})
				ok = false
				break
			}
			jl := core.JournalLine{Account: alpha(l.RACCT), Amount: amt, VendorID: alpha(l.LIFNR)}
			if l.AUGBL != "" {
				jl.ClearedBy = entryID(k.cc, k.gjahr, l.AUGBL)
			}
			je.Lines = append(je.Lines, jl)
		}
		if ok {
			ds.Entries = append(ds.Entries, je)
		}
	}
	return issues
}

func openingBalances(e *sap.Extract, m Mapping, currency map[string]string, recon map[string]bool, ds *Dataset) []Issue {
	var issues []Issue
	type acc struct {
		balances  map[string]money.Minor
		openItems []core.JournalLine
		cleared   money.Minor
		lastDate  string
		gjahr     string
	}
	per := map[string]*acc{}
	for _, l := range e.ACDOCA {
		a := per[l.RBUKRS]
		if a == nil {
			a = &acc{balances: map[string]money.Minor{}, gjahr: l.GJAHR}
			per[l.RBUKRS] = a
		}
		amt, err := m.parseAmount(l.HSL, currency[l.RBUKRS])
		if err != nil {
			issues = append(issues, Issue{SevError, "ACDOCA", l.RBUKRS + "/" + l.BELNR + "/" + l.DOCLN, err.Error()})
			continue
		}
		if l.BUDAT > a.lastDate {
			a.lastDate = l.BUDAT
		}
		account := alpha(l.RACCT)
		if !recon[l.RBUKRS+"/"+account] {
			a.balances[account] += amt
			continue
		}
		if l.AUGBL != "" {
			a.cleared += amt
			continue
		}
		ref := l.XBLNR
		if ref == "" {
			ref = l.BELNR
		}
		a.openItems = append(a.openItems, core.JournalLine{
			Account: account, Amount: amt, VendorID: alpha(l.LIFNR), Text: "Open item " + ref + " (SAP " + l.BELNR + ")",
		})
	}
	for _, cc := range sortedKeys(per) {
		a := per[cc]
		if a.cleared != 0 {
			issues = append(issues, Issue{SevError, "ACDOCA", cc,
				fmt.Sprintf("cleared vendor items net to %s instead of zero; clearing data is inconsistent", a.cleared)})
		}
		date, err := isoDate(a.lastDate)
		if err != nil {
			issues = append(issues, Issue{SevError, "ACDOCA", cc, err.Error()})
			continue
		}
		je := &core.JournalEntry{
			ID: fmt.Sprintf("SAP-OB-%s-%s", cc, a.gjahr), CompanyCode: cc, PostingDate: date,
			DocumentType: "OB", Reference: "Opening balance from SAP", Source: "sap:balances:" + cc + "/" + a.gjahr,
		}
		for _, acct := range sortedKeys(a.balances) {
			if b := a.balances[acct]; b != 0 {
				je.Lines = append(je.Lines, core.JournalLine{Account: acct, Amount: b})
			}
		}
		je.Lines = append(je.Lines, a.openItems...)
		ds.Entries = append(ds.Entries, je)
	}
	return issues
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
