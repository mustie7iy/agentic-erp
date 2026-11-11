package procurement

// GR/IR clearing of closed orders (T72e, SAP MR11; ADR-0034 As built,
// 03-data-model §3.8). A closed order takes no further receipts or
// invoices, so a GR/IR position it is left with (received and invoiced
// quantities that will not agree) stays open until it is cleared: an
// accountant proposes clearing the order's open positions as they stand,
// a controller approves (approvals.GRIRClearing; never the proposer), and
// the approval posts a WE entry through ledger.Postings whose GR/IR lines
// clear each position against the price difference account or, for an
// order item that is an asset's, against its asset clearing account
// (ADR-0057 §6 and As built B10 step 3; product owner 2026-11-10). The ledger
// refuses the posting when a position moved since the proposal
// (GRIRItem.Clears), so what posts is what was approved.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses of a GR/IR clearing besides Rejected and Cancelled.
const (
	ClearingPending = "pending"
	ClearingPosted  = "posted"
)

func init() {
	problem.RegisterConstraint("gr_ir_clearing_one_pending", func() *problem.Problem {
		return problem.Conflict("procurement.gr_ir_clearing_pending", "The purchase order has a pending GR/IR clearing; it is decided or withdrawn first.")
	})
}

// GRIRClearing is a row of procurement.gr_ir_clearing with its items.
type GRIRClearing struct {
	ID                 uuid.UUID
	PurchaseOrderID    uuid.UUID
	OrderNumber        string
	LegalEntityID      uuid.UUID
	Status             string
	Reason             string
	PostingDate        time.Time
	Currency           string
	Exponent           int
	OffsetAccount      ledger.AccountRef
	Items              []GRIRClearingItem
	ApprovalRequestID  uuid.UUID
	JournalEntryID     *uuid.UUID
	JournalEntryYear   *int16
	JournalEntryNumber *string
	DecisionComment    *string
	Version            int32
	CreatedAt          time.Time
	CreatedBy          uuid.UUID
	UpdatedAt          time.Time
	UpdatedBy          uuid.UUID
}

// GRIRClearingItem is one order item's position a clearing clears, as it
// stood when proposed: invoiced + cleared + migrated - received, and the balance in
// the order's currency; and the account it clears against, as shown when
// proposed: the clearing's price difference account or, for an order item
// that is an asset's, its asset clearing account (ADR-0057 §6; product
// owner 2026-11-10).
type GRIRClearingItem struct {
	POItemNo      int32             `json:"po_item_no"`
	Description   string            `json:"description"`
	Quantity      string            `json:"quantity"`
	Unit          string            `json:"unit"`
	Amount        money.Minor       `json:"amount"`
	OffsetAccount ledger.AccountRef `json:"offset_account"`
}

// Total is the items' balances summed, and Gross their absolute values
// summed: what the GR/IR account moves by, and what the clearing posts to
// the accounts its items clear against. ok is false when either is out of range,
// which the proposal refuses.
func (c GRIRClearing) Total() (total, gross money.Minor, ok bool) {
	amounts, absolute := make([]money.Minor, len(c.Items)), make([]money.Minor, len(c.Items))
	for i, it := range c.Items {
		amounts[i], absolute[i] = it.Amount, it.Amount
		if it.Amount < 0 {
			var err error
			if absolute[i], err = money.Diff(0, it.Amount); err != nil {
				return 0, 0, false
			}
		}
	}
	total, err := money.Sum(amounts...)
	if err != nil {
		return 0, 0, false
	}
	gross, err = money.Sum(absolute...)
	return total, gross, err == nil
}

// GRIRClearingInput is a proposed clearing; nil Items: every open position.
type GRIRClearingInput struct {
	Reason      string
	PostingDate time.Time
	Items       []int32
}

