package collections

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Invoices enters, posts and reverses the customer debit memos a dunning
// run charges its notices with (receivables context, T76h, T76i): the
// published API sales posts its billing documents through. A memo is
// entered, posted and reversed in the caller's transaction.
type Invoices interface {
	CreateInvoice(ctx context.Context, in receivables.Input) (receivables.Invoice, error)
	PostInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (receivables.Invoice, error)
	ReverseSourced(ctx context.Context, id, source uuid.UUID, in receivables.Reversal) (receivables.Invoice, error)
}

// Accounts finds a company's accounts by the numbers the configuration
// names them by (ledger context).
type Accounts interface {
	Accounts(ctx context.Context, legalEntity uuid.UUID, numbers []string) (map[string]uuid.UUID, error)
}

// Charge is a row of collections.dunning_charge: the customer debit memo
// a confirmed run posted a notice's charge and interest with (T76h), its
// journal entry and the date it posted on.
type Charge struct {
	NoticeID      uuid.UUID
	InvoiceID     uuid.UUID
	InvoiceNumber string
	EntryYear     int16
	EntryID       uuid.UUID
	PostingDate   time.Time
	// Reversal is the memo's reversal when its run was cancelled (T76i);
	// nil while it stands.
	Reversal *ChargeReversal
}

// ChargeReversal is the ledger's reversal of a charge's debit memo: its
// entry and the date it posted on.
type ChargeReversal struct {
	EntryYear int16
	EntryID   uuid.UUID
	Date      time.Time
}

// chargeAccounts are the accounts a company's dunning charges and
// interest post to, uuid.Nil for one it names none for.
type chargeAccounts struct{ charges, interest uuid.UUID }

// postCharges posts each notice's charge and interest to its customer
// (T76h, ADR-0065 As built): a company whose account determination names
// neither dunning account posts nothing, as before; one that names either
// posts one customer debit memo per notice charged anything, by notice,
// and records it. The memos are the confirmation's: entered and posted in
// its transaction, after its status guard, so a second confirmation posts
// none, and each names its notice as its source, which receivables takes
// once (customer_invoice_source_once). A refusal of receivables or the
// ledger refuses the confirmation, naming the notice's customer.
func (e *Endpoints) postCharges(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run Run, le tenancy.Entity, notices []Notice) (map[uuid.UUID]Charge, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: dunning accounts of company %s: %w", le.Code, err)
	}
	co := doc.Company(le.Code)
	if co == nil || !co.AccountDetermination.PostsCharges() {
		return nil, nil
	}
	accounts, err := e.chargeAccounts(ctx, le, co.AccountDetermination)
	if err != nil {
		return nil, err
	}
	day, err := doc.CompanyDay(le.Code, clock.Now(e.Now))
	if err != nil {
		return nil, fmt.Errorf("collections: today of company %s: %w", le.Code, err)
	}
	out := map[uuid.UUID]Charge{}
	for _, n := range notices {
		if n.ChargeAmount == 0 && n.InterestAmount == 0 {
			continue
		}
		c, err := e.postCharge(ctx, run, n, accounts, day.Date)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_charge (tenant_id, dunning_notice_id, dunning_run_id, customer_invoice_id,
				customer_invoice_number, journal_entry_year, journal_entry_id, posting_date) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			p.TenantID, n.ID, run.ID, c.InvoiceID, c.InvoiceNumber, c.EntryYear, c.EntryID, c.PostingDate); err != nil {
			return nil, fmt.Errorf("collections: record the charge of dunning notice %s: %w", n.ID, err)
		}
		out[n.ID] = c
	}
	return out, nil
}

// chargeAccounts reads the ids of the company's dunning accounts. The
// configuration names accounts of the company's chart, which the ledger
// holds once it is active.
func (e *Endpoints) chargeAccounts(ctx context.Context, le tenancy.Entity, ad config.AccountDetermination) (chargeAccounts, error) {
	var numbers []string
	for _, n := range []string{ad.DunningCharges, ad.DunningInterest} {
		if n != "" {
			numbers = append(numbers, n)
		}
	}
	ids, err := e.Accounts.Accounts(ctx, le.ID, numbers)
	if err != nil {
		return chargeAccounts{}, fmt.Errorf("collections: dunning accounts of company %s: %w", le.Code, err)
	}
	return chargeAccounts{charges: ids[ad.DunningCharges], interest: ids[ad.DunningInterest]}, nil
}

