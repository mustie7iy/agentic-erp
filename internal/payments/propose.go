package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Exclusion reasons.
const (
	SupplierBlocked   = "supplier_blocked"
	PaymentBlocked    = "payment_blocked"
	NoBankAccount     = "no_bank_account"
	BankChangePending = "bank_change_pending"
	BankChangeHold    = "bank_change_hold"
	DebitBalance      = "debit_balance"
	// A release drops a payment whose item was reversed, or is no longer
	// open, since the proposal (dropPayment).
	ItemReversed = "item_reversed"
	ItemNotOpen  = "item_not_open"
	// TaxCodeInvalid leaves out a down payment request whose input tax
	// code is not valid on the run date (T46c); the supplier's items may
	// still be paid.
	TaxCodeInvalid = "tax_code_invalid"
	// NoHouseBank leaves out, in a run proposed without a house bank, a
	// supplier's items or request when none of its payment methods has a
	// house bank ranked for the run currency (T45c, ADR-0038).
	NoHouseBank = "no_house_bank"
)

// Proposal is what a client asks to pay: from one house bank, or without
// one (nil) in Currency, each supplier through the bank its payment methods
// rank (ADR-0038).
type Proposal struct {
	LegalEntityID uuid.UUID
	HouseBankID   *uuid.UUID
	Currency      string // required without a house bank; with one, only its currency
	RunDate       time.Time
	DueBy         time.Time
}

