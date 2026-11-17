package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a dunning block (T76e): BlockActive, it holds for the
// runs at a key date up to its end date; BlockReleased, released before and holding
// for none.
const (
	BlockActive   = "active"
	BlockReleased = "released"
)

// Block is a row of collections.dunning_block (ADR-0065 As built, T76e):
// a company does not dun the customer, or the customer's open item Item
// (its journal line, with DocumentNumber), for runs at a key date up to
// ValidTo, unless it was released (ReleasedAt, ReleasedBy).
// CustomerNumber is the customer's when it was placed.
type Block struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	Item           *ledger.ItemRef
	DocumentNumber *string
	Reason         string
	ValidTo        time.Time
	ReleasedAt     *time.Time
	ReleasedBy     *uuid.UUID
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

// Status is BlockReleased for a released block, BlockActive otherwise.
func (b Block) Status() string {
	if b.ReleasedAt != nil {
		return BlockReleased
	}
	return BlockActive
}

const blockSelect = `SELECT b.id, b.legal_entity_id, b.customer_id, b.customer_number, b.fiscal_year, b.journal_entry_id, b.line_no, b.document_number,
		b.reason, b.valid_to, b.released_at, b.released_by, b.version, b.created_at, b.created_by, b.updated_at, b.updated_by
	FROM collections.dunning_block b`

func scanBlock(row pgx.Row) (Block, error) {
	var b Block
	var year *int16
	var entry *uuid.UUID
	var line *int32
	err := row.Scan(&b.ID, &b.LegalEntityID, &b.CustomerID, &b.CustomerNumber, &year, &entry, &line, &b.DocumentNumber, &b.Reason, &b.ValidTo,
		&b.ReleasedAt, &b.ReleasedBy, &b.Version, &b.CreatedAt, &b.CreatedBy, &b.UpdatedAt, &b.UpdatedBy)
	if err == nil && entry != nil { // dunning_block_item_check: the line is named whole or not at all
		b.Item = &ledger.ItemRef{FiscalYear: *year, EntryID: *entry, LineNo: *line}
	}
	return b, errs.Wrapf(err, "scan dunning block")
}

// readBlock reads a block of a legal entity the caller may act in, under
// its row lock when forUpdate (ADR-0008); another company's is not
// found, so ids cannot be probed (04 §4.9).
func readBlock(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Block, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Block{}, fmt.Errorf("collections: company access for dunning block %s: %w", id, err)
	}
	q := blockSelect + ` WHERE b.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF b`
	}
	b, err := scanBlock(tx.QueryRow(ctx, q, id))
	if err := authz.Visible(err, access, b.LegalEntityID, problem.NotFoundOf("dunning block"), "collections: read dunning block %s", id); err != nil {
		return Block{}, err
	}
	return b, nil
}

// GetBlock returns a dunning block of a legal entity the caller may act
// in.
func GetBlock(ctx context.Context, id uuid.UUID) (Block, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Block{}, err
	}
	return readBlock(ctx, tx, id, false)
}

// block is a dunning block as a run reads it: its id and its customer's
// number.
type block struct {
	ID             uuid.UUID
	CustomerNumber string
}

