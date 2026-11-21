package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// CountRange is the document type count documents draw their numbers
// from (configuration number_ranges, ADR-0004).
const CountRange = "physical_inventory"

// Count document statuses (ADR-0082 As built S2c, S2d): counting until
// every item is counted, then counted; pending_approval while differences
// above the company's tolerance wait for approval; posted, rejected or
// withdrawn once closed, which lifts the posting block.
const (
	CountCounting        = "counting"
	CountCounted         = "counted"
	CountPendingApproval = "pending_approval"
	CountPosted          = "posted"
	CountRejected        = "rejected"
	CountWithdrawn       = "withdrawn"
)

func init() {
	// openCount and blocked refuse these first, naming the document; the
	// database's answers should one pass them.
	problem.RegisterConstraint("count_item_one_open", func() *problem.Problem {
		return problem.Conflict("inventory.count_open", "A material is counted at a storage location by one open count document at a time.")
	})
	problem.RegisterConstraint("count_posting_block", func() *problem.Problem {
		return problem.Conflict("inventory.posting_blocked", "A goods movement moves a material at a storage location while a count of it there is open.")
	})
}

// CountItem is an item of a count document (SAP ISEG): a material, its
// book quantity in its base unit as the document was created and, once
// counted, the quantity counted in the base unit and as it was entered.
type CountItem struct {
	ItemNo     int16      `json:"item_no"`
	MaterialID uuid.UUID  `json:"material_id"`
	Book       string     `json:"book_quantity"`
	Counted    *string    `json:"counted_quantity"`
	Entry      *string    `json:"entry_quantity"`
	EntryUnit  *string    `json:"entry_unit"`
	CountedAt  *time.Time `json:"counted_at"`
	CountedBy  *uuid.UUID `json:"counted_by"`
	Open       bool       `json:"open"`
	// Difference (counted less book), Amount, the value it moved in the
	// document's currency, and the material document that moved it, once
	// the document is posted (S2d).
	Difference             *string      `json:"difference"`
	Amount                 *money.Minor `json:"amount"`
	MaterialDocumentID     *uuid.UUID   `json:"material_document_id"`
	MaterialDocumentNumber *string      `json:"material_document_number"`
}

// CountDocument is a count document (SAP IKPF) of a plant's storage
// location with its items in item order.
type CountDocument struct {
	ID                uuid.UUID
	LegalEntityID     uuid.UUID
	FiscalYear        int16
	Number            string
	PlantID           uuid.UUID
	StorageLocationID uuid.UUID
	DocumentDate      time.Time
	Status            string
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
	// WorkflowRequestID is the approval request of differences above the
	// company's tolerance and DifferenceValue the value it was asked for
	// (S2d), nil when none was asked; Currency and Exponent are what that
	// value and the items' are in, the company's, "" until either is.
	WorkflowRequestID *uuid.UUID
	DifferenceValue   *money.Minor
	Currency          string
	Exponent          int
	Items             []CountItem
}

// materialIDs are the materials the document counts, in item order.
func (d CountDocument) materialIDs() []uuid.UUID {
	out := make([]uuid.UUID, len(d.Items))
	for i, it := range d.Items {
		out[i] = it.MaterialID
	}
	return out
}

// countSelect reads count documents with their items in item order.
const countSelect = `SELECT d.id, d.legal_entity_id, d.fiscal_year, d.number, d.plant_id, d.storage_location_id, d.document_date, d.status, d.version,
		d.created_at, d.created_by, d.updated_at, d.updated_by, d.workflow_request_id, d.difference_value, coalesce(d.currency, ''),
		coalesce(c.exponent, 0),
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'material_id', i.material_id, 'book_quantity', trim_scale(i.book_quantity)::text,
				'counted_quantity', trim_scale(i.counted_quantity)::text, 'entry_quantity', trim_scale(i.entry_quantity)::text,
				'entry_unit', i.entry_uom_code, 'counted_at', i.counted_at, 'counted_by', i.counted_by, 'open', i.open,
				'difference', trim_scale(i.difference)::text, 'amount', i.amount, 'material_document_id', i.material_document_id,
				'material_document_number', m.number) ORDER BY i.item_no)
			FROM inventory.count_item i LEFT JOIN inventory.material_document m ON m.tenant_id = i.tenant_id AND m.id = i.material_document_id
			WHERE i.tenant_id = d.tenant_id AND i.count_document_id = d.id) AS items
	FROM inventory.count_document d
	LEFT JOIN ref.currency c ON c.code = d.currency`