// ProposeRun creates a run in status proposing, numbered from the
// payment_run range of the run date's fiscal year, and queues its
// payments.propose operation, whose resource is the run from the start.
// The Proposer builds it.
func (e *Endpoints) ProposeRun(ctx context.Context, in Proposal) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return operations.Operation{}, err
	}
	if le.Status != "active" {
		return operations.Operation{}, problem.Unprocessable("payments.legal_entity_not_active", "The legal entity is "+le.Status+"; payment runs need it active.")
	}
	runCcy, err := runCurrency(ctx, tx, le.ID, in)
	if err != nil {
		return operations.Operation{}, err
	}
	year, err := e.Calendar.FiscalYear(ctx, le.ID, in.RunDate)
	if err != nil {
		return operations.Operation{}, err
	}
	number, err := e.Numbers.Next(ctx, le.ID, RunNumbers, year)
	if err != nil {
		return operations.Operation{}, err
	}
	id := uuid.Must(uuid.NewV7())
	op, err := operations.StartFor(ctx, ProposeKind, map[string]any{"legal_entity_id": le.ID, "house_bank_id": in.HouseBankID, "currency": runCcy,
		"run_date": in.RunDate.Format(time.DateOnly), "due_by": in.DueBy.Format(time.DateOnly)}, "/v1/payment-runs/"+id.String())
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by,
			currency, total_amount, status, proposed_by, operation_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, 'proposing', $9, $10, $9, $9)`,
		p.TenantID, id, number, le.ID, in.HouseBankID, in.RunDate, in.DueBy, runCcy, p.ID, op.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("payments: insert run %s: %w", id, err)
	}
	return op, audit.Add(ctx, audit.Entry{Action: "payments.payment_run.propose", ObjectType: "payments.payment_run", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"document_number": number, "operation_id": op.ID, "run_date": in.RunDate.Format(time.DateOnly),
			"due_by": in.DueBy.Format(time.DateOnly)}})
}

// runCurrency is the currency of the run proposed: its house bank's, which
// must be an active bank of the legal entity, or without one the currency
// asked for, which is required and must exist.
func runCurrency(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, in Proposal) (string, error) {
	if in.HouseBankID == nil {
		if in.Currency == "" {
			return "", problem.Unprocessable("payments.currency_required", "A run proposed without a house bank names its currency.",
				problem.FieldError{Field: "currency", Code: "required", Message: "the run currency"})
		}
		_, err := currency.Exponent(ctx, tx, in.Currency, problem.ReferenceNotFound("currency", "currency", "a known ISO 4217 currency"))
		return in.Currency, err
	}
	hb, ok, err := loadHouseBank(ctx, tx, *in.HouseBankID)
	switch {
	case err != nil:
		return "", err
	case !ok || hb.LegalEntityID != legalEntity:
		return "", problem.ReferenceNotFound("house_bank_id", "house bank of the legal entity", "no house bank of the legal entity with this id that you can access")
	case hb.Status != "active":
		return "", problem.Unprocessable("payments.house_bank_inactive", "House bank "+hb.Code+" is inactive.",
			problem.FieldError{Field: "house_bank_id", Code: "inactive", Message: "an active house bank"})
	case in.Currency != "" && in.Currency != hb.Currency:
		return "", problem.Unprocessable("payments.currency_mismatch", "House bank "+hb.Code+" pays in "+hb.Currency+", not "+in.Currency+".",
			problem.FieldError{Field: "currency", Code: "mismatch", Message: "the house bank's currency, or none"})
	}
	return hb.Currency, nil
}

// candidate is a supplier's due items not in another live run.
type candidate struct {
	supplier uuid.UUID
	items    []Item
}

// errTooLarge fails a proposal whose amounts add up to more than an
// amount holds (money.ErrOverflow): a wrapped total would pay the wrong
// amount, and the exclusion reasons are a closed set in the database, so
// the run fails rather than leaving the supplier out. The problem replaces
// money.ErrOverflow by design: it is what the operation records.
func errTooLarge(what string) error {
	return problem.Unprocessable("payments.amount_too_large", "The open items of "+what+" add up to more than an amount can hold.")
}

// supplierTooLarge is errTooLarge for one supplier's items, before the
// supplier is named: the proposal computes without a context, so
// paymentsFor names it (nameTooLarge), by number and name as the release's
// refusals do.
type supplierTooLarge struct{ supplier uuid.UUID }

func (s supplierTooLarge) Error() string {
	return "payments: the open items of supplier " + s.supplier.String() + " add up to more than an amount can hold"
}

// nameTooLarge turns a supplierTooLarge into errTooLarge naming the
// supplier as its consumer knows it (REVIEW.md, an identifier without the
// name its consumer needs); any other error passes unchanged. The partner
// context failing stays an error, so the job is retried. A supplier it
// does not hold (journal lines name their partner without a foreign key)
// still fails the run, naming the supplier by its id: left an error, the
// job would be retried first in its tenant's queue forever (build).
func (e *Endpoints) nameTooLarge(ctx context.Context, err error) error {
	var tl supplierTooLarge
	if !errors.As(err, &tl) {
		return err
	}
	named, err := e.Payees.Names(ctx, []uuid.UUID{tl.supplier})
	if err != nil {
		return fmt.Errorf("payments: name supplier %s: %w", tl.supplier, err)
	}
	n, ok := named[tl.supplier]
	if !ok {
		return errTooLarge("supplier " + tl.supplier.String())
	}
	return errTooLarge("supplier " + supplierLabel(tl.supplier, n))
}

// total is what is owed the supplier after the cash discounts its items
// take and the withholding taxes deducted from them: positive when it is
// to be paid.
func (c candidate) total() (money.Minor, error) {
	amounts := make([]money.Minor, 0, 3*len(c.items))
	for _, it := range c.items {
		amounts = append(amounts, it.Amount, -it.Discount, -it.Withholding)
	}
	return money.Sum(amounts...)
}

// takeDiscounts sets the cash discount each item takes on the date, and
// the withholding tax deducted from what it pays after it (T46f), as the
// ledger posts it (ledger.PaymentWithholding).
func (c *candidate) takeDiscounts(on time.Time) error {
	for i, it := range c.items {
		d, err := discountOn(it.terms, it.Amount, on)
		if err == nil {
			c.items[i].Withholding, err = ledger.PaymentWithholding(it.withholding, it.Amount, d)
		}
		if err == nil {
			err = withholdingWithin(it, d, c.items[i].Withholding)
		}
		if err != nil {
			return fmt.Errorf("payments: the discount on item %d/%s/%d: %w", it.FiscalYear, it.EntryID, it.LineNo, err)
		}
		c.items[i].Discount = d
		if d != 0 {
			until := it.terms.Until(it.terms.Discounts[tierOn(it.terms, on)])
			c.items[i].until = &until
		}
	}
	return nil
}

// withholdingWithin refuses a withholding that, with the discount, takes
// all of what the item owes, as the payment item's check
// (payment_item_withholding_within) would. The invoice refuses codes that
// withhold its whole gross, but the payment rounds its withholding after
// the discount, so at extreme rates it can still reach the rest (422).
// The discount is of the item's sign and smaller than it (discountOn), so
// the difference cannot overflow.
func withholdingWithin(it Item, discount, withholding money.Minor) error {
	amount := it.Amount
	if withholding == 0 || ((withholding > 0) == (amount > 0) && withholding.Abs() < amount.Abs()-discount.Abs()) {
		return nil
	}
	return problem.Unprocessable("payments.withholding_too_large",
		fmt.Sprintf("Item %d/%s/%d's withholding tax of %d with its cash discount of %d takes all of the %d it owes.", it.FiscalYear, it.EntryID, it.LineNo,
			withholding, discount, amount))
}

// deductions are the cash discount the supplier's items take and the
// withholding tax deducted from them.
func (c candidate) deductions() (discount, withholding money.Minor, err error) {
	discounts, taxes := make([]money.Minor, len(c.items)), make([]money.Minor, len(c.items))
	for i, it := range c.items {
		discounts[i], taxes[i] = it.Discount, it.Withholding
	}
	if discount, err = money.Sum(discounts...); err == nil {
		withholding, err = money.Sum(taxes...)
	}
	return discount, withholding, err
}

// exclusion is why a supplier cannot be paid on the run date, or "".
func exclusion(p partner.Payee, found bool, runDate time.Time, holdDays int) string {
	switch {
	case !found || p.Status != partner.Active || p.PostingBlocked:
		return SupplierBlocked
	case p.PaymentBlocked:
		return PaymentBlocked
	case p.BankAccountID == nil:
		return NoBankAccount
	case p.BankChangePending:
		return BankChangePending
	case p.BankChanged && runDate.Before(p.BankValidFrom.AddDate(0, 0, holdDays)):
		return BankChangeHold
	}
	return ""
}

// candidates groups the legal entity's due open items by supplier, leaving
// out items reserved by another live run: the items due by the run's
// due-by date, and those whose cash discount would lapse before it
// (losesDiscount), each with the terms it was posted with.
func (e *Endpoints) candidates(ctx context.Context, tx pgx.Tx, r Run, requests []DownPayment) ([]candidate, map[uuid.UUID]partner.Payee, error) {
	open, err := e.OpenItems.Payable(ctx, r.LegalEntityID, r.Currency, r.DueBy)
	if err != nil {
		return nil, nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.item_fiscal_year, i.item_entry_id, i.item_line_no FROM payments.payment_item i
		JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
		WHERE r.legal_entity_id = $1 AND i.is_active`, r.LegalEntityID)
	reserved := map[ledger.ItemRef]bool{}
	var ref ledger.ItemRef
	if _, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo}, func() error { reserved[ref] = true; return nil }); err != nil {
		return nil, nil, fmt.Errorf("payments: read the reserved items of legal entity %s: %w", r.LegalEntityID, err)
	}
	var suppliers []uuid.UUID
	for i, it := range open {
		if i == 0 || it.PartnerID != open[i-1].PartnerID {
			suppliers = append(suppliers, it.PartnerID)
		}
	}
	for _, d := range requests {
		suppliers = append(suppliers, d.SupplierID)
	}
	payees, err := e.Payees.Payees(ctx, r.LegalEntityID, suppliers, r.RunDate)
	if err != nil {
		return nil, nil, err
	}
	var out []candidate
	for _, it := range open {
		due := dueDate(it, payees[it.PartnerID])
		if (due.After(r.DueBy) && !losesDiscount(it.Terms, r.RunDate, r.DueBy)) || reserved[it.ItemRef] {
			continue
		}
		if len(out) == 0 || out[len(out)-1].supplier != it.PartnerID {
			out = append(out, candidate{supplier: it.PartnerID})
		}
		c := &out[len(out)-1]
		c.items = append(c.items, Item{FiscalYear: it.FiscalYear, EntryID: it.EntryID, LineNo: it.LineNo, Amount: -it.Amount,
			DueDate: partner.Date{Time: due}, Active: true, terms: it.Terms, withholding: it.Withholding})
	}
	return out, payees, nil
}

