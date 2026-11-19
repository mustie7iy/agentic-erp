package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// einvoiceSales gives the einvoicing context the posted customer
// documents it renders (ADR-0061 §9, As built E2) over the published
// APIs of receivables (the document), the ledger (its tax codes'
// categories and rates, and its entry's tax items), tenancy and the
// configuration (the company and its postal address), tax filing (its
// FTA registration's number) and partner (the customer), each in the
// request's transaction, so einvoicing imports none of them.
type einvoiceSales struct{ keys *fieldcrypt.Keyring }

// uaeAuthority is the authority of the registration a UAE tax invoice
// names (the UAE pack's FTA registration, T61g).
const uaeAuthority, uaeTaxScheme = "FTA", "AE_TRN"

func (s einvoiceSales) Sale(ctx context.Context, id uuid.UUID) (einvoicing.Sale, bool, error) {
	v, err := receivables.GetInvoice(ctx, id)
	if err != nil {
		if problem.From(err).Status == http.StatusNotFound {
			return einvoicing.Sale{}, false, nil
		}
		return einvoicing.Sale{}, false, err
	}
	le, ok, err := tenancy.Directory{}.Entity(ctx, v.LegalEntityID)
	if err != nil || !ok {
		return einvoicing.Sale{}, false, err
	}
	out := einvoicing.Sale{ID: v.ID, Number: v.DocumentNumber, Kind: v.Kind, Status: v.Status, CompanyID: v.LegalEntityID, CustomerID: v.CustomerID,
		IssueDate: v.InvoiceDate, DueDate: v.NetDueDate, Currency: v.Currency, Exponent: v.Exponent, Net: v.Net, Tax: v.Tax, Gross: v.Gross,
		ReferenceNumber: v.ReferenceInvoiceNumber, SourceID: v.SourceDocumentID, SourceNumber: v.SourceDocumentNumber,
		LocalCurrency: le.LocalCurrency, Location: le.CountryCode}
	if out.Lines, err = saleLines(ctx, v); err != nil {
		return einvoicing.Sale{}, false, err
	}
	if v.EntryID != nil {
		if out.LocalTax, err = localTax(ctx, *v.EntryID); err != nil {
			return einvoicing.Sale{}, false, err
		}
	}
	return out, true, nil
}

// saleLines are the document's items with their codes' categories and
// rates on its date, each code read once.
func saleLines(ctx context.Context, v receivables.Invoice) ([]einvoicing.SaleLine, error) {
	codes := map[string]ledger.TaxCode{}
	out := make([]einvoicing.SaleLine, len(v.Items))
	for i, it := range v.Items {
		l := einvoicing.SaleLine{No: it.ItemNo, Net: it.Net, Tax: it.Tax}
		if it.Description != nil {
			l.Description = *it.Description
		}
		if it.TaxCode != nil {
			c, ok := codes[*it.TaxCode]
			if !ok {
				var err error
				if c, _, err = (ledger.Accounts{}).TaxCode(ctx, v.LegalEntityID, *it.TaxCode, v.InvoiceDate); err != nil {
					return nil, err
				}
				codes[*it.TaxCode] = c // a code without a rate that day is the zero TaxCode: no category, which the rules name
			}
			l.TaxCode, l.Category = *it.TaxCode, c.Category
			if len(c.Components) == 1 {
				l.Percent = c.Components[0].RatePct
			}
		}
		out[i] = l
	}
	return out, nil
}

// localTax is the VAT the entry's output tax items carry in the company's
// currency, as the posting converted it.
func localTax(ctx context.Context, entry uuid.UUID) (money.Minor, error) {
	items, err := (ledger.TaxItems{}).Of(ctx, entry)
	if err != nil {
		return 0, err
	}
	var lc []money.Minor
	for _, t := range items {
		if t.Direction == "output" {
			lc = append(lc, t.TaxLC)
		}
	}
	sum, err := money.Sum(lc...)
	if err != nil {
		return 0, fmt.Errorf("app: VAT of entry %s in the company's currency: %w", entry, err)
	}
	return sum, nil
}

func (s einvoiceSales) Seller(ctx context.Context, company uuid.UUID, day time.Time) (einvoicing.Seller, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, company)
	if err != nil || !ok {
		return einvoicing.Seller{}, err
	}
	out := einvoicing.Seller{Company: einvoicing.Company{ID: le.ID, Code: le.Code, Name: le.Name}}
	doc, _, err := configuration.Settings{}.Document(ctx)
	if err != nil {
		return einvoicing.Seller{}, err
	}
	if co := doc.Company(le.Code); co != nil && co.PostalAddress != nil {
		a := co.PostalAddress
		out.Address = &einvoicing.Address{Line1: a.Line1, Line2: a.Line2, City: a.City, PostalCode: a.PostalCode, Subentity: a.Region, Country: a.CountryCode}
	}
	out.TRN, out.Member, _, err = taxfiling.InvoiceNumbers{Keys: s.keys}.Number(ctx, company, uaeAuthority, day) // none: TRN "", which the rules name
	return out, err
}

func (s einvoiceSales) Buyer(ctx context.Context, customer uuid.UUID, day time.Time) (einvoicing.Buyer, bool, error) {
	p, ok, err := partner.InvoiceParties{Keys: s.keys}.Party(ctx, customer, uaeTaxScheme, day)
	if err != nil || !ok {
		return einvoicing.Buyer{}, false, err
	}
	out := einvoicing.Buyer{Number: p.Number, Name: p.Name, TRN: p.TaxID, Blocked: p.Blocked}
	if a := p.Address; a != nil {
		out.Address = &einvoicing.Address{Line1: a.Line1, Line2: deref(a.Line2), City: a.City, PostalCode: deref(a.PostalCode),
			Subentity: deref(a.Region), Country: a.CountryCode}
	}
	return out, true, nil
}

func (s einvoiceSales) Customers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]einvoicing.CustomerName, error) {
	named, err := partner.Payees{Keys: s.keys}.Names(ctx, ids)
	out := make(map[uuid.UUID]einvoicing.CustomerName, len(named))
	for id, n := range named {
		out[id] = einvoicing.CustomerName{Number: n.Number, Name: n.Name}
	}
	return out, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
