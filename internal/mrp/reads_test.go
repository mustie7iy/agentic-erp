package mrp_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// document writes a material document of the bracket at P1/S1 straight
// into inventory: its movement type, posting date, base quantity (negative
// for what leaves the plant), whether the line is in transit, and the
// document it reverses; it returns its id.
func (e env) document(t *testing.T, number, movementType, day, qty string, inTransit bool, reverses *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	var reference any
	if movementType == "601" || movementType == "602" || movementType == "643" || movementType == "351" {
		reference = "REF-" + number
	}
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) // a no-op once committed
	// The movement's trigger adds to the stock row in the tenant's scope.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", e.Tenant.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
		reversal_of_id, reference, created_by) VALUES ($1, $2, $3, 2026, $4, $5, $6::date, $7, $8, $9)`, e.Tenant.ID, id, e.DE, number, movementType, day,
		reverses, reference, e.Tenant.Human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
		quantity, entry_quantity, entry_uom_code, in_transit) VALUES ($1, $2, 1, 1, $3, $4, $5, $6::numeric, abs($6::numeric), 'EA', $7)`,
		e.Tenant.ID, id, e.bracket, e.p1, e.s1, qty, inTransit); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPlantIssues (S2n; inventory.PlantIssues): the plant's goods issues
// to a cost center, a delivery's and an intercompany one, less their
// reversals, posted from the first day to the last, both included; lines
// in transit, scrapping, transfers and receipts are no issues, and a
// history whose reversals are more is negative.
func TestPlantIssues(t *testing.T) {
	e := setup(t)
	gi := e.document(t, "D1", "201", "2026-08-01", "-10", false, nil)
	e.document(t, "D2", "202", "2026-08-02", "4", false, &gi)
	dl := e.document(t, "D3", "601", "2026-10-30", "-7", false, nil)
	e.document(t, "D4", "602", "2026-10-30", "2", false, &dl)
	e.document(t, "D5", "643", "2026-09-01", "-3", false, nil)
	e.document(t, "D6", "201", "2026-07-31", "-5", false, nil) // the day before the first
	e.document(t, "D7", "201", "2026-10-31", "-5", false, nil) // the day after the last
	e.document(t, "D8", "551", "2026-09-01", "-5", false, nil)
	e.document(t, "D9", "301", "2026-09-01", "-5", false, nil)
	pgtest.Exec(t, `INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
		VALUES ($1, gen_random_uuid(), $2, $3, $4, $5)`, e.Tenant.ID, e.DE, e.bracket, e.p1, e.s1)
	// In transit to P1: a 351, left out by its movement type already, the
	// read's in-transit filter being defensive (no listed type moves stock
	// in transit).
	e.document(t, "D10", "351", "2026-09-01", "5", true, nil)
	from, to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	inv := app.Inventory(app.Config{})
	for _, c := range []struct {
		name     string
		from, to time.Time
		want     string
	}{
		{"the days", from, to, "14"},
		{"reversals only", time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), "-4"},
		{"none", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got map[uuid.UUID]string
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				var err error
				got, err = inv.PlantIssues(ctx, e.p1, []uuid.UUID{uuid.MustParse(e.bracket)}, c.from, c.to)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if g := got[uuid.MustParse(e.bracket)]; g != c.want {
				t.Errorf("got %q, want %q", g, c.want)
			}
		})
	}
}