// build computes a proposing run's payments and exclusions, reserves the
// paid items and moves the run to proposed. Proposals of one legal entity
// are serialized, so two runs never reserve the same item.
func (e *Endpoints) build(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) (Run, error) {
	if err := lockProposals(ctx, tx, r.LegalEntityID); err != nil {
		return Run{}, err
	}
	// errNoEntity becomes a problem, which fails this run (Process): left an
	// error, the job would be retried first in its tenant's queue forever,
	// holding back every later proposal.
	le, err := e.entityOf(ctx, r)
	if errors.Is(err, errNoEntity) {
		return Run{}, noEntity(r)
	}
	if err != nil {
		return Run{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Run{}, err
	}
	pr, err := e.paymentsFor(ctx, tx, r, le.Code, doc.Payments.BankChangeHoldDays)
	if err != nil {
		return Run{}, err
	}
	if err := pr.insert(ctx, tx, p, r); err != nil {
		return Run{}, err
	}
	if err := e.Postings.Reserve(ctx, owner(r), pr.items()); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'proposed', total_amount = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		r.ID, pr.total, p.ID); err != nil {
		return Run{}, fmt.Errorf("payments: propose run %s: %w", r.ID, err)
	}
	n := int64(len(pr.itemPays))
	if err := operations.Report(ctx, *r.OperationID, n, n); err != nil {
		return Run{}, err
	}
	if _, err := operations.Succeed(ctx, *r.OperationID, "/v1/payment-runs/"+r.ID.String(), map[string]any{"payment_run_id": r.ID, "status": Proposed,
		"payment_count": len(pr.payIDs), "exclusion_count": len(pr.exSuppliers), "item_count": n, "total_amount": money.EventAmount(pr.total, r.Currency, r.Exponent)}); err != nil {
		return Run{}, err
	}
	proposed, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return Run{}, err
	}
	return proposed, record(ctx, "payments.payment_run.build", r.ID, map[string]any{"payments": len(pr.payIDs), "exclusions": len(pr.exSuppliers), "items": n},
		runEvent(proposed, "payment_run_proposed", map[string]any{"house_bank_id": proposed.HouseBankID, "run_date": proposed.RunDate.Format(time.DateOnly),
			"due_by": proposed.DueBy.Format(time.DateOnly), "total_amount": money.EventAmount(pr.total, proposed.Currency, proposed.Exponent),
			"discount_amount":    money.EventAmount(pr.discount, proposed.Currency, proposed.Exponent),
			"withholding_amount": money.EventAmount(pr.withheld, proposed.Currency, proposed.Exponent),
			"payment_count":      len(pr.payIDs), "exclusion_count": len(pr.exSuppliers), "proposed_by": proposed.ProposedBy, "operation_id": proposed.OperationID}))
}