// postCharge enters and posts the notice's debit memo: the customer, the
// run's currency, the company's, an item for the charge on the charge
// account and one for the interest to the run's key date on the interest
// account, each only when above zero and without a tax code (whether a
// country taxes them is a specialist's question, ADR-0065 As built), the
// customer's payment term, and today, the company's day the run is
// confirmed in its time zone, as the memo's date and posting date: the
// charge is raised then, and the memo is numbered in that day's fiscal
// year, whatever year the key date is in. A charge or interest the
// configuration no longer names an account for, since the run was
// proposed, is refused, 422 collections.no_charge_account. An amount goes
// as the decimal string of its minor units, which receivables reads back
// exactly. The memo posts at the version it was entered at: no one else
// can have read it, it being of this transaction.
func (e *Endpoints) postCharge(ctx context.Context, run Run, n Notice, accounts chargeAccounts, today time.Time) (Charge, error) {
	in := receivables.Input{LegalEntityID: run.LegalEntityID, CustomerID: n.CustomerID, Kind: receivables.KindDebitMemo, InvoiceDate: today, PostingDate: &today,
		Currency: run.Currency, Source: &receivables.Source{ID: n.ID, Number: n.ID.String()}}
	for _, it := range []struct {
		amount  money.Minor
		account uuid.UUID
		key     string
		text    string
	}{{n.ChargeAmount, accounts.charges, "dunning_charges", fmt.Sprintf("Dunning charge, level %d", n.Level)},
		{n.InterestAmount, accounts.interest, "dunning_interest", "Interest on overdue items to " + run.KeyDate.Format(time.DateOnly)}} {
		if it.amount == 0 {
			continue
		}
		if it.account == uuid.Nil {
			return Charge{}, problem.Unprocessable("collections.no_charge_account", "The notice of customer "+n.CustomerNumber+" charges "+
				strings.ToLower(it.text[:1])+it.text[1:]+", and the company's account determination names no account for it (account_determination."+it.key+").")
		}
		text := it.text
		in.Items = append(in.Items, receivables.ItemInput{Description: &text, AccountID: it.account,
			Net: receivables.Amount{Value: it.amount.Plain(run.Exponent), Currency: run.Currency}})
	}
	lead := "The dunning charge of customer " + n.CustomerNumber + " cannot be posted: "
	draft, err := e.Invoices.CreateInvoice(ctx, in)
	if err != nil {
		return Charge{}, problem.Led(fmt.Errorf("collections: enter the debit memo of dunning notice %s: %w", n.ID, err), lead)
	}
	v, err := e.Invoices.PostInvoice(ctx, draft.ID, func(int32) error { return nil }, nil)
	if err != nil {
		return Charge{}, problem.Led(fmt.Errorf("collections: post debit memo %s of dunning notice %s: %w", draft.DocumentNumber, n.ID, err), lead)
	}
	return Charge{NoticeID: n.ID, InvoiceID: v.ID, InvoiceNumber: v.DocumentNumber, EntryYear: *v.EntryYear, EntryID: *v.EntryID,
		PostingDate: *v.PostingDate}, nil // a posted memo names its entry and posting date (customer_invoice_posted)
}

// charged gives the notices the debit memos their confirmation posted
// them with (T76h); a notice charged nothing keeps none.
func charged(ctx context.Context, tx pgx.Tx, notices []Notice) error {
	at := make(map[uuid.UUID]int, len(notices))
	ids := make([]uuid.UUID, len(notices))
	for i, n := range notices {
		at[n.ID], ids[i] = i, n.ID
	}
	// Each row goes to its own notice, so the rows' order does not matter.
	rows, _ := tx.Query(ctx, `SELECT dunning_notice_id, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, posting_date,
			reversal_entry_year, reversal_entry_id, reversal_date
		FROM collections.dunning_charge WHERE dunning_notice_id = ANY($1)`, ids)
	var c Charge
	var revYear *int16
	var revEntry *uuid.UUID
	var revDate *time.Time
	_, err := pgx.ForEachRow(rows, []any{&c.NoticeID, &c.InvoiceID, &c.InvoiceNumber, &c.EntryYear, &c.EntryID, &c.PostingDate, &revYear, &revEntry, &revDate},
		func() error {
			charge := c
			if revEntry != nil { // the reversal is whole or none (dunning_charge_reversed)
				charge.Reversal = &ChargeReversal{EntryYear: *revYear, EntryID: *revEntry, Date: *revDate}
			}
			notices[at[c.NoticeID]].Charge = &charge
			return nil
		})
	if err != nil {
		return fmt.Errorf("collections: read the charges of %d dunning notices: %w", len(notices), err)
	}
	return nil
}
