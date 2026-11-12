package procurement

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Requests for quotation (ADR-0069). An RFQ is free-standing: a buyer
// creates it for a company with the items asked for and the suppliers
// invited, and publishes it once; from then on nothing changes it and the
// suppliers' quotations are recorded against it.

// RFQPublished is the status of an RFQ that was published to the suppliers
// invited; before that it is a Draft.
const RFQPublished = "published"

// rfqNumberRange is the document type RFQs draw their numbers from
// (configuration number_ranges).
const rfqNumberRange = "rfq"

func init() {
	problem.RegisterConstraint("rfq_item_uom_code_fkey", unknownUnit)
}

// RFQ is a row of procurement.rfq with its items, which carry no price
// (Item's UnitPrice and PriceUnit are zero), and the suppliers invited in
// the order they were named. CompanyCode, CompanyName, Accounts and the
// suppliers' numbers and names are what the other contexts call them,
// filled when the RFQ is shown (named); a row read alone has the ids only.
type RFQ struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	CompanyCode    string
	CompanyName    string
	Accounts       map[uuid.UUID]ledger.AccountRef // the items' accounts by id
	Currency       string
	DueDate        time.Time
	Status         string
	Items          []Item
	Suppliers      []RFQSupplier
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

// RFQSupplier is a supplier invited to quote. Number and Name are the
// business partner's, read from the partner context when the RFQ is shown
// (named); a row read alone has the id only.
type RFQSupplier struct {
	ID           uuid.UUID
	Number, Name string
}

const rfqSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.currency, r.due_date, r.status, r.version, r.created_at, r.created_by,
		r.updated_at, r.updated_by,
		coalesce((SELECT json_agg(json_build_object('item_no', i.item_no, 'material_key', i.material_key, 'description', i.description,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code, 'account_id', i.account_id) ORDER BY i.item_no)
		 FROM procurement.rfq_item i WHERE i.tenant_id = r.tenant_id AND i.rfq_id = r.id), '[]'),
		ARRAY(SELECT s.supplier_id FROM procurement.rfq_supplier s WHERE s.tenant_id = r.tenant_id AND s.rfq_id = r.id ORDER BY s.invite_no)
	FROM procurement.rfq r`

func scanRFQ(row pgx.Row) (RFQ, error) {
	var r RFQ
	var suppliers []uuid.UUID
	if err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.Currency, &r.DueDate, &r.Status, &r.Version, &r.CreatedAt, &r.CreatedBy,
		&r.UpdatedAt, &r.UpdatedBy, &r.Items, &suppliers); err != nil {
		return RFQ{}, fmt.Errorf("scan RFQ: %w", err)
	}
	r.Suppliers = make([]RFQSupplier, len(suppliers))
	for i, id := range suppliers {
		r.Suppliers[i].ID = id
	}
	return r, nil
}

// getRFQ loads an RFQ of a legal entity the caller may act in (404
// otherwise, 04 §4.9); lock takes the row lock for a change.
func getRFQ(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (RFQ, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return RFQ{}, err
	}
	q := rfqSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRFQ(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("RFQ"), "procurement: get RFQ %s", id); err != nil {
		return RFQ{}, err
	}
	return r, nil
}

// supplierIDs are the ids of the suppliers invited, in their order.
func (r RFQ) supplierIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(r.Suppliers))
	for i, s := range r.Suppliers {
		ids[i] = s.ID
	}
	return ids
}

// accountRef is how an account the ledger does not hold is reported
// (idref.Resolve).
var accountRef = idref.Kind{Name: "account", Context: "ledger"}

// companyRef is how a legal entity the directory does not hold is
// reported (idref.Resolve).
var companyRef = idref.Kind{Name: "legal entity", Context: "tenancy"}

// entities reads the legal entities by id from the directory; one it does
// not hold is left out.
func (e *Endpoints) entities(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Entity, error) {
	out := make(map[uuid.UUID]tenancy.Entity, len(ids))
	for _, id := range ids {
		le, ok, err := e.LegalEntities.Entity(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = le
		}
	}
	return out, nil
}

// named gives the RFQs what their ids name (REVIEW.md: an identifier with
// the name its consumer needs): each supplier's number and name from the
// partner context and each item's account's number and name from the
// ledger, in one call each for the page, and the company's code and name
// from the directory, read once per legal entity. One the owning context
// does not hold fails the read rather than showing it unnamed: the RFQ
// was created naming it.
func (e *Endpoints) named(ctx context.Context, rfqs ...RFQ) ([]RFQ, error) {
	var suppliers, accounts, entities []uuid.UUID
	for _, r := range rfqs {
		if !slices.Contains(entities, r.LegalEntityID) {
			entities = append(entities, r.LegalEntityID)
		}
		suppliers = append(suppliers, r.supplierIDs()...)
		for _, it := range r.Items {
			accounts = append(accounts, it.AccountID)
		}
	}
	partners, err := idref.Resolve(ctx, e.Partners.Names, suppliers, supplierRef)
	if err != nil {
		return nil, fmt.Errorf("procurement: name the suppliers of RFQs: %w", err)
	}
	refs, err := idref.Resolve(ctx, e.AccountNames.AccountNames, accounts, accountRef)
	if err != nil {
		return nil, fmt.Errorf("procurement: name the accounts of RFQs: %w", err)
	}
	companies, err := idref.Resolve(ctx, e.entities, entities, companyRef)
	if err != nil {
		return nil, fmt.Errorf("procurement: name the companies of RFQs: %w", err)
	}
	for i, r := range rfqs {
		le := companies[r.LegalEntityID]
		rfqs[i].CompanyCode, rfqs[i].CompanyName, rfqs[i].Accounts = le.Code, le.Name, refs
		for j, s := range r.Suppliers {
			r.Suppliers[j].Number, r.Suppliers[j].Name = partners[s.ID].Number, partners[s.ID].Name
		}
	}
	return rfqs, nil
}

// NewRFQ is the input of createRFQ.
type NewRFQ struct {
	LegalEntityID uuid.UUID
	Currency      string
	DueDate       time.Time
	Items         []ItemInput // without prices
	Suppliers     []uuid.UUID
}

// rfqItems validates an RFQ's items: each numbered by its place, its
// material key once, a described positive quantity, on an account the
// legal entity can post to.
func (e *Endpoints) rfqItems(ctx context.Context, le uuid.UUID, in []ItemInput) ([]Item, error) {
	items := make([]Item, len(in))
	keys := map[string]bool{}
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		if err := checkItemNo(it.No, i, field); err != nil {
			return nil, err
		}
		if keys[it.MaterialKey] {
			return nil, invalid("procurement.duplicate_material_key", field+".material_key", "each material key appears once per RFQ")
		}
		keys[it.MaterialKey] = true
		if err := problem.RequireText(field+".description", it.Description); err != nil {
			return nil, err
		}
		if err := quantity(it.Quantity, field+".quantity"); err != nil {
			return nil, err
		}
		if err := e.account(ctx, le, it.AccountID, field+".account_id"); err != nil {
			return nil, err
		}
		items[i] = Item{No: int32(i + 1), MaterialKey: it.MaterialKey, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
			AccountID: it.AccountID}
	}
	return items, nil
}

// once refuses a supplier named twice, by the place of its second naming.
func once(ids []uuid.UUID) error {
	seen := map[uuid.UUID]bool{}
	for i, id := range ids {
		if seen[id] {
			return problem.Unprocessable("procurement.rfq_supplier_repeated", "A supplier is invited to an RFQ once.",
				problem.FieldError{Field: fmt.Sprintf("supplier_ids[%d]", i), Code: "duplicate", Message: "each supplier is named once"})
		}
		seen[id] = true
	}
	return nil
}

// usable requires each supplier to be one an order could be placed with
// in the legal entity (supplierAt), refused by its own field: field is its
// format, taking the supplier's place.
func (e *Endpoints) usable(ctx context.Context, le uuid.UUID, ids []uuid.UUID, field string) error {
	for i, id := range ids {
		if _, err := e.supplierAt(ctx, id, le, fmt.Sprintf(field, i)); err != nil {
			return err
		}
	}
	return nil
}

// notPast requires the due date not to be before legal entity le's date
// today in its company's time zone, and returns that date. The refusal
// names the date and the zone, so a buyer in another zone sees which day
// it was judged on.
func (e *Endpoints) notPast(ctx context.Context, le uuid.UUID, due time.Time) (time.Time, error) {
	day, zone, err := e.todayOf(ctx, le)
	if err != nil {
		return time.Time{}, err
	}
	if due.Before(day) {
		why := fmt.Sprintf("the due date must not be before the legal entity's date today, %s in %s", day.Format(time.DateOnly), zone)
		return time.Time{}, problem.Unprocessable("procurement.rfq_due_date_past", "The RFQ's due date "+due.Format(time.DateOnly)+" has passed: "+why+".",
			problem.FieldError{Field: "due_date", Code: "past", Message: why})
	}
	return day, nil
}

// createRFQ creates a draft RFQ, numbered for its company's date today.
func (e *Endpoints) createRFQ(ctx context.Context, in NewRFQ) (RFQ, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return RFQ{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID)
	if err != nil {
		return RFQ{}, err
	}
	if _, err := exponent(ctx, tx, in.Currency, "currency"); err != nil {
		return RFQ{}, err
	}
	day, err := e.notPast(ctx, le.ID, in.DueDate)
	if err != nil {
		return RFQ{}, err
	}
	items, err := e.rfqItems(ctx, le.ID, in.Items)
	if err != nil {
		return RFQ{}, err
	}
	if err := once(in.Suppliers); err != nil {
		return RFQ{}, err
	}
	if err := e.usable(ctx, le.ID, in.Suppliers, "supplier_ids[%d]"); err != nil {
		return RFQ{}, err
	}
	number, err := e.number(ctx, le.ID, rfqNumberRange, day)
	if err != nil {
		return RFQ{}, err
	}
	r := RFQ{ID: uuid.Must(uuid.NewV7()), DocumentNumber: number, LegalEntityID: le.ID, Currency: in.Currency, DueDate: in.DueDate,
		Items: items, Suppliers: make([]RFQSupplier, len(in.Suppliers))}
	for i, id := range in.Suppliers {
		r.Suppliers[i].ID = id
	}
	if err := writeRFQ(ctx, tx, p, r); err != nil {
		return RFQ{}, err
	}
	return e.recorded(ctx, tx, r.ID, "create", "created", map[string]any{"document_number": number, "legal_entity_id": le.ID, "currency": in.Currency,
		"due_date": in.DueDate.Format(time.DateOnly), "items": len(items)})
}

// writeRFQ inserts a draft RFQ with its items and the suppliers invited,
// numbered in the order given.
func writeRFQ(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r RFQ) error {
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.rfq (tenant_id, id, document_number, legal_entity_id, currency, due_date, status,
			created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'draft', $7, $7)`, p.TenantID, r.ID, r.DocumentNumber, r.LegalEntityID, r.Currency, r.DueDate, p.ID); err != nil {
		return fmt.Errorf("procurement: create RFQ %s: %w", r.DocumentNumber, err)
	}
	n := len(r.Items)
	nos, keys, descriptions, quantities, units, accounts := make([]int32, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]uuid.UUID, n)
	for i, it := range r.Items {
		nos[i], keys[i], descriptions[i], quantities[i], units[i], accounts[i] = it.No, it.MaterialKey, it.Description, it.Quantity, it.Unit, it.AccountID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.rfq_item (tenant_id, rfq_id, item_no, material_key, description, quantity, uom_code, account_id)
		SELECT $1, $2, n, k, d, q, u, a
		FROM unnest($3::int[], $4::text[], $5::text[], $6::numeric[], $7::text[], $8::uuid[]) AS x(n, k, d, q, u, a)`,
		p.TenantID, r.ID, nos, keys, descriptions, quantities, units, accounts); err != nil {
		return fmt.Errorf("procurement: write items of RFQ %s: %w", r.ID, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.rfq_supplier (tenant_id, rfq_id, supplier_id, invite_no)
		SELECT $1, $2, s, n FROM unnest($3::uuid[]) WITH ORDINALITY AS x(s, n)`, p.TenantID, r.ID, r.supplierIDs()); err != nil {
		return fmt.Errorf("procurement: write suppliers of RFQ %s: %w", r.ID, err)
	}
	return nil
}

