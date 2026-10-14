// Package nfr holds the in-memory performance budgets of 01-requirements
// §1.3, tested as 06-engineering-process §6.6 and 05-scalability §5.8
// describe: the semantic query engine and configuration validation.
// Database-backed budgets live with their contexts (ledger trial balance,
// payment proposal), and concurrency, security and reliability are tested on
// the foundation by each context (SoD and agent rules in authz and
// approvals, idempotency and audit in the unit of work). It contains only
// test helpers; the tests themselves live in the _test files.
package nfr

import (
	"fmt"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// LargeFacts builds synthetic fact tables for one company: per invoice a
// journal entry of three lines, a purchase order line and the invoice.
// Amounts are deterministic.
func LargeFacts(vendors, invoices int) *analytics.Facts {
	f := &analytics.Facts{Tables: map[string][]analytics.Fact{}, Companies: map[string]string{"1000": "USD"}}
	for i := 0; i < invoices; i++ {
		vendor := fmt.Sprintf("V%06d", i%vendors)
		month := 1 + i%9
		date := fmt.Sprintf("2026-%02d-%02d", month, 1+i%28)
		net := money.Minor((1 + i%20) * (1_000 + i%50_000))
		tax := net * 8 / 100
		dims := func(extra map[string]string) map[string]string {
			d := map[string]string{"company": "1000", "posting_date": date, "fiscal_year": "2026", "fiscal_month": strconv.Itoa(month),
				"fiscal_quarter": strconv.Itoa((month-1)/3 + 1), "vendor": vendor, "vendor_name": "Vendor " + vendor}
			for k, v := range extra {
				d[k] = v
			}
			return d
		}
		id := fmt.Sprintf("INV%07d", i)
		for _, l := range []struct {
			account, typ string
			amount       money.Minor
			recon        bool
		}{{"510000", "expense", net, false}, {"140000", "asset", tax, false}, {"211000", "liability", -(net + tax), true}} {
			f.Tables["gl_lines"] = append(f.Tables["gl_lines"], analytics.Fact{ID: "JE" + id, Amount: l.amount, Currency: "USD",
				Dims: dims(map[string]string{"account": l.account, "account_type": l.typ, "document_type": "KR", "vendor_recon": strconv.FormatBool(l.recon),
					"cleared": "false"})})
		}
		f.Tables["po_lines"] = append(f.Tables["po_lines"], analytics.Fact{ID: "PO" + id, Amount: net, Currency: "USD",
			Dims: dims(map[string]string{"account": "510000"})})
		f.Tables["ap_invoices"] = append(f.Tables["ap_invoices"], analytics.Fact{ID: id, Amount: net + tax, Currency: "USD",
			Dims: dims(map[string]string{"invoice_status": "posted"})})
	}
	return f
}
