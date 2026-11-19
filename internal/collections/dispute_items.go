package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// MaxCaseItems is the most items one dispute case names (T86c;
// dispute_case_item_no_check): a case is one customer's dispute of some
// invoices, and a document made from an item names it by the case and its
// number in at most 40 characters (T86d).
const MaxCaseItems = 50

func init() {
	// A second open case of an item, by the items' index (migration
	// 000395), as by the case's own line (dispute_case_one_open).
	problem.RegisterConstraint("dispute_case_item_one_open", errDisputeExists)
}

// errDisputeExists refuses a case on an item that is in another open case.
func errDisputeExists() *problem.Problem {
	return problem.Conflict("collections.dispute_exists", "An item has an open dispute case already; work that one.")
}

// CaseItem is a row of collections.dispute_case_item: an open item of the
// case's customer by its journal line, with what the case read of it (its
// entry's document number and reference) and the amount disputed of it,
// in the case's currency and, as its share of the item as booked, in the
// company's. ID is the item's own, which a document made from it names;
// AccountID the item's account, which a write-off credits.
type CaseItem struct {
	ItemNo int16
	ID     uuid.UUID
	ledger.ItemRef
	AccountID      uuid.UUID
	DocumentNumber string
	Reference      *string
	Amount         money.Minor
	AmountLC       money.Minor
	// Accepted and AcceptedLC are what a resolution accepted of the item
	// (T86d), from zero to Amount, and CreditMemoID and CreditMemoNumber
	// the credit memo that credited it; nil for an item of a case not
	// resolved, and the memo for one not credited.
	Accepted, AcceptedLC *money.Minor
	CreditMemoID         *uuid.UUID
	CreditMemoNumber     *string
}

// caseItems reads the items of the cases, each case's by number, and gives
// them to their cases.
func caseItems(ctx context.Context, tx pgx.Tx, cases []Case) error {
	at := make(map[uuid.UUID]int, len(cases))
	ids := make([]uuid.UUID, len(cases))
	for i, c := range cases {
		at[c.ID], ids[i] = i, c.ID
	}
	rows, _ := tx.Query(ctx, `SELECT dispute_case_id, item_no, id, fiscal_year, journal_entry_id, line_no, account_id, document_number, reference,
			disputed_amount, disputed_amount_lc, accepted_amount, accepted_amount_lc, credit_memo_id, credit_memo_number
		FROM collections.dispute_case_item WHERE dispute_case_id = ANY($1) ORDER BY dispute_case_id, item_no`, ids)
	var id uuid.UUID
	var it CaseItem
	_, err := pgx.ForEachRow(rows, []any{&id, &it.ItemNo, &it.ID, &it.FiscalYear, &it.EntryID, &it.LineNo, &it.AccountID, &it.DocumentNumber, &it.Reference,
		&it.Amount, &it.AmountLC, &it.Accepted, &it.AcceptedLC, &it.CreditMemoID, &it.CreditMemoNumber}, func() error {
		c := &cases[at[id]]
		c.Items = append(c.Items, it)
		return nil
	})
	if err != nil {
		return fmt.Errorf("collections: read the items of %d dispute cases: %w", len(cases), err)
	}
	return nil
}

// CaseItemInput is an item of a case as the request names it: its journal
// line and the disputed amount, a decimal string in Currency. Field is
// where the request names it ("" for a request of one item, whose fields
// are the request's own; "items[n]." for one of a list), which a refusal
// names.
type CaseItemInput struct {
	Item             ledger.ItemRef
	Amount, Currency string
	Field            string
}

// disputedItem is an item of a case to open: the open item as the ledger
// reads it now and the amount disputed of it in its currency.
type disputedItem struct {
	ledger.ReceivableItem
	disputed money.Minor
}