// blocksInForce reads the company's blocks that hold for a run at the key
// date (not released, ending on or after it), by item and by customer,
// over the index dunning_block_in_force. Of several blocks of one item or
// customer the latest placed is named.
func blocksInForce(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, keyDate time.Time) (map[ledger.ItemRef]block, map[uuid.UUID]block, error) {
	rows, _ := tx.Query(ctx, `SELECT id, customer_id, customer_number, fiscal_year, journal_entry_id, line_no FROM collections.dunning_block
		WHERE legal_entity_id = $1 AND released_at IS NULL AND valid_to >= $2 ORDER BY created_at, id`, legalEntity, keyDate)
	items, customers := map[ledger.ItemRef]block{}, map[uuid.UUID]block{}
	var b block
	var customer uuid.UUID
	var year *int16
	var entry *uuid.UUID
	var line *int32
	_, err := pgx.ForEachRow(rows, []any{&b.ID, &customer, &b.CustomerNumber, &year, &entry, &line}, func() error {
		if entry == nil {
			customers[customer] = b
		} else {
			items[ledger.ItemRef{FiscalYear: *year, EntryID: *entry, LineNo: *line}] = b
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("collections: read the dunning blocks of company %s: %w", legalEntity, err)
	}
	return items, customers, nil
}

// BlockInput is a dunning block as a client places it: the company, the
// customer, the customer's open item by its journal line (nil: the whole
// customer), the reason and the last key date it holds for.
type BlockInput struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	Item          *ledger.ItemRef
	Reason        string
	ValidTo       time.Time
}

// PlaceBlock places a dunning block in a company the caller may act in
// (ADR-0065 As built, T76e): the customer is a business partner of the
// tenant, an item one of the company's open items the customer owes, the
// end date not before the company's today and at most LastKeyDate. The
// audit record is written in the request's transaction; nothing is
// posted and no item changes.
func (e *Endpoints) PlaceBlock(ctx context.Context, in BlockInput) (Block, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Block{}, err
	}
	if err := problem.RequireText("reason", in.Reason); err != nil {
		return Block{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Block{}, fmt.Errorf("collections: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Block{}, fmt.Errorf("collections: legal entity of a dunning block: %w", err)
	}
	if err := e.blockEnd(ctx, le, in.ValidTo); err != nil {
		return Block{}, err
	}
	number, err := e.blockedCustomer(ctx, in.CustomerID)
	if err != nil {
		return Block{}, err
	}
	var year *int16
	var entry *uuid.UUID
	var line *int32
	var document *string
	if in.Item != nil {
		it, err := e.customersItem(ctx, le, in.CustomerID, *in.Item)
		if err != nil {
			return Block{}, err
		}
		year, entry, line, document = &it.FiscalYear, &it.EntryID, &it.LineNo, &it.Number
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO collections.dunning_block (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year,
			journal_entry_id, line_no, document_number, reason, valid_to, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		p.TenantID, id, le.ID, in.CustomerID, number, year, entry, line, document, in.Reason, in.ValidTo, p.ID); err != nil {
		return Block{}, fmt.Errorf("collections: place a dunning block on customer %s: %w", in.CustomerID, err)
	}
	b, err := readBlock(ctx, tx, id, false)
	if err != nil {
		return Block{}, err
	}
	return b, recordBlock(ctx, "collections.dunning_block.place", b, map[string]any{"reason": b.Reason})
}

// blockEnd refuses an end date before the company's today, when the
// block would hold for no run started from now on, or after LastKeyDate,
// the latest key date of a run.
func (e *Endpoints) blockEnd(ctx context.Context, le tenancy.Entity, validTo time.Time) error {
	today, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return fmt.Errorf("collections: company %s's day: %w", le.Code, err)
	}
	if validTo.Before(today.Date) || validTo.After(LastKeyDate) {
		return problem.Invalid("collections.invalid_block_end", "valid_to", fmt.Sprintf("A dunning block holds up to an end date from company %s's "+
			"today, %s (%s), to %s.", le.Code, today.Date.Format(time.DateOnly), today.Zone, LastKeyDate.Format(time.DateOnly)))
	}
	return nil
}

// blockedCustomer is the number of the customer a block names, a
// business partner of the tenant; another id is refused, naming the
// field.
func (e *Endpoints) blockedCustomer(ctx context.Context, customer uuid.UUID) (string, error) {
	names, err := e.Partners.Names(ctx, []uuid.UUID{customer})
	if err != nil {
		return "", fmt.Errorf("collections: customer %s of a dunning block: %w", customer, err)
	}
	named, ok := names[customer]
	if !ok {
		return "", problem.ReferenceNotFound("customer_id", "business partner", "a business partner of the tenant")
	}
	return named.Number, nil
}

// customersItem is the company's open item at the journal line that the
// customer owes; another line, or another customer's item, is refused,
// naming the journal entry.
func (e *Endpoints) customersItem(ctx context.Context, le tenancy.Entity, customer uuid.UUID, ref ledger.ItemRef) (ledger.ReceivableItem, error) {
	it, err := e.openItem(ctx, le, ref)
	if err == nil && it.PartnerID != customer {
		err = problem.Invalid("collections.item_not_open", "journal_entry_id", fmt.Sprintf("Line %d of the journal entry of fiscal year %d is "+
			"another customer's open item, not one customer %s owes company %s.", ref.LineNo, ref.FiscalYear, customer, le.Code))
	}
	return it, err
}

// ReleaseBlock releases an active block once, with the caller's version
// of it (ADR-0008): the block is read under its row lock, its version
// compared, a released block refused (409 collections.block_released),
// and the update is guarded by its being active.
func (e *Endpoints) ReleaseBlock(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Block, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Block{}, err
	}
	b, err := readBlock(ctx, tx, id, true)
	if err != nil {
		return Block{}, err
	}
	if err := ifMatch(b.Version); err != nil {
		return Block{}, err
	}
	if b.ReleasedAt != nil {
		return Block{}, problem.Conflict("collections.block_released", "The dunning block is released already; a block is released once.")
	}
	if _, err := tx.Exec(ctx, `UPDATE collections.dunning_block SET released_at = now(), released_by = $2, version = version + 1, updated_by = $2
		WHERE id = $1 AND released_at IS NULL`, id, p.ID); err != nil {
		return Block{}, fmt.Errorf("collections: release dunning block %s: %w", id, err)
	}
	if b, err = readBlock(ctx, tx, id, false); err != nil {
		return Block{}, err
	}
	return b, recordBlock(ctx, "collections.dunning_block.release", b, map[string]any{})
}

// recordBlock adds the audit record of a change of a block (detail: what
// the change set), with its company, customer, item and end date
// (ADR-0011). A block publishes no event: it changes nothing outside the
// context, and a run started afterwards names the items it held back.
// Without events audit.Record fails only outside a unit of work, which
// reqctx.Request refused before.
func recordBlock(ctx context.Context, action string, b Block, detail map[string]any) error {
	detail["legal_entity_id"], detail["customer_id"], detail["valid_to"], detail["status"] = b.LegalEntityID, b.CustomerID,
		b.ValidTo.Format(time.DateOnly), b.Status()
	if b.Item != nil {
		detail["fiscal_year"], detail["journal_entry_id"], detail["line_no"] = b.Item.FiscalYear, b.Item.EntryID, b.Item.LineNo
	}
	return audit.Record(ctx, action, "collections.dunning_block", b.ID, detail)
}