const clearingSelect = `SELECT c.id, c.purchase_order_id, o.document_number, c.legal_entity_id, c.status, c.reason, c.posting_date, c.currency, cur.exponent,
		c.offset_account_id, c.offset_account_number, c.offset_account_name,
		coalesce((SELECT json_agg(json_build_object('po_item_no', i.po_item_no, 'description', oi.description, 'quantity', trim_scale(i.quantity)::text,
			'unit', i.uom_code, 'amount', i.amount,
			-- an item without an account of its own clears against the clearing's (000264)
			'offset_account', json_build_object('ID', coalesce(i.offset_account_id, c.offset_account_id),
				'Number', coalesce(i.offset_account_number, c.offset_account_number), 'Name', coalesce(i.offset_account_name, c.offset_account_name)))
			ORDER BY i.po_item_no)
		 FROM procurement.gr_ir_clearing_item i
		 JOIN procurement.purchase_order_item oi ON oi.tenant_id = i.tenant_id AND oi.purchase_order_id = c.purchase_order_id AND oi.item_no = i.po_item_no
		 WHERE i.tenant_id = c.tenant_id AND i.gr_ir_clearing_id = c.id), '[]'),
		c.approval_request_id, c.journal_entry_id, c.journal_entry_year, c.journal_entry_number, c.decision_comment, c.version, c.created_at, c.created_by,
		c.updated_at, c.updated_by
	FROM procurement.gr_ir_clearing c
	JOIN procurement.purchase_order o ON o.tenant_id = c.tenant_id AND o.id = c.purchase_order_id
	JOIN ref.currency cur ON cur.code = c.currency`

func scanClearing(row pgx.Row) (GRIRClearing, error) {
	var c GRIRClearing
	a := &c.OffsetAccount
	if err := row.Scan(&c.ID, &c.PurchaseOrderID, &c.OrderNumber, &c.LegalEntityID, &c.Status, &c.Reason, &c.PostingDate, &c.Currency, &c.Exponent,
		&a.ID, &a.Number, &a.Name, &c.Items, &c.ApprovalRequestID, &c.JournalEntryID, &c.JournalEntryYear, &c.JournalEntryNumber, &c.DecisionComment,
		&c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy); err != nil {
		return GRIRClearing{}, fmt.Errorf("scan GR/IR clearing: %w", err)
	}
	return c, nil
}

// GetGRIRClearing returns a clearing of an order the caller can see.
func GetGRIRClearing(ctx context.Context, id uuid.UUID) (GRIRClearing, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return GRIRClearing{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return GRIRClearing{}, err
	}
	c, err := scanClearing(tx.QueryRow(ctx, clearingSelect+` WHERE c.id = $1`, id))
	if err = authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("GR/IR clearing"), "procurement: get GR/IR clearing %s", id); err != nil {
		return GRIRClearing{}, err
	}
	return c, nil
}

// createGRIRClearing proposes clearing a closed order's open GR/IR
// positions, or those of the items named, and opens its approval request.
// The order is locked, so a clearing is proposed from what it reads and
// one pending clearing per order holds (gr_ir_clearing_one_pending).
func (e *Endpoints) createGRIRClearing(ctx context.Context, order uuid.UUID, in GRIRClearingInput) (GRIRClearing, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return GRIRClearing{}, err
	}
	o, err := getOrder(ctx, tx, order, true)
	if err != nil {
		return GRIRClearing{}, err
	}
	if o.Status != Closed {
		return GRIRClearing{}, statusConflict("purchase order", o.Status, "a GR/IR clearing", Closed)
	}
	if in.Reason = strings.TrimSpace(in.Reason); in.Reason == "" {
		return GRIRClearing{}, invalid("procurement.reason_required", "reason", "why the positions are cleared")
	}
	c, err := e.proposeClearing(ctx, o, in)
	if err != nil {
		return GRIRClearing{}, err
	}
	if c.ApprovalRequestID, err = e.openClearingRequest(ctx, o, c); err != nil {
		return GRIRClearing{}, err
	}
	if err := insertClearing(ctx, tx, p, c); err != nil {
		return GRIRClearing{}, err
	}
	created, err := GetGRIRClearing(ctx, c.ID)
	if err != nil {
		return GRIRClearing{}, err
	}
	return created, audit.Record(ctx, "procurement.gr_ir_clearing.create", "procurement.gr_ir_clearing", c.ID,
		map[string]any{"purchase_order_id": order, "document_number": o.DocumentNumber, "items": len(c.Items), "posting_date": c.PostingDate.Format(time.DateOnly),
			"approval_request_id": c.ApprovalRequestID, "reason": c.Reason})
}