// paymentsFor computes a run's payments and exclusions: of the suppliers'
// due items and of the due down payment requests, those whose tax code is
// not valid on the run date left out (invalidTaxCodes), each paid through
// the house bank and method its router chooses, refused when the legal
// entity lacks an account the payments post to.
func (e *Endpoints) paymentsFor(ctx context.Context, tx pgx.Tx, r Run, entityCode string, holdDays int) (proposal, error) {
	requests, err := e.dueRequests(ctx, tx, r)
	if err != nil {
		return proposal{}, err
	}
	cands, payees, err := e.candidates(ctx, tx, r, requests)
	if err != nil {
		return proposal{}, err
	}
	route, err := routerFor(ctx, tx, r)
	if err != nil {
		return proposal{}, err
	}
	pr := proposal{route: route}
	if err := pr.propose(cands, payees, r, entityCode, holdDays); err != nil {
		return proposal{}, e.nameTooLarge(ctx, err)
	}
	invalid, err := e.invalidTaxCodes(ctx, r, requests)
	if err != nil {
		return proposal{}, err
	}
	if err := pr.payRequests(requests, invalid, payees, r, entityCode, holdDays); err != nil {
		return proposal{}, err
	}
	if err := e.requireCashDiscountAccount(ctx, r, pr); err != nil {
		return proposal{}, err
	}
	if err := e.requireDownPaymentAccount(ctx, r, pr); err != nil {
		return proposal{}, err
	}
	return pr, nil
}

