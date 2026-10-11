package analytics

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Source builds the fact tables from the platform's data as the public API
// returns it to one principal, so company access, permissions and masking
// are the API's: a person sees in analytics exactly what they may read.
// Read models behind an analytics API replace it at scale.
type Source struct {
	API *apitools.Client
}

type amount struct {
	Amount, Currency string
}

// minor converts an API amount, which carries its currency's minor-unit
// digits, to minor units.
func (a amount) minor() (int64, error) {
	decimals := 0
	if i := strings.IndexByte(a.Amount, '.'); i >= 0 {
		decimals = len(a.Amount) - i - 1
	}
	m, err := money.ParseDecimal(a.Amount, decimals)
	return int64(m), err
}

// list reads every page of a list operation.
func list[T any](ctx context.Context, api *apitools.Client, path string, query url.Values) ([]T, error) {
	var out []T
	q := url.Values{"page_size": {"500"}}
	for k, v := range query {
		q[k] = v
	}
	for {
		resp, err := api.Do(ctx, http.MethodGet, path, q, nil, "")
		if err != nil {
			return nil, err
		}
		var page struct {
			Items         []T
			NextPageToken string `json:"next_page_token"`
		}
		if err := resp.Decode(&page); err != nil {
			return nil, fmt.Errorf("analytics: %s: %w", path, err)
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("page_token", page.NextPageToken)
	}
}

type legalEntity struct {
	ID, Code      string
	LocalCurrency string `json:"local_currency"`
}

type account struct {
	ID, Number, Name   string
	AccountType        string `json:"account_type"`
	ReconciliationKind string `json:"reconciliation_kind"`
}

type journalEntry struct {
	ID           string
	DocumentType string `json:"document_type"`
	PostingDate  string `json:"posting_date"`
	Lines        []struct {
		LineNo    int    `json:"line_no"`
		AccountID string `json:"account_id"`
		AmountLC  amount `json:"amount_lc"`
		PartnerID string `json:"partner_id"`
	}
}

type openItem struct {
	JournalEntryID string `json:"journal_entry_id"`
	LineNo         int    `json:"line_no"`
}

type partner struct {
	ID, Number, Name string
}

type order struct {
	ID         string
	SupplierID string `json:"supplier_id"`
	OrderDate  string `json:"order_date"`
	Items      []struct {
		Quantity  struct{ Value string }
		UnitPrice amount `json:"unit_price"`
		PriceUnit int64  `json:"price_unit"`
		AccountID string `json:"account_id"`
	}
}

type invoice struct {
	ID          string
	SupplierID  string `json:"supplier_id"`
	InvoiceDate string `json:"invoice_date"`
	Status      string
	Gross       amount `json:"gross_amount"`
}

// Facts reads the data of every legal entity the principal may see.
func (s Source) Facts(ctx context.Context, m *Model, asOf time.Time) (*Facts, error) {
	les, err := list[legalEntity](ctx, s.API, "/v1/legal-entities", nil)
	if err != nil {
		return nil, err
	}
	partners, err := list[partner](ctx, s.API, "/v1/business-partners", nil)
	if err != nil {
		return nil, err
	}
	vendors := map[string]partner{}
	for _, p := range partners {
		vendors[p.ID] = p
	}
	f := &Facts{Tables: map[string][]Fact{}, Companies: map[string]string{}, AsOf: asOf}
	cal := func(dims map[string]string, date string) error {
		fy, fm, err := fiscal(date, m.Calendar.FiscalYearStartMonth)
		if err != nil {
			return err
		}
		dims["posting_date"] = date
		dims["fiscal_year"] = strconv.Itoa(fy)
		dims["fiscal_month"] = strconv.Itoa(fm)
		dims["fiscal_quarter"] = strconv.Itoa((fm-1)/3 + 1)
		f.DataThrough = max(f.DataThrough, date)
		return nil
	}
	for _, le := range les {
		f.Companies[le.Code] = le.LocalCurrency
		byLE := url.Values{"legal_entity_id": {le.ID}}
		accounts, err := list[account](ctx, s.API, "/v1/accounts", byLE)
		if err != nil {
			return nil, err
		}
		chart := map[string]account{}
		for _, a := range accounts {
			chart[a.ID] = a
		}
		open, err := list[openItem](ctx, s.API, "/v1/open-items", byLE)
		if err != nil {
			return nil, err
		}
		isOpen := map[string]bool{}
		for _, o := range open {
			isOpen[o.JournalEntryID+"/"+strconv.Itoa(o.LineNo)] = true
		}
		entries, err := list[journalEntry](ctx, s.API, "/v1/journal-entries", byLE)
		if err != nil {
			return nil, err
		}
		for _, je := range entries {
			for _, l := range je.Lines {
				a := chart[l.AccountID]
				recon := a.ReconciliationKind == "supplier"
				v := vendors[l.PartnerID]
				dims := map[string]string{"company": le.Code, "account": a.Number, "account_name": a.Name, "account_type": a.AccountType,
					"document_type": je.DocumentType, "vendor": v.Number, "vendor_name": v.Name, "vendor_recon": strconv.FormatBool(recon),
					"cleared": strconv.FormatBool(recon && !isOpen[je.ID+"/"+strconv.Itoa(l.LineNo)])}
				amt, err := l.AmountLC.minor()
				if err == nil {
					err = cal(dims, je.PostingDate)
				}
				if err != nil {
					return nil, fmt.Errorf("journal entry %s: %w", je.ID, err)
				}
				f.Tables["gl_lines"] = append(f.Tables["gl_lines"], Fact{ID: je.ID, Dims: dims, Amount: amt, Currency: le.LocalCurrency})
			}
		}
		orders, err := list[order](ctx, s.API, "/v1/purchase-orders", byLE)
		if err != nil {
			return nil, err
		}
		for _, o := range orders {
			v := vendors[o.SupplierID]
			for _, it := range o.Items {
				a := chart[it.AccountID]
				dims := map[string]string{"company": le.Code, "vendor": v.Number, "vendor_name": v.Name, "account": a.Number, "account_name": a.Name}
				qty, err := money.ParseQty(it.Quantity.Value)
				price, perr := it.UnitPrice.minor()
				if err == nil {
					err = perr
				}
				if err == nil {
					err = cal(dims, o.OrderDate)
				}
				if err != nil {
					return nil, fmt.Errorf("purchase order %s: %w", o.ID, err)
				}
				f.Tables["po_lines"] = append(f.Tables["po_lines"], Fact{ID: o.ID, Dims: dims,
					Amount: int64(money.Extend(qty, money.Minor(price), max(it.PriceUnit, 1))), Currency: it.UnitPrice.Currency})
			}
		}
		invoices, err := list[invoice](ctx, s.API, "/v1/supplier-invoices", byLE)
		if err != nil {
			return nil, err
		}
		for _, inv := range invoices {
			v := vendors[inv.SupplierID]
			dims := map[string]string{"company": le.Code, "vendor": v.Number, "vendor_name": v.Name, "invoice_status": inv.Status}
			gross, err := inv.Gross.minor()
			if err == nil {
				err = cal(dims, inv.InvoiceDate)
			}
			if err != nil {
				return nil, fmt.Errorf("invoice %s: %w", inv.ID, err)
			}
			f.Tables["ap_invoices"] = append(f.Tables["ap_invoices"], Fact{ID: inv.ID, Dims: dims, Amount: gross, Currency: inv.Gross.Currency})
		}
	}
	return f, nil
}
