// Package sales is the sales context (ADR-0067; 02-architecture §2.2,
// 03-data-model §3.16 and §3.29): quotations to customers and sales
// orders, each a header and its items, an item a quantity of something
// described at a unit price with the revenue account and output tax code
// its billing will post with. A sales order is created from an accepted
// quotation or directly and confirmed once (orders.go); Orders is the
// context's published read of one. The confirmation checks the order's
// gross value against the customer's credit limit and blocks an order
// over it until a credit manager releases it (ADR-0066 and ADR-0067, As
// built T77b; credit.go). An item's price is the one entered
// with it, or the price record of its item key valid on the document's
// date (ADR-0067 §10; prices.go the records, pricing.go the one function
// that prices an item for both documents). A tenant whose configuration
// has a pricing procedure has the discounts recorded for the item's key
// taken off in the procedure's order, and the item keeps the conditions
// applied (ADR-0067 §11).
//
// A sales document carries net amounts and posts nothing itself: tax is
// calculated when the order is billed, and a billing document posts
// through receivables, as a customer invoice entered and posted in the
// billing's transaction (billing_post.go; ADR-0067 §3, §9; ADR-0063 §1). There is no material master: an
// item names what is sold by a description and an optional free-text item
// key (ADR-0067 §3). Quantities are decimal strings, amounts minor units
// of the document's currency (ADR-0005).
//
// The context reads the ledger (accounts, tax codes, entries' numbers,
// exchange rates), the partner context (the customer by role, its company
// data and the payment terms), tenancy and the configuration through the
// ports below, as receivables does, enters and posts customer invoices
// through receivables' published API (Invoices), and asks the credit
// context, a leaf, about a customer's limit through a port of its own
// (Credit).
package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// customerRole is the partner role a customer plays (03 §3.6).
const customerRole = "customer"

// LegalEntities is what sales reads about legal entities (tenancy
// context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Customers says whether a business partner may be sold to as a customer
// (partner context): the check by role receivables makes of one at entry.
type Customers interface {
	PostingBlock(ctx context.Context, partnerID, legalEntity uuid.UUID, kind string) (string, error)
}

// CustomerData reads a customer's data in a company: what a sales order
// requires and its payment term defaults from (partner context, T74a).
type CustomerData interface {
	Customer(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Customer, bool, error)
}

// Terms reads a configured payment term (partner context).
type Terms interface {
	PaymentTerm(ctx context.Context, code string) (partner.PaymentTerm, bool, error)
}

// Partners names business partners by number and name (partner context).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// Accounts is what sales asks the ledger about an item's revenue account
// and output tax code.
type Accounts interface {
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
}

// References names the accounts a document's items carry by id (ledger
// context).
type References interface {
	AccountNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.AccountRef, error)
}

// Invoices is receivables' published API as billing uses it (ADR-0067
// §9; ADR-0063 §1): a customer invoice entered and posted, in the
// request's transaction.
type Invoices interface {
	CreateInvoice(ctx context.Context, in receivables.Input) (receivables.Invoice, error)
	PostInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (receivables.Invoice, error)
}

// Entries names journal entries by their document numbers (ledger
// context): the entry a posted billing document's invoice posted.
type Entries interface {
	Numbers(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: a company's time zone
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the sales part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Customers     Customers
	CustomerData  CustomerData
	Terms         Terms
	Partners      Partners
	Accounts      Accounts
	References    References
	Invoices      Invoices
	Entries       Entries
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	Rates         Rates
	Credit        Credit
	People        People
	// Now is the clock a document is dated by and a quotation's validity
	// is judged on, in the company's time zone; nil: the wall clock.
	Now func() time.Time
}

// today is the company's date now in its time zone (product owner
// 2026-10-31), never UTC's for a company that sets one: the date a sales
// document is made on, with the zone it is told in, so a refusal can name
// it beside the date.
func (e *Endpoints) today(ctx context.Context, company string) (config.CompanyDay, error) {
	day, err := config.DocumentDay(ctx, e.Settings, company, clock.Now(e.Now))
	if err != nil {
		return config.CompanyDay{}, fmt.Errorf("sales: today in company %s: %w", company, err)
	}
	return day, nil
}

// number draws a document number of the type from the company's range of
// the date's fiscal year.
func (e *Endpoints) number(ctx context.Context, le uuid.UUID, documentType string, date time.Time) (string, error) {
	year, err := e.Calendar.FiscalYear(ctx, le, date)
	if err != nil {
		return "", fmt.Errorf("sales: fiscal year of %s: %w", date.Format(time.DateOnly), err)
	}
	number, err := e.Numbers.Next(ctx, le, documentType, year)
	if err != nil {
		return "", fmt.Errorf("sales: number a %s: %w", documentType, err)
	}
	return number, nil
}

// record adds the audit record of a change to a sales document and
// publishes its event in the same transaction (ADR-0007, ADR-0011).
func record(ctx context.Context, action, aggregate, event string, id uuid.UUID, detail, payload map[string]any) error {
	if err := audit.Record(ctx, action, "sales."+aggregate, id, detail,
		outbox.Event{Context: "sales", Name: event, AggregateType: "sales." + aggregate, AggregateID: id, Payload: payload}); err != nil {
		return fmt.Errorf("sales: record %s of %s %s: %w", action, aggregate, id, err)
	}
	return nil
}