// requireCashDiscountAccount fails a run that takes a cash discount in a
// legal entity without a cash discount account (422
// payments.no_cash_discount_account), at proposal rather than after its
// approval, when its release would fail on it.
func (e *Endpoints) requireCashDiscountAccount(ctx context.Context, r Run, pr proposal) error {
	if !slices.ContainsFunc(pr.itemDiscounts, func(d money.Minor) bool { return d != 0 }) {
		return nil
	}
	_, ok, err := e.Accounts.Determination(ctx, r.LegalEntityID, "cash_discount")
	if err != nil || ok {
		return err
	}
	return problem.Unprocessable("payments.no_cash_discount_account", "The run takes cash discounts, and the legal entity has no cash_discount account in its account determination.")
}

// requireDownPaymentAccount fails a run that pays a down payment request
// in a legal entity without a down payment account (422
// payments.no_down_payment_account), at proposal rather than at release,
// when it would fail after some payments posted. Once determined, the
// account does not change or go (ledger.Setup.Check), so the release
// finds it.
func (e *Endpoints) requireDownPaymentAccount(ctx context.Context, r Run, pr proposal) error {
	if !slices.Contains(pr.kinds, KindDownPayment) {
		return nil
	}
	_, ok, err := e.Accounts.Determination(ctx, r.LegalEntityID, "down_payment")
	if err != nil || ok {
		return err
	}
	return problem.Unprocessable("payments.no_down_payment_account", "The run pays down payment requests, and the legal entity has no down_payment account in its account determination.")
}

// lockProposals takes the legal entity's proposals lock until the
// transaction ends. Proposals take it so two runs never reserve the same
// item; whoever would change what a proposal pays (an invoice reversal,
// through RunOf) takes it too, so it waits for a proposal under way and then
// sees the items it reserved, and a proposal that starts later waits for it
// and then no longer finds the item open.
func lockProposals(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "payments.propose."+legalEntity.String()); err != nil {
		return fmt.Errorf("payments: lock the proposals of legal entity %s: %w", legalEntity, err)
	}
	return nil
}

// proposal is a run's payments, their items and its exclusions, as the
// columns they are inserted from.
type proposal struct {
	payIDs, suppliers, banks, exSuppliers []uuid.UUID
	exRequests                            []*uuid.UUID // the request an exclusion leaves out, or nil for the supplier's items
	kinds                                 []string
	requests                              []*uuid.UUID // the down payment request a payment pays, or nil
	amounts, discounts, withholding       []money.Minor
	exAmounts                             []money.Minor
	e2e, exReasons                        []string
	itemPays, itemEntries                 []uuid.UUID
	itemYears                             []int16
	itemLines                             []int32
	itemAmounts, itemDiscounts, itemWHT   []money.Minor
	itemDue                               []time.Time
	untils                                []*time.Time
	total, discount, withheld             money.Minor
	houseBanks                            []*uuid.UUID // the bank that pays a payment, or nil for the run's (via)
	methods                               []*string
	route                                 router
}

