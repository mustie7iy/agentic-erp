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
func (a amount) minor() (money.Minor, error) {
	decimals := 0
	if i := strings.IndexByte(a.Amount, '.'); i >= 0 {
		decimals = len(a.Amount) - i - 1
	}
	m, err := money.ParseDecimal(a.Amount, decimals)
	return m, err
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
	b := &factBuilder{
		f:          &Facts{Tables: map[string][]Fact{}, Companies: map[string]string{}, AsOf: asOf},
		startMonth: m.Calendar.FiscalYearStartMonth,
		vendors:    map[string]partner{},
	}
	for _, p := range partners {
		b.vendors[p.ID] = p
	}
	for _, le := range les {
		if err := s.addEntity(ctx, b, le); err != nil {
			return nil, err
		}
	}
	return b.f, nil
}

// factBuilder accumulates the fact tables across legal entities.
type factBuilder struct {
	f          *Facts
	startMonth int
	vendors    map[string]partner
}

// calendar adds the posting date and its fiscal year, month and quarter to
// dims, and advances DataThrough.
func (b *factBuilder) calendar(dims map[string]string, date string) error {
	fy, fm, err := fiscal(date, b.startMonth)
	if err != nil {
		return err
	}
	dims["posting_date"] = date
	dims["fiscal_year"] = strconv.Itoa(fy)
	dims["fiscal_month"] = strconv.Itoa(fm)
	dims["fiscal_quarter"] = strconv.Itoa((fm-1)/3 + 1)
	b.f.DataThrough = max(b.f.DataThrough, date)
	return nil
}

// addEntity reads one legal entity's ledger, purchase orders and supplier
// invoices into the fact tables.
func (s Source) addEntity(ctx context.Context, b *factBuilder, le legalEntity) error {
	b.f.Companies[le.Code] = le.LocalCurrency
	byLE := url.Values{"legal_entity_id": {le.ID}}
	accounts, err := list[account](ctx, s.API, "/v1/accounts", byLE)
	if err != nil {
		return err
	}
	chart := map[string]account{}
	for _, a := range accounts {
		chart[a.ID] = a
	}
	if err := s.addJournal(ctx, b, le, byLE, chart); err != nil {
		return err
	}
	if err := s.addOrders(ctx, b, le, byLE, chart); err != nil {
		return err
	}
	return s.addInvoices(ctx, b, le, byLE)
}

// addJournal adds one fact per journal line to gl_lines; a supplier line
// counts as cleared when it is no longer an open item.
func (s Source) addJournal(ctx context.Context, b *factBuilder, le legalEntity, byLE url.Values, chart map[string]account) error {
	open, err := list[openItem](ctx, s.API, "/v1/open-items", byLE)
	if err != nil {
		return err
	}
	isOpen := map[string]bool{}
	for _, o := range open {
		isOpen[o.JournalEntryID+"/"+strconv.Itoa(o.LineNo)] = true
	}
	entries, err := list[journalEntry](ctx, s.API, "/v1/journal-entries", byLE)
	if err != nil {
		return err
	}
	for _, je := range entries {
		if err := b.addEntry(le, je, chart, isOpen); err != nil {
			return err
		}
	}
	return nil
}

func (b *factBuilder) addEntry(le legalEntity, je journalEntry, chart map[string]account, isOpen map[string]bool) error {
	for _, l := range je.Lines {
		a := chart[l.AccountID]
		recon := a.ReconciliationKind == "supplier"
		v := b.vendors[l.PartnerID]
		dims := map[string]string{"company": le.Code, "account": a.Number, "account_name": a.Name, "account_type": a.AccountType,
			"document_type": je.DocumentType, "vendor": v.Number, "vendor_name": v.Name, "vendor_recon": strconv.FormatBool(recon),
			"cleared": strconv.FormatBool(recon && !isOpen[je.ID+"/"+strconv.Itoa(l.LineNo)])}
		amt, err := l.AmountLC.minor()
		if err == nil {
			err = b.calendar(dims, je.PostingDate)
		}
		if err != nil {
			return fmt.Errorf("journal entry %s: %w", je.ID, err)
		}
		b.f.Tables["gl_lines"] = append(b.f.Tables["gl_lines"], Fact{ID: je.ID, Dims: dims, Amount: amt, Currency: le.LocalCurrency})
	}
	return nil
}

// addOrders adds one fact per purchase order item to po_lines, valued at
// quantity times unit price per price unit.
func (s Source) addOrders(ctx context.Context, b *factBuilder, le legalEntity, byLE url.Values, chart map[string]account) error {
	orders, err := list[order](ctx, s.API, "/v1/purchase-orders", byLE)
	if err != nil {
		return err
	}
	for _, o := range orders {
		if err := b.addOrder(le, o, chart); err != nil {
			return err
		}
	}
	return nil
}

func (b *factBuilder) addOrder(le legalEntity, o order, chart map[string]account) error {
	v := b.vendors[o.SupplierID]
	for _, it := range o.Items {
		a := chart[it.AccountID]
		dims := map[string]string{"company": le.Code, "vendor": v.Number, "vendor_name": v.Name, "account": a.Number, "account_name": a.Name}
		qty, err := money.ParseQty(it.Quantity.Value)
		price, perr := it.UnitPrice.minor()
		if err == nil {
			err = perr
		}
		if err == nil {
			err = b.calendar(dims, o.OrderDate)
		}
		if err != nil {
			return fmt.Errorf("purchase order %s: %w", o.ID, err)
		}
		b.f.Tables["po_lines"] = append(b.f.Tables["po_lines"], Fact{ID: o.ID, Dims: dims,
			Amount: money.Extend(qty, price, max(it.PriceUnit, 1)), Currency: it.UnitPrice.Currency})
	}
	return nil
}

// addInvoices adds one fact per supplier invoice to ap_invoices.
func (s Source) addInvoices(ctx context.Context, b *factBuilder, le legalEntity, byLE url.Values) error {
	invoices, err := list[invoice](ctx, s.API, "/v1/supplier-invoices", byLE)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		v := b.vendors[inv.SupplierID]
		dims := map[string]string{"company": le.Code, "vendor": v.Number, "vendor_name": v.Name, "invoice_status": inv.Status}
		gross, err := inv.Gross.minor()
		if err == nil {
			err = b.calendar(dims, inv.InvoiceDate)
		}
		if err != nil {
			return fmt.Errorf("invoice %s: %w", inv.ID, err)
		}
		b.f.Tables["ap_invoices"] = append(b.f.Tables["ap_invoices"], Fact{ID: inv.ID, Dims: dims, Amount: gross, Currency: inv.Gross.Currency})
	}
	return nil
}