// proposeClearing reads the order's GR/IR positions and keeps the open
// ones it clears, with the price difference account they clear against.
func (e *Endpoints) proposeClearing(ctx context.Context, o PurchaseOrder, in GRIRClearingInput) (GRIRClearing, error) {
	nothing := problem.Conflict("procurement.nothing_to_clear", "The purchase order "+o.DocumentNumber+" has no open GR/IR position to clear.")
	if o.Valuation != ValuationGRIR {
		return GRIRClearing{}, nothing
	}
	g, err := e.Postings.GRIROrder(ctx, o.LegalEntityID, o.ID)
	if err != nil {
		return GRIRClearing{}, err
	}
	c := GRIRClearing{ID: uuid.Must(uuid.NewV7()), PurchaseOrderID: o.ID, OrderNumber: o.DocumentNumber, LegalEntityID: o.LegalEntityID,
		Reason: in.Reason, PostingDate: in.PostingDate, Currency: o.Currency, Exponent: o.Exponent}
	open := map[int32]ledger.GRIRPosition{}
	for _, pos := range g.Positions {
		if pos.Open {
			open[pos.POItemNo] = pos
		}
	}
	nos := in.Items
	if nos == nil {
		nos = slices.Sorted(maps.Keys(open))
	}
	for _, no := range nos {
		pos, ok := open[no]
		if !ok {
			return GRIRClearing{}, invalid("procurement.invalid_item_no", "po_item_nos", fmt.Sprintf("item %d has no open GR/IR position", no))
		}
		c.Items = append(c.Items, GRIRClearingItem{POItemNo: no, Quantity: pos.Quantity, Unit: pos.Unit, Amount: pos.Balance})
	}
	switch _, _, ok := c.Total(); {
	case len(c.Items) == 0:
		return GRIRClearing{}, nothing
	case !ok:
		return GRIRClearing{}, invalid("procurement.amount_too_large", "po_item_nos", "the positions' balances and their total must be in the range of an amount")
	case g.PriceDifference == nil:
		return GRIRClearing{}, problem.Unprocessable("ledger.no_price_difference_account", "The legal entity has no price_difference account in its account determination.")
	}
	c.OffsetAccount = *g.PriceDifference
	return c, e.clearingAccounts(ctx, o, &c)
}

// clearingAccounts gives each item of the proposed clearing the account
// it clears against: the item's own account when that is an asset
// clearing account of the company (the ledger's rule, ADR-0057 §6: what
// was paid for an asset, or not paid after all, belongs to its cost),
// else the clearing's price difference account.
func (e *Endpoints) clearingAccounts(ctx context.Context, o PurchaseOrder, c *GRIRClearing) error {
	byNo := map[int32]uuid.UUID{}
	accounts := make([]uuid.UUID, 0, len(o.Items))
	for _, it := range o.Items {
		byNo[it.No] = it.AccountID
		accounts = append(accounts, it.AccountID)
	}
	assets, err := e.Postings.AssetClearingAccounts(ctx, o.LegalEntityID, accounts)
	if err != nil {
		return err
	}
	for i, it := range c.Items {
		c.Items[i].OffsetAccount = c.OffsetAccount
		if a, ok := assets[byNo[it.POItemNo]]; ok {
			c.Items[i].OffsetAccount = a
		}
	}
	return nil
}

// openClearingRequest opens the clearing's gr_ir_clearing approval
// request, whose amount is what the clearing posts to the price
// difference account (Total's gross, which the proposal checked).
func (e *Endpoints) openClearingRequest(ctx context.Context, o PurchaseOrder, c GRIRClearing) (uuid.UUID, error) {
	_, gross, _ := c.Total() // in range: proposeClearing refused it otherwise
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.GRIRClearing, ObjectID: c.ID,
		Title:  fmt.Sprintf("Clear the GR/IR positions of purchase order %s: %s", o.DocumentNumber, c.Reason),
		Amount: &gross, Currency: &c.Currency, LegalEntityID: o.LegalEntityID, Date: c.PostingDate})
	if err != nil {
		return uuid.Nil, err
	}
	if !needed {
		return uuid.Nil, fmt.Errorf("procurement: GR/IR clearing of order %s: the approval policy requires no approval, which it always must", o.ID)
	}
	return request, nil
}