func scanCount(row pgx.Row) (CountDocument, error) {
	var d CountDocument
	err := row.Scan(&d.ID, &d.LegalEntityID, &d.FiscalYear, &d.Number, &d.PlantID, &d.StorageLocationID, &d.DocumentDate, &d.Status, &d.Version,
		&d.CreatedAt, &d.CreatedBy, &d.UpdatedAt, &d.UpdatedBy, &d.WorkflowRequestID, &d.DifferenceValue, &d.Currency, &d.Exponent, &d.Items)
	return d, errs.Wrapf(err, "scan count document")
}

// loadCount reads a count document of a company the caller may act in,
// another company's not found; lock takes its row lock.
func loadCount(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (CountDocument, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return CountDocument{}, fmt.Errorf("inventory: company access for count document %s: %w", id, err)
	}
	q := countSelect + ` WHERE d.id = $1`
	if lock {
		q += ` FOR UPDATE OF d`
	}
	d, err := scanCount(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(d.LegalEntityID)) {
		return CountDocument{}, problem.NotFoundOf("count document")
	}
	if err != nil {
		return CountDocument{}, fmt.Errorf("inventory: read count document %s: %w", id, err)
	}
	return d, nil
}

// CountInput is a count document as a request creates it: a plant's
// storage location and the materials counted there.
type CountInput struct {
	PlantID, StorageLocationID uuid.UUID
	Materials                  []uuid.UUID
}

// CreateCount creates a count document (ADR-0082 As built S2c; SAP MI01):
// the location and materials checked, the valuations and the stock rows
// of the materials there locked in the order every posting takes them,
// no material under another open count there, the book quantities read
// under those locks, the document dated the company's date today and
// numbered from its physical_inventory range, and the audit record. From
// then on the posting block holds (blocked; count_posting_block).
func (e *Endpoints) CreateCount(ctx context.Context, in CountInput) (CountDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CountDocument{}, err
	}
	r, err := e.read(ctx, in.Materials, []uuid.UUID{in.PlantID, in.StorageLocationID}, nil, nil)
	if err != nil {
		return CountDocument{}, err
	}
	plant, err := countLocation(in, r)
	if err != nil {
		return CountDocument{}, err
	}
	lines, err := countLines(in, r)
	if err != nil {
		return CountDocument{}, err
	}
	c, err := e.companyOf(ctx, plant.LegalEntityID)
	if err != nil {
		return CountDocument{}, err
	}
	book, err := freeze(ctx, tx, p, c.le.ID, lines, r)
	if err != nil {
		return CountDocument{}, err
	}
	n, err := e.countNumber(ctx, c)
	if err != nil {
		return CountDocument{}, err
	}
	d, err := insertCount(ctx, tx, p, in, c, n, book)
	if err != nil {
		return CountDocument{}, err
	}
	return d, recordCount(ctx, "inventory.count_document.create", d, r)
}

// countLocation checks the document's plant, an active plant of a company
// the caller may act in, and its storage location, an active one under it.
func countLocation(in CountInput, r refs) (tenancy.Unit, error) {
	u, ok := r.units[in.PlantID]
	if !ok || u.Kind != kindPlant || u.Status != tenancy.Active || !r.access.Allows(u.LegalEntityID) {
		return tenancy.Unit{}, problem.Invalid("inventory.plant_invalid", "plant_id", "No active plant with this id in a company you can access.")
	}
	l, ok := r.units[in.StorageLocationID]
	if !ok || l.Kind != kindStorageLocation || l.Status != tenancy.Active || l.ParentID == nil || *l.ParentID != in.PlantID {
		return tenancy.Unit{}, problem.Invalid("inventory.storage_location_invalid", "storage_location_id", "No active storage location with this id under plant "+
			u.Code+".")
	}
	return u, nil
}