// TestPlantRequirementsDue (S2n; sales.PlantRequirementsDue): the plant's
// confirmed orders by the day each is due, the day asked for, else the
// order's date; a draft order demands nothing.
func TestPlantRequirementsDue(t *testing.T) {
	e := setup(t)
	order := func(requested string) string {
		r := ""
		if requested != "" {
			r = fmt.Sprintf(`"requested_delivery_date": %q,`, requested)
		}
		o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q, %s
			"items": [{"material_id": %q, "quantity": {"value": "3", "unit": "EA"}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}]}`,
			e.DE, e.customer, e.p1, r, e.bracket, e.revenue), "")
		return o["id"].(string)
	}
	for _, requested := range []string{"", "2030-01-01"} {
		e.Must(200, "POST", "/v1/sales-orders/"+order(requested)+":confirm", "", `"1"`)
	}
	order("2030-02-01") // a draft
	var got []sales.DueRequirement
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		got, err = sales.Orders{}.PlantRequirementsDue(ctx, e.p1, []uuid.UUID{uuid.MustParse(e.bracket)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var today string
	pgtest.Scan(t, `SELECT order_date::text FROM sales.sales_order WHERE tenant_id = $1 AND requested_delivery_date IS NULL`, []any{e.Tenant.ID}, &today)
	want := fmt.Sprintf("[%s 3 EA 2030-01-01 3 EA]", today)
	var g []string
	for _, r := range got {
		g = append(g, fmt.Sprintf("%s %s %s", r.Due.Format(time.DateOnly), r.Quantity, r.Unit))
	}
	if s := fmt.Sprint(g); s != want {
		t.Errorf("got %s, want %s", s, want)
	}
}

// TestProcurementReadsForReview (S2n; procurement.TransportReceipts and
// IntercompanyDue): stock transport orders within the company count only
// in transit to the plant; intercompany orders the plant supplies count
// by their items' delivery dates, else the order's date, while not issued,
// closed or cancelled, their removed items not at all.
func TestProcurementReadsForReview(t *testing.T) {
	e := setup(t)
	for i, status := range []string{"open", "in_transit", "received"} {
		id := uuid.Must(uuid.NewV7())
		issued, received := any(nil), any(nil)
		if status != "open" {
			issued = uuid.Must(uuid.NewV7())
		}
		if status == "received" {
			received = uuid.Must(uuid.NewV7())
		}
		pgtest.Exec(t, `INSERT INTO procurement.stock_transport_order (tenant_id, id, legal_entity_id, fiscal_year, number, supplying_plant_id, receiving_plant_id,
			order_date, status, issue_document_id, issue_document_number, issue_date, receipt_document_id, receipt_document_number, receipt_date, created_by, updated_by)
			VALUES ($1, $2, $3, 2026, $4, gen_random_uuid(), $5, '2026-11-01', $6, $7, CASE WHEN $7::uuid IS NULL THEN NULL ELSE 'MD' END,
				CASE WHEN $7::uuid IS NULL THEN NULL ELSE date '2026-11-01' END, $8, CASE WHEN $8::uuid IS NULL THEN NULL ELSE 'MR' END,
				CASE WHEN $8::uuid IS NULL THEN NULL ELSE date '2026-11-02' END, $9, $9)`,
			e.Tenant.ID, id, e.DE, fmt.Sprintf("ST%d", i), e.p1, status, issued, received, e.Tenant.Human.ID)
		pgtest.Exec(t, `INSERT INTO procurement.stock_transport_order_item (tenant_id, order_id, item_no, material_id, quantity, uom_code, base_quantity,
			supplying_storage_location_id, receiving_storage_location_id) VALUES ($1, $2, 1, $3, $4, 'EA', $4, gen_random_uuid(), $5)`,
			e.Tenant.ID, id, e.bracket, 10+i, e.s1)
	}
	type po struct {
		number, status, delivery string
		issued, removed          bool
	}
	for _, o := range []po{{"IC1", "released", "2026-12-15", false, false}, {"IC2", "draft", "", false, false}, {"IC3", "released", "", true, false},
		{"IC4", "closed", "", false, false}, {"IC5", "cancelled", "", false, false}, {"IC6", "released", "", false, true}} {
		id := uuid.Must(uuid.NewV7())
		var issue, delivery any
		if o.issued {
			issue = uuid.Must(uuid.NewV7())
		}
		if o.delivery != "" {
			delivery = o.delivery
		}
		pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
			payment_term_code, status, supplying_plant_id, issue_document_id, issue_document_number, issue_date, created_by, updated_by)
			VALUES ($1, $2, $3, $4, gen_random_uuid(), 'USD', '2026-11-01', 'NT30', 'draft', $5, $6, CASE WHEN $6::uuid IS NULL THEN NULL ELSE 'MI' END,
				CASE WHEN $6::uuid IS NULL THEN NULL ELSE date '2026-11-02' END, $7, $7)`,
			e.Tenant.ID, id, o.number, e.US, e.p1, issue, e.Tenant.Human.ID)
		pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, material_id, description, quantity, uom_code,
			unit_price, price_unit, account_id, plant_id, storage_location_id, delivery_date, is_removed)
			VALUES ($1, $2, 1, $3, 'Bracket', 2, 'EA', 200, 1, $4, $5, gen_random_uuid(), $6::date, $7)`, e.Tenant.ID, id, e.bracket, e.stock, e.p9, delivery, o.removed)
		for _, s := range map[string][]string{"released": {"released"}, "closed": {"released", "closed"}, "cancelled": {"cancelled"}}[o.status] {
			pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = $2, version = version + 1 WHERE id = $1`, id, s)
		}
	}
	ms := []uuid.UUID{uuid.MustParse(e.bracket)}
	var transit []procurement.OnOrder
	var due []procurement.DueOnOrder
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		if transit, err = (procurement.Orders{}).TransportReceipts(ctx, e.p1, ms); err != nil {
			return err
		}
		due, err = procurement.Orders{}.IntercompanyDue(ctx, e.p1, ms)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(transit); got != fmt.Sprintf("[{%s EA 11}]", e.bracket) {
		t.Errorf("in transit: got %s, want the in-transit order's 11 EA alone", got)
	}
	var g []string
	for _, d := range due {
		g = append(g, d.Due.Format(time.DateOnly)+" "+d.Quantity)
	}
	if got := fmt.Sprint(g); got != "[2026-11-01 2 2026-12-15 2]" {
		t.Errorf("intercompany: got %s, want [2026-11-01 2 2026-12-15 2] (IC2 by its date, IC1 by its item's)", got)
	}
}