func insertClearing(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c GRIRClearing) error {
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.gr_ir_clearing (tenant_id, id, purchase_order_id, legal_entity_id, reason, posting_date, currency,
			offset_account_id, offset_account_number, offset_account_name, approval_request_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`, p.TenantID, c.ID, c.PurchaseOrderID, c.LegalEntityID, c.Reason, c.PostingDate,
		c.Currency, c.OffsetAccount.ID, c.OffsetAccount.Number, c.OffsetAccount.Name, c.ApprovalRequestID, p.ID); err != nil {
		return fmt.Errorf("procurement: insert GR/IR clearing of order %s: %w", c.PurchaseOrderID, err)
	}
	nos, qty, units, amounts := make([]int32, len(c.Items)), make([]string, len(c.Items)), make([]string, len(c.Items)), make([]money.Minor, len(c.Items))
	// An item's account is kept only where it is not the clearing's.
	ids, numbers, names := make([]*uuid.UUID, len(c.Items)), make([]*string, len(c.Items)), make([]*string, len(c.Items))
	for i, it := range c.Items {
		nos[i], qty[i], units[i], amounts[i] = it.POItemNo, it.Quantity, it.Unit, it.Amount
		if a := it.OffsetAccount; a.ID != c.OffsetAccount.ID {
			ids[i], numbers[i], names[i] = &a.ID, &a.Number, &a.Name
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.gr_ir_clearing_item (tenant_id, gr_ir_clearing_id, po_item_no, quantity, uom_code, amount,
			offset_account_id, offset_account_number, offset_account_name)
		SELECT $1, $2, x.n, x.q::numeric, x.u, x.a, x.id, x.number, x.name
		FROM unnest($3::int[], $4::text[], $5::text[], $6::bigint[], $7::uuid[], $8::text[], $9::text[]) AS x(n, q, u, a, id, number, name)`,
		p.TenantID, c.ID, nos, qty, units, amounts, ids, numbers, names); err != nil {
		return fmt.Errorf("procurement: insert the items of GR/IR clearing %s: %w", c.ID, err)
	}
	return nil
}

// GRIRClearings moves a GR/IR clearing on when its gr_ir_clearing approval
// request completes (approvals.Handler), in the deciding transaction.
// Each acts only on a pending clearing that names the request, so a
// decision repeated or on another request changes nothing and fails.
type GRIRClearings struct {
	Endpoints *Endpoints
}