// recorded reads the RFQ as the change left it, named as it is shown,
// audits the change and publishes it: RfqChanged for every change, and
// RfqPublished beside it for a publication, with the suppliers invited by
// id and number. A supplier's name goes into neither the events nor the
// audit record (product owner 2026-11-05: a partner may be a person).
func (e *Endpoints) recorded(ctx context.Context, tx pgx.Tx, id uuid.UUID, action, change string, detail map[string]any) (RFQ, error) {
	read, err := getRFQ(ctx, tx, id, false)
	if err != nil {
		return RFQ{}, err
	}
	shown, err := e.named(ctx, read)
	if err != nil {
		return RFQ{}, err
	}
	stored := shown[0]
	detail["supplier_ids"] = stored.supplierIDs()
	payload := func() map[string]any {
		return map[string]any{"rfq_id": stored.ID, "document_number": stored.DocumentNumber, "legal_entity_id": stored.LegalEntityID,
			"currency": stored.Currency, "due_date": stored.DueDate.Format(time.DateOnly), "item_count": len(stored.Items)}
	}
	changed := payload()
	changed["status"], changed["change"], changed["supplier_count"] = stored.Status, change, len(stored.Suppliers)
	events := []outbox.Event{event("rfq_changed", "rfq", stored.ID, changed)}
	if stored.Status == RFQPublished {
		published := payload()
		invited := make([]map[string]any, len(stored.Suppliers))
		for i, s := range stored.Suppliers {
			invited[i] = map[string]any{"supplier_id": s.ID, "supplier_number": s.Number}
		}
		published["suppliers"] = invited
		events = append(events, event("rfq_published", "rfq", stored.ID, published))
	}
	return stored, audit.Record(ctx, "procurement.rfq."+action, "procurement.rfq", stored.ID, detail, events...)
}

// publishRFQ publishes a draft RFQ once: read under its row lock at the
// version the caller read, checked again as it was when created (a
// supplier blocked since, a due date passed since), then moved to
// published; a second publication finds it published and is refused.
func (e *Endpoints) publishRFQ(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (RFQ, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return RFQ{}, err
	}
	r, err := getRFQ(ctx, tx, id, true)
	if err != nil {
		return RFQ{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return RFQ{}, err
	}
	if r.Status != Draft {
		return RFQ{}, statusConflict("RFQ", r.Status, "publish", Draft)
	}
	if _, err := e.notPast(ctx, r.LegalEntityID, r.DueDate); err != nil {
		return RFQ{}, err
	}
	if err := e.usable(ctx, r.LegalEntityID, r.supplierIDs(), "suppliers[%d].supplier_id"); err != nil {
		return RFQ{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.rfq SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1 AND status = $4`,
		id, RFQPublished, p.ID, Draft); err != nil {
		return RFQ{}, fmt.Errorf("procurement: publish RFQ %s: %w", id, err)
	}
	return e.recorded(ctx, tx, id, "publish", "published", map[string]any{"from_status": r.Status})
}