// dueDate is when an open item is due: an invoice (a credit on the supplier
// account) its supplier's net days after its document date; every debit on
// the supplier account (a credit memo, whether or not it names an invoice,
// a subsequent credit, a manual or migrated debit) on its document date, so
// it nets with the next run that pays the supplier (T34d). SAP has a credit
// memo without an invoice reference due at once and gives one naming an
// invoice that invoice's due date; netting earlier never pays more than is
// owed. A credit posted with payment terms (T36b) is due on its net due
// date by the terms it was posted with, so a later change to the term does
// not move it; one posted without them (before T36, manual, migrated) by
// its supplier's net days now.
func dueDate(it ledger.PayableItem, payee partner.Payee) time.Time {
	switch {
	case it.Amount > 0:
		return it.DocumentDate
	case it.Terms != nil:
		return it.Terms.NetDueDate()
	}
	return it.DocumentDate.AddDate(0, 0, payee.NetDays)
}

// propose pays each candidate supplier its open items in one payment, or
// excludes it with the reason; a supplier is paid what its items owe after
// their cash discounts, and one owed nothing after them is excluded with
// its debit balance, one no ranked house bank can pay with NoHouseBank.
// Totals out of range fail the proposal (supplierTooLarge, errTooLarge).
func (pr *proposal) propose(cands []candidate, payees map[uuid.UUID]partner.Payee, r Run, entityCode string, holdDays int) error {
	for _, c := range cands {
		if err := c.takeDiscounts(r.RunDate); err != nil {
			return err
		}
		total, err := c.total()
		if err != nil {
			return supplierTooLarge{c.supplier}
		}
		payee, found := payees[c.supplier]
		reason := exclusion(payee, found, r.RunDate, holdDays)
		if reason == "" && total <= 0 {
			reason = DebitBalance
		}
		v, routed := pr.route.route(payee)
		if reason == "" && !routed {
			reason = NoHouseBank
		}
		if reason != "" {
			pr.exclude(c.supplier, nil, reason, total)
			continue
		}
		if err := pr.pay(c, *payee.BankAccountID, v, total, r, entityCode); err != nil {
			return err
		}
	}
	return nil
}

// pay adds the payment of a candidate supplier's items, through v, to the
// proposal.
func (pr *proposal) pay(c candidate, bank uuid.UUID, v via, total money.Minor, r Run, entityCode string) error {
	discount, withholding, err := c.deductions()
	if err != nil {
		return supplierTooLarge{c.supplier}
	}
	if pr.total, err = money.Sum(pr.total, total); err == nil {
		pr.discount, err = money.Sum(pr.discount, discount)
	}
	if err == nil {
		pr.withheld, err = money.Sum(pr.withheld, withholding)
	}
	if err != nil {
		return errTooLarge("the run")
	}
	id := pr.payment(c.supplier, bank, v, [3]money.Minor{total, discount, withholding}, KindItems, nil, r, entityCode)
	var until *time.Time
	for _, it := range c.items {
		if it.until != nil && (until == nil || it.until.Before(*until)) {
			until = it.until
		}
		pr.itemPays, pr.itemYears, pr.itemEntries, pr.itemLines = append(pr.itemPays, id), append(pr.itemYears, it.FiscalYear), append(pr.itemEntries, it.EntryID), append(pr.itemLines, it.LineNo)
		pr.itemAmounts, pr.itemDiscounts, pr.itemDue = append(pr.itemAmounts, it.Amount), append(pr.itemDiscounts, it.Discount), append(pr.itemDue, it.DueDate.Time)
		pr.itemWHT = append(pr.itemWHT, it.Withholding)
	}
	pr.untils[len(pr.untils)-1] = until
	return nil
}

// payment adds a payment to the proposal, of its amount, discount and
// withholding, through v, and returns its id.
func (pr *proposal) payment(supplier, bank uuid.UUID, v via, amounts [3]money.Minor, kind string, request *uuid.UUID, r Run, entityCode string) uuid.UUID {
	id := uuid.Must(uuid.NewV7())
	pr.houseBanks, pr.methods = append(pr.houseBanks, v.bank), append(pr.methods, v.method)
	pr.payIDs, pr.suppliers, pr.banks, pr.amounts = append(pr.payIDs, id), append(pr.suppliers, supplier), append(pr.banks, bank), append(pr.amounts, amounts[0])
	pr.discounts, pr.withholding = append(pr.discounts, amounts[1]), append(pr.withholding, amounts[2])
	pr.e2e = append(pr.e2e, fmt.Sprintf("%s-%s-%06d", entityCode, r.DocumentNumber, len(pr.payIDs)))
	pr.kinds, pr.requests, pr.untils = append(pr.kinds, kind), append(pr.requests, request), append(pr.untils, nil)
	return id
}