// Approved posts the clearing: a WE entry whose GR/IR line clears each
// item's position against the account the clearing shows for it, refused
// by the ledger when a position moved or that account is no longer the
// one since the proposal, which leaves the request pending.
func (h GRIRClearings) Approved(ctx context.Context, clearing, request uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	c, err := pendingClearing(ctx, tx, clearing, request)
	if err != nil {
		return err
	}
	o, err := readOrder(ctx, tx, c.PurchaseOrderID, false)
	if err != nil {
		return err
	}
	entry, err := h.Endpoints.Postings.Post(ctx, clearingPosting(c, o))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.gr_ir_clearing SET status = 'posted', journal_entry_id = $3, journal_entry_year = $4, journal_entry_number = $5,
			version = version + 1, updated_by = $6 WHERE id = $1 AND approval_request_id = $2 AND status = 'pending'`,
		clearing, request, entry.ID, entry.FiscalYear, entry.DocumentNumber, p.ID); err != nil {
		return fmt.Errorf("procurement: post GR/IR clearing %s: %w", clearing, err)
	}
	return audit.Record(ctx, "procurement.gr_ir_clearing.post", "procurement.gr_ir_clearing", clearing,
		map[string]any{"purchase_order_id": c.PurchaseOrderID, "approval_request_id": request, "journal_entry_id": entry.ID,
			"journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber},
		event("gr_ir_cleared", "gr_ir_clearing", clearing, clearedEvent(c, entry)))
}

// clearingPosting is the clearing's WE entry: one clearing item per
// position, at the item's valuation price, with its cost center for a
// price difference line and its account, by which the ledger judges
// again whether it is an asset's, against the account that was shown.
func clearingPosting(c GRIRClearing, o PurchaseOrder) ledger.Posting {
	byNo := map[int32]Item{}
	for _, it := range o.Items {
		byNo[it.No] = it
	}
	items := make([]ledger.GRIRItem, len(c.Items))
	for i, it := range c.Items {
		oi := byNo[it.POItemNo]
		price, unit := oi.UnitPrice, oi.PriceUnit
		if oi.ValuationPrice != nil {
			price, unit = *oi.ValuationPrice, *oi.ValuationPriceUnit
		}
		items[i] = ledger.GRIRItem{PurchaseOrderID: c.PurchaseOrderID, POItemNo: it.POItemNo, Quantity: negated(it.Quantity), Unit: it.Unit,
			UnitPrice: price, PriceUnit: unit, CostCenterID: oi.CostCenterID, ItemAccountID: &oi.AccountID,
			Clears: &ledger.GRIRClearing{Balance: it.Amount, Account: it.OffsetAccount.ID}}
	}
	text := "GR/IR clearing of purchase order " + c.OrderNumber
	return ledger.Posting{LegalEntityID: c.LegalEntityID, DocumentType: WEDocumentType, DocumentDate: c.PostingDate, PostingDate: c.PostingDate,
		Currency: c.Currency, Reference: &c.OrderNumber, HeaderText: &text, SourceContext: "procurement", SourceDocumentID: c.ID, GRIR: items}
}

func clearedEvent(c GRIRClearing, entry ledger.Entry) map[string]any {
	items := make([]map[string]any, len(c.Items))
	for i, it := range c.Items {
		items[i] = map[string]any{"po_item_no": it.POItemNo, "quantity": it.Quantity, "unit": it.Unit,
			"amount": map[string]any{"amount": it.Amount.Plain(c.Exponent), "currency": c.Currency}}
	}
	return map[string]any{"gr_ir_clearing_id": c.ID, "purchase_order_id": c.PurchaseOrderID, "document_number": c.OrderNumber,
		"legal_entity_id": c.LegalEntityID, "posting_date": c.PostingDate.Format(time.DateOnly), "journal_entry_id": entry.ID,
		"journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber, "items": items}
}

// Rejected ends the clearing unposted; its positions stay open.
func (GRIRClearings) Rejected(ctx context.Context, clearing, request uuid.UUID, comment string) error {
	return endClearing(ctx, clearing, request, Rejected, "reject", comment)
}

// Cancelled ends the clearing unposted: its requester withdrew it.
func (GRIRClearings) Cancelled(ctx context.Context, clearing, request uuid.UUID, comment string) error {
	return endClearing(ctx, clearing, request, Cancelled, "cancel", comment)
}

func endClearing(ctx context.Context, clearing, request uuid.UUID, status, action, comment string) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	c, err := pendingClearing(ctx, tx, clearing, request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.gr_ir_clearing SET status = $3, decision_comment = nullif($4, ''), version = version + 1, updated_by = $5
		WHERE id = $1 AND approval_request_id = $2 AND status = 'pending'`, clearing, request, status, comment, p.ID); err != nil {
		return fmt.Errorf("procurement: %s GR/IR clearing %s: %w", action, clearing, err)
	}
	return audit.Record(ctx, "procurement.gr_ir_clearing."+action, "procurement.gr_ir_clearing", clearing,
		map[string]any{"purchase_order_id": c.PurchaseOrderID, "approval_request_id": request, "comment": comment})
}

// pendingClearing locks the clearing and returns it if it is pending on
// the request. The decider may not see the order's company: the request
// is what authorises the decision.
func pendingClearing(ctx context.Context, tx pgx.Tx, clearing, request uuid.UUID) (GRIRClearing, error) {
	c, err := scanClearing(tx.QueryRow(ctx, clearingSelect+` WHERE c.id = $1 FOR UPDATE OF c`, clearing))
	if err != nil {
		return GRIRClearing{}, fmt.Errorf("procurement: read GR/IR clearing %s: %w", clearing, err)
	}
	if c.Status != ClearingPending || c.ApprovalRequestID != request {
		return GRIRClearing{}, fmt.Errorf("procurement: GR/IR clearing %s (%s) is not pending on request %s", clearing, c.Status, request)
	}
	return c, nil
}

// Resources is where the API reads each GR/IR clearing (approvals.Locator).
func (GRIRClearings) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/gr-ir-clearings/%s", ids), nil
}