// disputedItems reads the items of a case to open (T86c): 1 to
// MaxCaseItems, each named once, each an open item the first item's
// customer owes the company, in the first item's currency, disputed above
// zero and at most what is owed on it. A refusal names the item's field.
func (e *Endpoints) disputedItems(ctx context.Context, le tenancy.Entity, in []CaseItemInput) ([]disputedItem, error) {
	if len(in) == 0 || len(in) > MaxCaseItems { // the contract bounds them; a Go caller is held to it too
		return nil, problem.Unprocessable("request.validation_failed", fmt.Sprintf("The request is not valid: a dispute case names 1 to %d items.",
			MaxCaseItems), problem.FieldError{Field: "items", Code: "invalid", Message: fmt.Sprintf("1 to %d items", MaxCaseItems)})
	}
	seen := make(map[ledger.ItemRef]bool, len(in))
	out := make([]disputedItem, len(in))
	for i, item := range in {
		if seen[item.Item] {
			return nil, problem.Unprocessable("request.validation_failed", "The request is not valid: a dispute case names each item once.",
				problem.FieldError{Field: item.Field + "journal_entry_id", Code: "duplicate", Message: "named before"})
		}
		seen[item.Item] = true
		var (
			it  ledger.ReceivableItem
			err error
		)
		if i == 0 {
			it, err = e.openItem(ctx, le, item.Item, item.Field+"journal_entry_id")
		} else {
			it, err = e.customersItem(ctx, le, out[0].PartnerID, item.Item, item.Field+"journal_entry_id")
		}
		if err != nil {
			return nil, err
		}
		if i > 0 && it.Currency != out[0].Currency {
			return nil, problem.Invalid("collections.currency_mismatch", item.Field+"journal_entry_id", "The items of a dispute case are in one "+
				"currency, its first item's, "+out[0].Currency+"; this item is in "+it.Currency+".")
		}
		if out[i].disputed, err = disputed(item, it); err != nil {
			return nil, err
		}
		out[i].ReceivableItem = it
	}
	return out, nil
}

// disputed reads the amount disputed of an item: in the item's currency,
// more than zero and at most what the customer owes on it.
func disputed(in CaseItemInput, it ledger.ReceivableItem) (money.Minor, error) {
	field := in.Field + "disputed_amount"
	if in.Currency != it.Currency {
		return 0, problem.Invalid("collections.currency_mismatch", field+".currency", "The disputed amount is in the item's currency, "+it.Currency+".")
	}
	m, err := currency.ParseAmount(in.Amount, in.Currency, it.Exponent, field+".amount")
	if err != nil {
		return 0, err
	}
	if m <= 0 || m > it.Amount {
		return 0, problem.Invalid("collections.invalid_disputed_amount", field+".amount", fmt.Sprintf("The disputed amount is more than "+
			"zero and at most what the customer owes on the item, %s %s.", it.Amount.Plain(it.Exponent), it.Currency))
	}
	return m, nil
}

// totals are what the items dispute in their currency and, as booked, in
// the company's; more than an amount holds is refused.
func totals(items []disputedItem) (amount, lc money.Minor, err error) {
	amounts, locals := make([]money.Minor, len(items)), make([]money.Minor, len(items))
	for i, it := range items {
		amounts[i], locals[i] = it.disputed, share(it.ReceivableItem, it.disputed)
	}
	amount, err = money.Sum(amounts...)
	if err == nil {
		lc, err = money.Sum(locals...)
	}
	if err != nil { // money.Sum's only error is the range
		return 0, 0, problem.Unprocessable("collections.amount_too_large", "The disputed amounts of the items add up to more than an amount holds.")
	}
	return amount, lc, nil
}

