package analytics

import (
	"context"
	"fmt"
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

// minor converts an API amount to minor units. The API writes an amount
// with exactly its currency's minor-unit digits from ref.currency
// (ADR-0005), so the digits are the currency's exponent: they are recorded
// in Facts.Exponents, and an amount whose digits disagree with an earlier
// one in the same currency is refused rather than rendered with either.
func (b *factBuilder) minor(a amount) (money.Minor, error) {
	decimals := 0
	if i := strings.IndexByte(a.Amount, '.'); i >= 0 {
		decimals = len(a.Amount) - i - 1
	}
	m, err := money.ParseDecimal(a.Amount, decimals)
	if err != nil {
		return 0, err
	}
	if exp, seen := b.f.Exponents[a.Currency]; seen && exp != decimals {
		return 0, fmt.Errorf("amount %s %s has %d decimals, other %s amounts %d", a.Amount, a.Currency, decimals, a.Currency, exp)
	}
	b.f.Exponents[a.Currency] = decimals
	return m, nil
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
	les, err := apitools.List[legalEntity](ctx, s.API, "/v1/legal-entities", nil)
	if err != nil {
		return nil, err
	}
	partners, err := apitools.List[partner](ctx, s.API, "/v1/business-partners", nil)
	if err != nil {
		return nil, err
	}
	b := &factBuilder{
		f:          &Facts{Tables: map[string][]Fact{}, Companies: map[string]string{}, Exponents: map[string]int{}, AsOf: asOf},
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
	accounts, err := apitools.List[account](ctx, s.API, "/v1/accounts", byLE)
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
	open, err := apitools.List[openItem](ctx, s.API, "/v1/open-items", byLE)
	if err != nil {
		return err
	}
	isOpen := map[string]bool{}
	for _, o := range open {
		isOpen[o.JournalEntryID+"/"+strconv.Itoa(o.LineNo)] = true
	}
	entries, err := apitools.List[journalEntry](ctx, s.API, "/v1/journal-entries", byLE)
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
		amt, err := b.minor(l.AmountLC)
		if err == nil {
			err = b.calendar(dims, je.PostingDate)
		}
		if err != nil {
			return fmt.Errorf("journal entry %s: %w", je.ID, err)
		}
		b.f.Tables["gl_lines"] = append(b.f.Tables["gl_lines"], Fact{ID: je.ID, Dims: dims, Amount: amt, Currency: l.AmountLC.Currency})
	}
	return nil
}

// addOrders adds one fact per purchase order item to po_lines, valued at
// quantity times unit price per price unit.
func (s Source) addOrders(ctx context.Context, b *factBuilder, le legalEntity, byLE url.Values, chart map[string]account) error {
	orders, err := apitools.List[order](ctx, s.API, "/v1/purchase-orders", byLE)
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
		price, err := b.minor(it.UnitPrice)
		var amount money.Minor
		if err == nil {
			// Extended from the decimal string, as procurement does: a
			// document quantity has up to six decimals.
			if amount, err = money.Scale(price, it.Quantity.Value, max(it.PriceUnit, 1)); err != nil {
				err = fmt.Errorf("quantity %s: %w", it.Quantity.Value, err)
			}
		}
		if err == nil {
			err = b.calendar(dims, o.OrderDate)
		}
		if err != nil {
			return fmt.Errorf("purchase order %s: %w", o.ID, err)
		}
		b.f.Tables["po_lines"] = append(b.f.Tables["po_lines"], Fact{ID: o.ID, Dims: dims, Amount: amount, Currency: it.UnitPrice.Currency})
	}
	return nil
}

// addInvoices adds one fact per supplier invoice to ap_invoices.
func (s Source) addInvoices(ctx context.Context, b *factBuilder, le legalEntity, byLE url.Values) error {
	invoices, err := apitools.List[invoice](ctx, s.API, "/v1/supplier-invoices", byLE)
	if err != nil {
		return err
	}
	for _, inv := range invoices {
		v := b.vendors[inv.SupplierID]
		dims := map[string]string{"company": le.Code, "vendor": v.Number, "vendor_name": v.Name, "invoice_status": inv.Status}
		gross, err := b.minor(inv.Gross)
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
