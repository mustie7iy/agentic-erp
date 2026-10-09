// Package nfr holds the non-functional test suites: performance budgets,
// concurrency safety, security controls and reliability. It contains only
// test helpers; the tests themselves live in the _test files.
package nfr

import (
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// LargeStore builds a synthetic company with the given number of vendors
// and posted invoices (each with a PO and full goods receipt), plus parked
// invoices waiting for matching. Amounts are deterministic.
func LargeStore(vendors, posted, parked int) *core.Store {
	s := core.NewStore()
	s.Companies["1000"] = &core.Company{Code: "1000", Name: "Load Test Co", Currency: "USD"}
	for _, a := range []*core.GLAccount{
		{CompanyCode: "1000", Number: "100000", Name: "Bank", Type: core.Asset},
		{CompanyCode: "1000", Number: "140000", Name: "Input tax", Type: core.Asset},
		{CompanyCode: "1000", Number: "211000", Name: "AP", Type: core.Liability, VendorRecon: true},
		{CompanyCode: "1000", Number: "400000", Name: "Revenue", Type: core.Revenue},
		{CompanyCode: "1000", Number: "510000", Name: "Materials", Type: core.Expense},
	} {
		s.PutAccount(a)
	}
	for i := 0; i < vendors; i++ {
		id := fmt.Sprintf("V%06d", i)
		s.Vendors[id] = &core.Vendor{ID: id, Name: "Vendor " + id, TaxID: "T" + id, BankAccount: fmt.Sprintf("%012d", i),
			PaymentTermsDays: 30, APAccount: map[string]string{"1000": "211000"}}
	}
	add := func(n int, status core.InvoiceStatus, prefix string) {
		for i := 0; i < n; i++ {
			vendor := fmt.Sprintf("V%06d", i%vendors)
			poID := fmt.Sprintf("%sPO%07d", prefix, i)
			qty := money.Qty((1 + i%20) * money.QtyScale)
			price := money.Minor(1_000 + i%50_000)
			month := 1 + i%9
			date := fmt.Sprintf("2026-%02d-%02d", month, 1+i%28)
			s.PurchaseOrders[poID] = &core.PurchaseOrder{ID: poID, CompanyCode: "1000", VendorID: vendor, Currency: "USD", OrderDate: date,
				Lines: []core.POLine{{LineNo: 10, Description: "item", Quantity: qty, UnitPrice: price, PriceUnit: 1, GLAccount: "510000"}}}
			grID := fmt.Sprintf("%sGR%07d", prefix, i)
			s.GoodsReceipts[grID] = &core.GoodsReceipt{ID: grID, POID: poID, POLineNo: 10, Quantity: qty, PostingDate: date}
			net := money.Extend(qty, price, 1)
			tax := money.Percent(net, 8)
			invID := fmt.Sprintf("%sINV%07d", prefix, i)
			inv := &core.SupplierInvoice{ID: invID, CompanyCode: "1000", VendorID: vendor, VendorReference: invID, POID: poID,
				InvoiceDate: date, Currency: "USD", Lines: []core.InvoiceLine{{POLineNo: 10, Quantity: qty, Amount: net}},
				TaxAmount: tax, GrossAmount: net + tax, Status: status, Source: "load"}
			s.Invoices[invID] = inv
			if status == core.InvoicePosted {
				jeID := fmt.Sprintf("%sJE%07d", prefix, i)
				s.Journal[jeID] = &core.JournalEntry{ID: jeID, CompanyCode: "1000", PostingDate: date, DocumentType: "RE", Reference: invID,
					Source: "load", Lines: []core.JournalLine{{Account: "510000", Amount: net}, {Account: "140000", Amount: tax},
						{Account: "211000", Amount: -(net + tax), VendorID: vendor}}}
				inv.JournalEntryID = jeID
			}
		}
	}
	add(posted, core.InvoicePosted, "P")
	add(parked, core.InvoiceParked, "Q")
	return s
}