// countLines checks each material counted and makes a line of no quantity
// of it at the document's location, numbered as its item: a stocked
// material of the tenant, not pending (a blocked one's stock is still
// counted), kept at the storage location, named once.
func countLines(in CountInput, r refs) ([]Line, error) {
	lines := make([]Line, 0, len(in.Materials))
	for i, id := range in.Materials {
		field := fmt.Sprintf("items[%d].material_id", i)
		m, ok := r.materials[id]
		switch {
		case !ok:
			return nil, problem.Invalid("inventory.material_invalid", field, "No material with this id.")
		case m.Status == materials.Pending:
			return nil, problem.Invalid("inventory.material_not_usable", field, "Material "+m.Number+" is pending its approval; it has no stock to count.")
		case !m.Stocked():
			return nil, problem.Invalid("inventory.material_not_stocked", field, "Material "+m.Number+" is of type "+m.Type+", which is kept in no stock.")
		case !hasPlant(m, in.PlantID):
			return nil, problem.Invalid("inventory.plant_not_extended", field, "Material "+m.Number+" is not extended to plant "+r.units[in.PlantID].Code+".")
		case !m.KeptAt(in.PlantID, in.StorageLocationID):
			return nil, problem.Invalid("inventory.storage_location_not_extended", field, "Material "+m.Number+" is not kept at storage location "+
				r.units[in.StorageLocationID].Code+".")
		case m.BatchManaged:
			return nil, problem.Invalid("inventory.batch_not_supported", field, "Material "+m.Number+" is managed in batches; a count by batch "+
				"is not built yet.")
		case m.SerialManaged:
			return nil, problem.Invalid("inventory.serial_not_supported", field, "Material "+m.Number+" has a serial number profile; a count by "+
				"serial number is not built yet.")
		case slices.Contains(in.Materials[:i], id):
			return nil, problem.Invalid("inventory.duplicate_material", field, "Material "+m.Number+" is counted once in a document.")
		}
		lines = append(lines, Line{LineNo: int16(i + 1), ItemNo: int16(i + 1), MaterialID: id, PlantID: in.PlantID, StorageLocationID: in.StorageLocationID,
			Quantity: "0"}) // at most 100 items (the contract's)
	}
	return lines, nil
}

// freeze locks the materials' valuations in the plant (refusing one not
// valued, whose difference could not be posted) and their stock rows at
// the storage location, in the order every posting locks them, refuses a
// material another open count holds, and answers each one's book
// quantity by item number. A posting that came first is in the quantity;
// one that comes after waits for the locks and then meets the block.
func freeze(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, lines []Line, r refs) (map[int16]string, error) {
	if _, err := lockValuations(ctx, tx, lines, r); err != nil {
		return nil, err
	}
	keys := make([]stockKey, len(lines))
	for i, l := range lines {
		keys[i] = keyOf(l)
	}
	slices.SortFunc(keys, stockKey.compare)
	stock, err := lockStock(ctx, tx, p, company, keys, r)
	if err != nil {
		return nil, err
	}
	if k, number, ok, err := openCount(ctx, tx, keys); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return nil, problem.Conflict("inventory.count_open", r.counted(k, number)+"; it is counted again once that document's differences are "+
			"posted, rejected or withdrawn.")
	}
	out := make(map[int16]string, len(lines))
	for _, l := range lines {
		out[l.ItemNo] = money.PlainQuantity(stock[keyOf(l)])
	}
	return out, nil
}

// openCount is an open count of one of the stock keys, its document's
// number with it, ok = false when there is none. The caller holds the
// keys' stock rows' locks, which a count takes before it writes its items,
// so it sees every count created before it and none can be created behind
// it.
func openCount(ctx context.Context, tx pgx.Tx, keys []stockKey) (k stockKey, number string, ok bool, err error) {
	ms, plants, locations := make([]uuid.UUID, len(keys)), make([]uuid.UUID, len(keys)), make([]uuid.UUID, len(keys))
	for i, key := range keys {
		ms[i], plants[i], locations[i] = key.material, key.plant, key.location
	}
	err = tx.QueryRow(ctx, `SELECT i.material_id, i.plant_id, i.storage_location_id, d.number FROM inventory.count_item i
		JOIN unnest($1::uuid[], $2::uuid[], $3::uuid[]) AS x (m, p, l) ON i.material_id = x.m AND i.plant_id = x.p AND i.storage_location_id = x.l
		JOIN inventory.count_document d ON d.tenant_id = i.tenant_id AND d.id = i.count_document_id
		WHERE i.open ORDER BY i.material_id, i.plant_id, i.storage_location_id LIMIT 1`, ms, plants, locations).Scan(&k.material, &k.plant, &k.location, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		return stockKey{}, "", false, nil
	}
	if err != nil {
		return stockKey{}, "", false, fmt.Errorf("inventory: open counts of %d locations: %w", len(keys), err)
	}
	return k, number, true, nil
}

// counted names a material counted at a location by a count document.
func (r refs) counted(k stockKey, number string) string {
	return fmt.Sprintf("Material %s is being counted at %s by count document %s", r.materials[k.material].Number, r.where(k), number)
}

