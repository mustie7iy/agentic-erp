package controlling

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// An order settled and closed (ADR-0064 §3, T60e). A settlement moves the
// order's unsettled balance up to a period, account by account, to the
// receiver of its rule: one entry through the ledger's posting port,
// which per account credits the order and debits the receiving cost
// center, so each account's balance is unchanged. The order is held FOR
// UPDATE from before its balance is read until the entry is posted: a
// second settlement waits and finds nothing left, and a posting to the
// order, which holds it FOR SHARE, is either read by the settlement or
// waits for it.

// settlement is a row of controlling.order_settlement; ReceiverCode is
// named by the ledger's port when the settlement is shown.
type settlement struct {
	ID, OrderID              uuid.UUID
	FiscalYear, FiscalPeriod int16
	PostingDate              time.Time
	ReceiverID               uuid.UUID
	ReceiverCode             string
	EntryID                  uuid.UUID
	DocumentNumber           string
	Amount                   money.Minor
	Accounts                 int32
	CreatedAt                time.Time
	CreatedBy                uuid.UUID
}

const settlementColumns = `s.id, s.internal_order_id, s.fiscal_year, s.fiscal_period, s.posting_date, s.receiver_cost_center_id, s.journal_entry_id,
	s.document_number, s.amount, s.account_count, s.created_at, s.created_by`

func scanSettlement(r pgx.Row) (settlement, error) {
	var s settlement
	err := r.Scan(&s.ID, &s.OrderID, &s.FiscalYear, &s.FiscalPeriod, &s.PostingDate, &s.ReceiverID, &s.EntryID, &s.DocumentNumber, &s.Amount, &s.Accounts,
		&s.CreatedAt, &s.CreatedBy)
	return s, errs.Wrapf(err, "controlling: scan order settlement")
}

func settlementToAPI(currency string, exp int) func(settlement) gen.InternalOrderSettlement {
	return func(s settlement) gen.InternalOrderSettlement {
		return gen.InternalOrderSettlement{ID: s.ID, InternalOrderID: s.OrderID, FiscalYear: int32(s.FiscalYear), FiscalPeriod: int32(s.FiscalPeriod),
			PostingDate: s.PostingDate, ReceiverCostCenterID: s.ReceiverID, ReceiverCostCenterCode: s.ReceiverCode,
			Amount: apitypes.Money(s.Amount, currency, exp), AccountCount: s.Accounts, JournalEntryID: s.EntryID, JournalEntryNumber: s.DocumentNumber,
			CreatedAt: s.CreatedAt, CreatedBy: s.CreatedBy}
	}
}

// released refuses an order that takes no settlement or close: one never
// released, or one already closed.
func released(o Order) error {
	switch o.Status {
	case OrderReleased:
		return nil
	case OrderClosed:
		return problem.Conflict("controlling.internal_order_closed", "The internal order is closed.")
	}
	return problem.Conflict("controlling.internal_order_not_released", "The internal order is "+o.Status+"; only a released order is settled or closed.")
}

// SettleInternalOrder implements POST /v1/internal-orders/{id}:settle, at
// the order's version the caller read: it moves with the rule, so the
// receiver is the one the caller saw; a settlement does not move it.
func (e *Endpoints) SettleInternalOrder(ctx context.Context, req *gen.InternalOrderSettle, params gen.SettleInternalOrderParams) (gen.SettleInternalOrderRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, true, orderNotFound)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(o.Version); err != nil {
		return nil, err
	}
	if err := released(o); err != nil {
		return nil, err
	}
	rule, err := e.ruleOf(ctx, tx, o.ID)
	if err != nil {
		return nil, err
	}
	if !rule.Set {
		return nil, problem.Conflict("controlling.settlement_rule_missing", "The internal order has no settlement rule; give it its receiver first.")
	}
	period, err := e.regularPeriod(ctx, le.ID, req.FiscalYear, req.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	res := gen.InternalOrderSettlementResult{InternalOrderID: o.ID, InternalOrderCode: o.Code, InternalOrderName: o.Name, LegalEntityID: le.ID,
		FiscalYear: req.FiscalYear, FiscalPeriod: req.FiscalPeriod}
	unsettled, err := e.Ledger.OrderAccountBalances(ctx, le.ID, o.ID, &ledger.PeriodKey{FiscalYear: period.FiscalYear, Period: period.Number})
	if err != nil {
		return nil, fmt.Errorf("controlling: unsettled balance of internal order %s: %w", o.ID, err)
	}
	if len(unsettled) == 0 {
		return &gen.InternalOrderSettlementResultHeaders{Response: res}, nil
	}
	s, err := e.settle(ctx, tx, p, o, le, rule, period, unsettled)
	if err != nil {
		return nil, err
	}
	res.Settled = true
	res.Settlement.SetTo(settlementToAPI(le.LocalCurrency, exp)(s))
	return &gen.InternalOrderSettlementResultHeaders{Response: res}, recordSettlement(ctx, o, le.LocalCurrency, exp, s)
}