// insertCase writes the case opened, its own journal line its first
// item's and its amounts the items' totals, and its items by number, in
// the request's transaction. The deferred check dispute_case_items holds
// the two to each other at commit.
func insertCase(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, le tenancy.Entity, in CaseInput, number string,
	items []disputedItem) error {
	amount, lc, err := totals(items)
	if err != nil {
		return err
	}
	first := items[0]
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dispute_case (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year,
			journal_entry_id, line_no, document_number, reference, currency, disputed_amount, currency_lc, disputed_amount_lc, reason, assignee_id, note,
			status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, 'open', $18, $18)`,
		p.TenantID, id, le.ID, first.PartnerID, number, first.FiscalYear, first.EntryID, first.LineNo, first.Number, first.Reference, first.Currency,
		amount, first.LocalCurrency, lc, in.Reason, in.Assignee, in.Note, p.ID); err != nil {
		return fmt.Errorf("collections: open a dispute case on %d items of customer %s: %w", len(items), number, err)
	}
	nos, ids, accounts := make([]int16, len(items)), make([]uuid.UUID, len(items)), make([]uuid.UUID, len(items))
	refs, docs, refTexts := make([]ledger.ItemRef, len(items)), make([]string, len(items)), make([]*string, len(items))
	amounts, locals := make([]money.Minor, len(items)), make([]money.Minor, len(items))
	for i, it := range items {
		nos[i], ids[i], accounts[i], refs[i] = int16(i+1), uuid.Must(uuid.NewV7()), it.AccountID, it.ItemRef // at most MaxCaseItems
		docs[i], refTexts[i] = it.Number, it.Reference
		amounts[i], locals[i] = it.disputed, share(it.ReceivableItem, it.disputed)
	}
	years, entries, lines := ledger.ItemColumns(refs)
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dispute_case_item (tenant_id, dispute_case_id, item_no, id, fiscal_year, journal_entry_id,
			line_no, account_id, document_number, reference, disputed_amount, disputed_amount_lc)
		SELECT $1, $2, n, i, y, je, ln, acc, dn, r, a, lc
		FROM unnest($3::smallint[], $4::uuid[], $5::smallint[], $6::uuid[], $7::int[], $8::uuid[], $9::text[], $10::text[], $11::bigint[], $12::bigint[])
			AS x(n, i, y, je, ln, acc, dn, r, a, lc)`,
		p.TenantID, id, nos, ids, years, entries, lines, accounts, docs, refTexts, amounts, locals); err != nil {
		return fmt.Errorf("collections: the items of dispute case %s: %w", id, err)
	}
	return nil
}

// itemsDetail is the case's items as its audit records and events name
// them: by number and journal line, with the document number and the
// disputed amount and, once the case is resolved (T86d), what was accepted
// of each and its credit memo.
func itemsDetail(c Case) []map[string]any {
	out := make([]map[string]any, len(c.Items))
	for i, it := range c.Items {
		out[i] = map[string]any{"item_no": it.ItemNo, "fiscal_year": it.FiscalYear, "journal_entry_id": it.EntryID, "line_no": it.LineNo,
			"document_number": it.DocumentNumber, "disputed_amount": money.EventAmount(it.Amount, c.Currency, c.Exponent)}
		if it.Accepted != nil {
			out[i]["accepted_amount"] = money.EventAmount(*it.Accepted, c.Currency, c.Exponent)
			out[i]["credit_memo_id"], out[i]["credit_memo_number"] = it.CreditMemoID, it.CreditMemoNumber
		}
	}
	return out
}

// resolutionDetail is a resolved case's resolution as its audit record and
// event name it, nil for a case not resolved.
func resolutionDetail(c Case) map[string]any {
	r := c.Resolution
	if r == nil {
		return nil
	}
	out := map[string]any{"kind": r.Kind, "accepted_amount": money.EventAmount(r.Accepted, c.Currency, c.Exponent),
		"posting_date": r.PostingDate.Format(time.DateOnly), "write_off_journal_entry_id": nil, "write_off_journal_entry_year": nil,
		"write_off_journal_entry_number": nil}
	if w := r.WriteOff; w != nil {
		out["write_off_journal_entry_id"], out["write_off_journal_entry_year"], out["write_off_journal_entry_number"] = w.ID, w.Year, w.Number
	}
	return out
}