// blocked is the posting block (ADR-0082 As built S2c; SAP MI01's
// indicator): a posting moving a material at a storage location an open
// count document counts is refused (409 inventory.posting_blocked),
// naming the material, the location and the document. takeStock asks it
// under the stock rows' locks; the database's count_posting_block refuses
// what passes it.
func blocked(ctx context.Context, tx pgx.Tx, keys []stockKey, r refs) error {
	k, number, ok, err := openCount(ctx, tx, keys)
	if err != nil || !ok {
		return err
	}
	return problem.Conflict("inventory.posting_blocked", r.counted(k, number)+"; no goods movement moves it there until the document's "+
		"differences are posted, rejected or withdrawn.")
}

// countNumber numbers a count document from its company's
// physical_inventory range of the fiscal year of the company's date.
func (e *Endpoints) countNumber(ctx context.Context, c companyOf) (numbering, error) {
	year, err := e.Calendar.FiscalYear(ctx, c.le.ID, c.day.Date)
	if err != nil {
		return numbering{}, fmt.Errorf("inventory: fiscal year of %s in company %s: %w", c.day.Date.Format(time.DateOnly), c.le.Code, err)
	}
	number, err := e.Numbers.Next(ctx, c.le.ID, CountRange, year)
	if err != nil {
		return numbering{}, fmt.Errorf("inventory: number a count document in company %s: %w", c.le.Code, err)
	}
	return numbering{id: uuid.Must(uuid.NewV7()), year: year, number: number}, nil
}

// insertCount writes a count document, counting, with its items and
// their book quantities, and reads it back.
func insertCount(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in CountInput, c companyOf, n numbering, book map[int16]string) (CountDocument, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.count_document (tenant_id, id, legal_entity_id, fiscal_year, number, plant_id, storage_location_id,
			document_date, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		p.TenantID, n.id, c.le.ID, n.year, n.number, in.PlantID, in.StorageLocationID, c.day.Date, p.ID); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: insert count document %s: %w", n.number, err)
	}
	quantities := make([]string, len(in.Materials))
	for i := range in.Materials {
		quantities[i] = book[int16(i+1)] // at most 100 items (the contract's)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.count_item (tenant_id, count_document_id, item_no, material_id, plant_id, storage_location_id,
			book_quantity)
		SELECT $1, $2, x.n, x.m, $3, $4, x.q::numeric FROM unnest($5::uuid[], $6::text[]) WITH ORDINALITY AS x (m, q, n) ORDER BY x.n`,
		p.TenantID, n.id, in.PlantID, in.StorageLocationID, in.Materials, quantities); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: items of count document %s: %w", n.number, err)
	}
	return loadCount(ctx, tx, n.id, false)
}

// recordCount adds the audit record of a count document's step (ADR-0011):
// its number, company, location, status and items, each naming its
// material by id and number with its book quantity and, once counted, the
// quantity counted and, once posted, its difference, value and material
// document; the approval request and the value it asked for (S2d). A
// count document publishes no event: the stock moves only by the material
// documents its differences post, which publish MaterialDocumentPosted.
func recordCount(ctx context.Context, action string, d CountDocument, r refs) error {
	items := make([]map[string]any, len(d.Items))
	for i, it := range d.Items {
		m := r.materials[it.MaterialID]
		item := map[string]any{"item_no": it.ItemNo, "material_id": it.MaterialID, "material_number": m.Number, "unit": m.BaseUnit,
			"book_quantity": it.Book, "counted_quantity": it.Counted, "difference": it.Difference, "material_document_id": it.MaterialDocumentID}
		if it.Amount != nil {
			item["difference_value"] = money.EventAmount(*it.Amount, d.Currency, d.Exponent)
		}
		items[i] = item
	}
	detail := map[string]any{"number": d.Number, "legal_entity_id": d.LegalEntityID, "plant_id": d.PlantID, "plant_code": r.units[d.PlantID].Code,
		"storage_location_id": d.StorageLocationID, "storage_location_code": r.units[d.StorageLocationID].Code, "status": d.Status, "items": items,
		"workflow_request_id": d.WorkflowRequestID}
	if d.DifferenceValue != nil {
		detail["difference_value"] = money.EventAmount(*d.DifferenceValue, d.Currency, d.Exponent)
	}
	return audit.Record(ctx, action, "inventory.count_document", d.ID, detail) // its error names the action
}