// settle posts the order's unsettled balances to the rule's receiver on
// the period's last day and keeps the settlement. The receiver is checked
// on that day first, so a rule whose receiver has ended since is refused
// as the rule's, not as a line of an entry the caller never wrote; the
// ledger checks it again under its locks.
func (e *Endpoints) settle(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o Order, le tenancy.Entity, rule settlementRule,
	period ledger.FiscalPeriod, unsettled []ledger.OrderAccountBalance) (settlement, error) {
	valid, err := e.Ledger.CostCenterValidOn(ctx, rule.ReceiverID, period.End)
	if err != nil {
		return settlement{}, fmt.Errorf("controlling: receiver of internal order %s: %w", o.ID, err)
	}
	if !valid {
		return settlement{}, problem.Conflict("controlling.settlement_receiver_not_valid", "The receiving cost center "+rule.ReceiverCode+
			" is not valid on "+period.End.Format(time.DateOnly)+", the period's last day; replace the order's settlement rule.")
	}
	lines, total, err := settlementLines(o, rule, unsettled)
	if err != nil {
		return settlement{}, err
	}
	id := uuid.Must(uuid.NewV7())
	text := "Settlement of internal order " + o.Code
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: le.ID, DocumentType: "SA", DocumentDate: period.End, PostingDate: period.End,
		Currency: le.LocalCurrency, Reference: &o.Code, HeaderText: &text, SourceContext: "controlling", SourceDocumentID: id, Lines: lines})
	if err != nil {
		return settlement{}, fmt.Errorf("controlling: post the settlement of internal order %s: %w", o.ID, err)
	}
	s, err := scanSettlement(tx.QueryRow(ctx, `INSERT INTO controlling.order_settlement AS s (tenant_id, id, internal_order_id, legal_entity_id, fiscal_year,
			fiscal_period, posting_date, receiver_cost_center_id, journal_entry_id, document_number, amount, account_count, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+settlementColumns, p.TenantID, id, o.ID, le.ID, period.FiscalYear,
		period.Number, period.End, rule.ReceiverID, entry.ID, entry.DocumentNumber, total, len(unsettled), p.ID))
	if err != nil {
		return settlement{}, fmt.Errorf("controlling: keep the settlement of internal order %s: %w", o.ID, err)
	}
	s.ReceiverCode = rule.ReceiverCode
	return s, nil
}

// settlementLines are the entry's lines, two per account in the order the
// balances came in: the order credited with its balance and the receiver
// debited with it, so the account's balance is unchanged; and the total
// moved.
func settlementLines(o Order, rule settlementRule, unsettled []ledger.OrderAccountBalance) ([]ledger.PostingLine, money.Minor, error) {
	lines := make([]ledger.PostingLine, 0, 2*len(unsettled))
	amounts := make([]money.Minor, len(unsettled))
	for i, b := range unsettled {
		amounts[i] = b.Balance
		lines = append(lines, ledger.PostingLine{AccountID: b.AccountID, Amount: -b.Balance, InternalOrderID: &o.ID},
			ledger.PostingLine{AccountID: b.AccountID, Amount: b.Balance, CostCenterID: &rule.ReceiverID})
	}
	total, err := money.Sum(amounts...)
	if err != nil {
		return nil, 0, fmt.Errorf("controlling: total settled of internal order %s: %w", o.ID, err)
	}
	return lines, total, nil
}

// recordSettlement audits a settlement and publishes it.
func recordSettlement(ctx context.Context, o Order, currency string, exp int, s settlement) error {
	day := s.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"legal_entity_id": o.LegalEntityID, "code": o.Code, "settlement_id": s.ID, "fiscal_year": s.FiscalYear,
		"fiscal_period": s.FiscalPeriod, "posting_date": day, "receiver_cost_center_id": s.ReceiverID, "receiver_cost_center_code": s.ReceiverCode,
		"amount": s.Amount.Plain(exp), "currency": currency, "account_count": s.Accounts, "journal_entry_id": s.EntryID,
		"journal_entry_number": s.DocumentNumber}
	return audit.Record(ctx, "controlling.internal_order.settle", "controlling.internal_order", o.ID, detail,
		outbox.Event{Context: "controlling", Name: "internal_order_settled", AggregateType: "controlling.internal_order", AggregateID: o.ID,
			Payload: map[string]any{"internal_order_id": o.ID, "legal_entity_id": o.LegalEntityID, "code": o.Code, "name": o.Name, "settlement_id": s.ID,
				"fiscal_year": s.FiscalYear, "fiscal_period": s.FiscalPeriod, "posting_date": day, "receiver_cost_center_id": s.ReceiverID,
				"receiver_cost_center_code": s.ReceiverCode, "amount": money.EventAmount(s.Amount, currency, exp), "account_count": s.Accounts,
				"journal_entry_id": s.EntryID, "journal_entry_number": s.DocumentNumber}})
}

var settlementSort = pagination.Sort{Default: "posting_date", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "posting_date", Column: "s.posting_date", Type: "date"}}}

// ListInternalOrderSettlements implements GET /v1/internal-orders/{id}/settlements.
func (e *Endpoints) ListInternalOrderSettlements(ctx context.Context, params gen.ListInternalOrderSettlementsParams) (gen.ListInternalOrderSettlementsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(settlementSort, fmt.Sprintf("listInternalOrderSettlements|%s", params.ID),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, false, orderNotFound)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+settlementColumns+` FROM controlling.order_settlement s WHERE s.internal_order_id = $1`, []any{o.ID})
	rows, _ := tx.Query(ctx, q, args...)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (settlement, error) { return scanSettlement(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list the settlements of internal order %s: %w", o.ID, err)
	}
	ids := make([]uuid.UUID, len(list))
	for i, s := range list {
		ids[i] = s.ReceiverID
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("controlling: receivers of the settlements of internal order %s: %w", o.ID, err)
	}
	for i := range list {
		list[i].ReceiverCode = refs[list[i].ReceiverID].Code
	}
	items, next := apitypes.PageOf(ks, list, func(s settlement) (string, uuid.UUID) { return s.PostingDate.Format(time.DateOnly), s.ID },
		settlementToAPI(le.LocalCurrency, exp))
	return &gen.InternalOrderSettlementPage{Items: items, NextPageToken: next}, nil
}

// CloseInternalOrder implements POST /v1/internal-orders/{id}:close: the
// order is read FOR UPDATE at the version the caller read and moves from
// released to closed once. Its balance is read under that lock: a posting
// to the order holds it FOR SHARE, so every posting is either in the
// balance read here or finds the order closed.
func (e *Endpoints) CloseInternalOrder(ctx context.Context, params gen.CloseInternalOrderParams) (gen.CloseInternalOrderRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	o, le, exp, err := e.orderInCompany(ctx, tx, params.ID, true, orderNotFound)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(params.IfMatch)(o.Version); err != nil {
		return nil, err
	}
	if err := released(o); err != nil {
		return nil, err
	}
	open, err := e.Ledger.OrderAccountBalances(ctx, le.ID, o.ID, nil)
	if err != nil {
		return nil, fmt.Errorf("controlling: balance of internal order %s: %w", o.ID, err)
	}
	if len(open) > 0 {
		return nil, problem.Conflict("controlling.internal_order_not_settled", fmt.Sprintf("The internal order still holds %s %s on account %s"+
			" and a balance on %d account(s) in all; settle it first.", open[0].Balance.Plain(exp), le.LocalCurrency, open[0].AccountNumber, len(open)))
	}
	o, err = scanOrder(tx.QueryRow(ctx, `UPDATE controlling.internal_order SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 AND version = $4 RETURNING `+orderColumns, o.ID, OrderClosed, p.ID, o.Version))
	if err != nil {
		return nil, fmt.Errorf("controlling: close internal order %s: %w", params.ID, err)
	}
	os, err := e.named(ctx, o)
	if err != nil {
		return nil, err
	}
	if err := recordOrder(ctx, os[0], "close", "closed"); err != nil {
		return nil, err
	}
	return &gen.CloseInternalOrderOKHeaders{ETag: gen.NewOptString(etag.Format(o.Version)), Response: orderToAPI(os[0])}, nil
}
