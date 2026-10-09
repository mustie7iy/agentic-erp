package analytics

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Fact is one row of a fact table. Amount is in minor units of Currency.
type Fact struct {
	ID       string
	Dims     map[string]string
	Amount   int64
	Currency string
}

// Facts is the analytical store: denormalized fact tables built from the
// core store, so queries never touch transactional structures. A columnar
// engine (DuckDB over Parquet) replaces this in-memory store at scale.
type Facts struct {
	Tables    map[string][]Fact
	Companies map[string]string // company code -> currency
	AsOf      time.Time
	// DataThrough is the latest posting or document date in the data.
	DataThrough string
}

// BuildFacts derives the fact tables from the core store.
func BuildFacts(s *core.Store, m *Model, asOf time.Time) (*Facts, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	f := &Facts{Tables: map[string][]Fact{}, Companies: map[string]string{}, AsOf: asOf}
	for code, c := range s.Companies {
		f.Companies[code] = c.Currency
	}
	cal := func(dims map[string]string, date string) error {
		fy, fm, err := fiscal(date, m.Calendar.FiscalYearStartMonth)
		if err != nil {
			return err
		}
		dims["posting_date"] = date
		dims["fiscal_year"] = strconv.Itoa(fy)
		dims["fiscal_month"] = strconv.Itoa(fm)
		dims["fiscal_quarter"] = strconv.Itoa((fm-1)/3 + 1)
		if date > f.DataThrough {
			f.DataThrough = date
		}
		return nil
	}
	vendorName := func(id string) string {
		if v := s.Vendors[id]; v != nil {
			return v.Name
		}
		return ""
	}

	for _, id := range core.SortedKeys(s.Journal) {
		je := s.Journal[id]
		for i, l := range je.Lines {
			acct := s.Account(je.CompanyCode, l.Account)
			if acct == nil {
				return nil, fmt.Errorf("journal %s line %d: unknown account %s", je.ID, i+1, l.Account)
			}
			dims := map[string]string{
				"company": je.CompanyCode, "account": l.Account, "account_name": acct.Name,
				"account_type": string(acct.Type), "document_type": je.DocumentType,
				"vendor": l.VendorID, "vendor_name": vendorName(l.VendorID),
				"vendor_recon": strconv.FormatBool(acct.VendorRecon), "cleared": strconv.FormatBool(l.ClearedBy != ""),
			}
			if err := cal(dims, je.PostingDate); err != nil {
				return nil, fmt.Errorf("journal %s: %w", je.ID, err)
			}
			f.Tables["gl_lines"] = append(f.Tables["gl_lines"], Fact{
				ID: je.ID, Dims: dims, Amount: int64(l.Amount), Currency: f.Companies[je.CompanyCode],
			})
		}
	}

	for _, id := range core.SortedKeys(s.PurchaseOrders) {
		po := s.PurchaseOrders[id]
		for _, l := range po.Lines {
			name := ""
			if a := s.Account(po.CompanyCode, l.GLAccount); a != nil {
				name = a.Name
			}
			dims := map[string]string{
				"company": po.CompanyCode, "vendor": po.VendorID, "vendor_name": vendorName(po.VendorID),
				"account": l.GLAccount, "account_name": name,
			}
			if err := cal(dims, po.OrderDate); err != nil {
				return nil, fmt.Errorf("purchase order %s: %w", po.ID, err)
			}
			f.Tables["po_lines"] = append(f.Tables["po_lines"], Fact{
				ID: po.ID, Dims: dims, Amount: int64(money.Extend(l.Quantity, l.UnitPrice, l.PriceUnit)), Currency: po.Currency,
			})
		}
	}

	for _, id := range core.SortedKeys(s.Invoices) {
		inv := s.Invoices[id]
		dims := map[string]string{
			"company": inv.CompanyCode, "vendor": inv.VendorID, "vendor_name": vendorName(inv.VendorID),
			"invoice_status": string(inv.Status),
		}
		if err := cal(dims, inv.InvoiceDate); err != nil {
			return nil, fmt.Errorf("invoice %s: %w", inv.ID, err)
		}
		f.Tables["ap_invoices"] = append(f.Tables["ap_invoices"], Fact{
			ID: inv.ID, Dims: dims, Amount: int64(inv.GrossAmount), Currency: inv.Currency,
		})
	}
	return f, nil
}

// fiscal maps an ISO date to fiscal year and period. With a start month
// other than January, the fiscal year is named after the calendar year in
// which it ends.
func fiscal(date string, startMonth int) (year, month int, err error) {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid date %q", date)
	}
	m := int(t.Month())
	year = t.Year()
	month = (m-startMonth+12)%12 + 1
	if startMonth > 1 && m >= startMonth {
		year++
	}
	return year, month, nil
}