// items are the open items the proposal pays.
func (pr *proposal) items() []ledger.ItemRef {
	refs := make([]ledger.ItemRef, len(pr.itemEntries))
	for i := range refs {
		refs[i] = ledger.ItemRef{FiscalYear: pr.itemYears[i], EntryID: pr.itemEntries[i], LineNo: pr.itemLines[i]}
	}
	return refs
}

// owner is the run as the holder of the open items it reserves in the
// ledger.
func owner(r Run) ledger.Owner {
	return ledger.Owner{Context: "payments", ID: r.ID, Reference: r.DocumentNumber}
}

// insert stores the proposal's payments, items and exclusions.
func (pr *proposal) insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) error {
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, discount_amount,
			discount_until, currency, end_to_end_id, created_by, kind, down_payment_request_id, withholding_amount, house_bank_id, payment_method)
		SELECT $1, i, $2, s, b, a, d, u, $3, x, $4, k, q, w, h, m
		FROM unnest($5::uuid[], $6::uuid[], $7::uuid[], $8::bigint[], $9::bigint[], $10::date[], $11::text[], $12::text[], $13::uuid[], $14::bigint[],
			$15::uuid[], $16::text[]) AS t(i, s, b, a, d, u, x, k, q, w, h, m)`,
		p.TenantID, r.ID, r.Currency, p.ID, pr.payIDs, pr.suppliers, pr.banks, pr.amounts, pr.discounts, pr.untils, pr.e2e, pr.kinds, pr.requests,
		pr.withholding, pr.houseBanks, pr.methods); err != nil {
		return fmt.Errorf("payments: insert the payments of run %s: %w", r.ID, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount,
			discount_amount, due_date, withholding_amount)
		SELECT $1, p, y, je, n, a, c, d, w FROM unnest($2::uuid[], $3::smallint[], $4::uuid[], $5::int[], $6::bigint[], $7::bigint[], $8::date[], $9::bigint[])
			AS t(p, y, je, n, a, c, d, w)`,
		p.TenantID, pr.itemPays, pr.itemYears, pr.itemEntries, pr.itemLines, pr.itemAmounts, pr.itemDiscounts, pr.itemDue, pr.itemWHT); err != nil {
		return fmt.Errorf("payments: insert the payment items of run %s: %w", r.ID, err)
	}
	// A supplier's items are excluded in payment_exclusion, a request left
	// out in request_exclusion (T46c): the request is the row's key there.
	if _, err := tx.Exec(ctx, `WITH x AS (SELECT * FROM unnest($3::uuid[], $4::text[], $5::bigint[], $6::uuid[]) AS t(s, c, a, q)),
		items AS (INSERT INTO payments.payment_exclusion (tenant_id, payment_run_id, supplier_id, reason_code, amount)
			SELECT $1, $2, s, c, a FROM x WHERE q IS NULL)
		INSERT INTO payments.request_exclusion (tenant_id, payment_run_id, down_payment_request_id, supplier_id, reason_code, amount)
		SELECT $1, $2, q, s, c, a FROM x WHERE q IS NOT NULL`,
		p.TenantID, r.ID, pr.exSuppliers, pr.exReasons, pr.exAmounts, pr.exRequests); err != nil {
		return fmt.Errorf("payments: insert the exclusions of run %s: %w", r.ID, err)
	}
	return nil
}

// fail moves a run that could not be proposed, or whose operation was
// cancelled, to failed; the operation records the problem unless it is
// already cancelled.
func fail(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, why *problem.Problem) error {
	if why != nil {
		if _, err := operations.Fail(ctx, *r.OperationID, why); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_run SET status = 'failed', version = version + 1, updated_by = $2 WHERE id = $1`, r.ID, p.ID); err != nil {
		return fmt.Errorf("payments: fail run %s: %w", r.ID, err)
	}
	failed, err := load(ctx, tx, r.ID, false)
	if err != nil {
		return err
	}
	detail := map[string]any{"cancelled": why == nil}
	if why != nil {
		detail["code"], detail["detail"] = why.Code, why.Detail
	}
	return record(ctx, "payments.payment_run.fail", r.ID, detail, runEvent(failed, "payment_run_failed", map[string]any{"operation_id": r.OperationID,
		"released_item_count": 0, "previous_status": Proposing}))
}

// Job is a proposal waiting for the Proposer: a queued one to build, or
// a cancelled one whose run is still proposing.
type Job struct {
	RunID        uuid.UUID
	Cancelled    bool
	ProposedBy   uuid.UUID
	ProposerKind string
}

// NextJob finds and locks the tenant's oldest job, skipping jobs another
// worker holds.
func (e *Endpoints) NextJob(ctx context.Context, tx pgx.Tx) (Job, bool, error) {
	var j Job
	var status string
	err := tx.QueryRow(ctx, `SELECT r.id, o.status, r.proposed_by FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
		WHERE o.kind = $1 AND r.status = 'proposing' AND o.status IN ('queued', 'cancelled')
		ORDER BY o.id LIMIT 1 FOR UPDATE OF o, r SKIP LOCKED`, ProposeKind).Scan(&j.RunID, &status, &j.ProposedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("payments: find the next proposal job: %w", err)
	}
	return e.proposer(ctx, tx, j, status)
}

// JobFor finds and locks one run's job: the run still proposing with its
// operation queued or cancelled. ok is false when there is nothing left to
// do for the run, so a retried worker does nothing twice.
func (e *Endpoints) JobFor(ctx context.Context, tx pgx.Tx, run uuid.UUID) (Job, bool, error) {
	var j Job
	var status string
	err := tx.QueryRow(ctx, `SELECT r.id, o.status, r.proposed_by FROM payments.payment_run r
		JOIN platform.operation o ON o.tenant_id = r.tenant_id AND o.id = r.operation_id
		WHERE r.id = $1 AND r.status = 'proposing' AND o.status IN ('queued', 'cancelled') FOR UPDATE OF o, r`, run).Scan(&j.RunID, &status, &j.ProposedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("payments: find the proposal job of run %s: %w", run, err)
	}
	return e.proposer(ctx, tx, j, status)
}

// proposer completes a locked job with whether its operation was
// cancelled and the kind of principal its proposer is.
func (e *Endpoints) proposer(ctx context.Context, tx pgx.Tx, j Job, status string) (Job, bool, error) {
	j.Cancelled = status == operations.Cancelled
	p, err := e.Actors.Actor(ctx, tx, j.ProposedBy)
	if err != nil {
		return Job{}, false, fmt.Errorf("payments: read the proposer of run %s: %w", j.RunID, err)
	}
	j.ProposerKind = p.Kind
	return j, true, nil
}

// Process does a job as its proposer: it builds the run, or fails it when
// its operation was cancelled. A problem while building fails the run and
// its operation; other errors leave both for another attempt.
func (e *Endpoints) Process(ctx context.Context, j Job) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := load(ctx, tx, j.RunID, false)
	if err != nil {
		return err
	}
	if j.Cancelled {
		return fail(ctx, tx, p, r, nil)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT propose`); err != nil {
		return fmt.Errorf("payments: savepoint before building run %s: %w", r.ID, err)
	}
	_, err = e.build(ctx, tx, p, r)
	var why *problem.Problem
	if !errors.As(err, &why) || why.Status >= 500 {
		return err
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT propose`); err != nil {
		return fmt.Errorf("payments: roll back the build of run %s: %w", r.ID, err)
	}
	audit.Discard(ctx)
	return fail(ctx, tx, p, r, why)
}
