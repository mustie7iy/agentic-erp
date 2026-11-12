package procurement_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Scheduling agreements (ADR-0070, T83a): a contract of its own kind
// whose delivery schedule is entered once it is active.

const agreementKind = `, "kind": "scheduling_agreement"`

// agreement is an active scheduling agreement of 2026 with items A (5.00
// EUR per EA) and B (30.00 EUR per 2 BOX), and its path.
func (e env) agreement(t *testing.T) (map[string]any, string) {
	t.Helper()
	c := e.contract(t, e.Supplier.String(), agreementKind)
	path := "/v1/contracts/" + id(c)
	return e.act(t, path, "release", ""), path
}

// lineBody is a schedule line's request.
func lineBody(item int, date, qty, unit string) string {
	return fmt.Sprintf(`{"contract_item_no": %d, "delivery_date": %q, "quantity": {"value": %q, "unit": %q}}`, item, date, qty, unit)
}

// lines are a schedule page's lines as "line_no date quantity unit
// amount", in the page's order.
func lines(t *testing.T, page map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range apitest.List(t, page, "items") {
		l := it.(map[string]any)
		q := l["quantity"].(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v %v %s", l["line_no"], l["delivery_date"], q["value"], q["unit"], amountOf(t, l, "amount")))
	}
	return strings.Join(out, ", ")
}

func TestSchedulingAgreement(t *testing.T) {
	e := setupIn2026(t)
	draft := e.contract(t, e.Supplier.String(), agreementKind)
	path := "/v1/contracts/" + id(draft)
	want(t, "created agreement", draft, map[string]any{"kind": "scheduling_agreement", "document_number": "CT100000", "status": "draft"})
	e.Problem(409, "procurement.invalid_status", "POST", path+"/schedule-lines", lineBody(1, "2026-07-01", "10", "EA"), "")
	// Changed and released as any contract; the kind stays.
	changed, _ := e.Must(200, "PATCH", path, `{"valid_to": "2026-11-30"}`, `"1"`)
	want(t, "changed agreement", changed, map[string]any{"kind": "scheduling_agreement", "valid_to": "2026-11-30"})
	c := e.act(t, path, "release", "")
	want(t, "released agreement", c, map[string]any{"kind": "scheduling_agreement", "status": "active"})

	first := e.API.Do(apitest.Req{Method: http.MethodPost, Path: path + "/schedule-lines", Body: lineBody(2, "2026-08-15", "3", "BOX"),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()})
	if first.Code != 201 {
		t.Fatalf("first line: got %d %s, want 201", first.Code, first.Body)
	}
	if got, want := first.Header().Get("Location"), path+"/schedule-lines?line_no=1"; got != want {
		t.Errorf("first line's Location: got %s, want %s", got, want)
	}
	if got := first.Header().Get("ETag"); got != `"1"` {
		t.Errorf("first line's ETag: got %s, want \"1\"", got)
	}
	l := apitest.JSON(t, first)
	want(t, "first line", l, map[string]any{"contract_id": c["id"], "contract_document_number": "CT100000", "line_no": float64(1),
		"contract_item_no": float64(2), "material_key": "B", "description": "Toner", "delivery_date": "2026-08-15", "price_unit": float64(2),
		"version": float64(1)})
	if got := amountOf(t, l, "unit_price"); got != "30.00 EUR" {
		t.Errorf("first line's unit price: got %s, want 30.00 EUR", got)
	}
	if got := amountOf(t, l, "amount"); got != "45.00 EUR" {
		t.Errorf("first line's amount: got %s, want 45.00 EUR (3 BOX at 30.00 per 2)", got)
	}
	located, _ := e.Must(200, "GET", first.Header().Get("Location"), "", "")
	if got := listed(t, located, "id"); got != id(l) {
		t.Errorf("lines at the first line's Location: got %s, want %s alone", got, id(l))
	}
	// An earlier date entered later, and a value rounded half away from zero: 0.333 BOX at 30.00 per 2 is 4.995.
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-07-01", "2.5", "EA"), "")
	e.Must(201, "POST", path+"/schedule-lines", lineBody(2, "2026-07-01", "0.333", "BOX"), "")
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-11-30", "1", "EA"), "")

	page, _ := e.Must(200, "GET", path+"/schedule-lines", "", "")
	if got, want := lines(t, page), "2 2026-07-01 2.5 EA 12.50 EUR, 3 2026-07-01 0.333 BOX 5.00 EUR, 1 2026-08-15 3 BOX 45.00 EUR, 4 2026-11-30 1 EA 5.00 EUR"; got != want {
		t.Errorf("schedule by date:\n got %s\nwant %s", got, want)
	}
	for query, want := range map[string]string{
		"contract_item_no=1":                               "2, 4",
		"contract_item_no=2":                               "3, 1",
		"line_no=3":                                        "3",
		"contract_item_no=1&line_no=3":                     "",
		"order_by=delivery_date%20desc":                    "4, 1, 3, 2",
		"contract_item_no=2&order_by=delivery_date":        "3, 1",
		"contract_item_no=9":                               "",
		"line_no=2&order_by=delivery_date%20desc":          "2",
		"contract_item_no=1&order_by=delivery_date%20desc": "4, 2",
	} {
		t.Run("listed with "+query, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", path+"/schedule-lines?"+query, "", "")
			if got := listed(t, page, "line_no"); got != want {
				t.Errorf("lines: got %s, want %s", got, want)
			}
		})
	}
	t.Run("paged", func(t *testing.T) {
		e := e.in(t)
		var seen []string
		next := path + "/schedule-lines?page_size=3"
		for range 2 {
			page, _ := e.Must(200, "GET", next, "", "")
			seen = append(seen, listed(t, page, "line_no"))
			next = path + "/schedule-lines?page_size=3&page_token=" + fmt.Sprint(page["next_page_token"])
		}
		if got, want := strings.Join(seen, " | "), "2, 3, 1 | 4"; got != want {
			t.Errorf("pages of three: got %s, want %s", got, want)
		}
		e.Problem(400, "request.invalid_order_by", "GET", path+"/schedule-lines?order_by=line_no", "", "")
	})
	// Entering lines leaves the contract as it was.
	after, _ := e.Must(200, "GET", path, "", "")
	want(t, "agreement after its schedule", after, map[string]any{"version": c["version"], "status": "active"})
	if got := amountOf(t, after, "consumed_value"); got != "0.00 EUR" {
		t.Errorf("consumed value after the schedule: got %s, want 0.00 EUR", got)
	}
}

// TestScheduleLineRecorded: a line entered is audited once and published
// as ScheduleLineChanged, naming the company by its code and the supplier
// by id and number, never by name.
func TestScheduleLineRecorded(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	c, path := e.agreement(t)
	l, _ := e.Must(201, "POST", path+"/schedule-lines", lineBody(2, "2026-08-15", "3", "BOX"), "")
	published := events(t, e, "schedule_line_changed")
	if len(published) != 1 {
		t.Fatalf("schedule_line_changed events: got %d, want 1", len(published))
	}
	want(t, "ScheduleLineChanged", published[0], map[string]any{"schedule_line_id": l["id"], "contract_id": c["id"],
		"document_number": c["document_number"], "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "supplier_id": e.Supplier.String(),
		"supplier_number": "BP90000001", "line_no": float64(1), "contract_item_no": float64(2), "material_key": "B", "delivery_date": "2026-08-15",
		"change": "created"})
	if got := fmt.Sprint(published[0]["quantity"]); got != "map[unit:BOX value:3]" {
		t.Errorf("ScheduleLineChanged quantity: got %s, want 3 BOX", got)
	}
	if got := amountOf(t, published[0], "amount"); got != "45.00 EUR" {
		t.Errorf("ScheduleLineChanged amount: got %s, want 45.00 EUR", got)
	}
	apitest.NeverPublished(t, e.Tenant, "procurement", tyrell)
	if n := audits(t, e, "procurement.schedule_line.create", id(l)); n != 1 {
		t.Errorf("procurement.schedule_line.create audit records: got %d, want 1", n)
	}
}

func TestScheduleLineRefusals(t *testing.T) {
	e := setup(t)
	_, path := e.agreement(t)
	plain, plainPath := e.activeContract(t)
	terminated, terminatedPath := e.agreement(t)
	e.Must(200, "POST", terminatedPath+":terminate", `{"reason": "Supplier closed"}`, fmt.Sprintf(`"%v"`, terminated["version"]))
	// An agreement whose item B costs the largest amount there is.
	big, _ := e.Must(201, "POST", "/v1/contracts", fmt.Sprintf(`{"kind": "scheduling_agreement", "legal_entity_id": %q, "supplier_id": %q, "currency": "EUR",
		"valid_from": "2026-01-01", "valid_to": "2026-12-31", "target_value": {"amount": "1.00", "currency": "EUR"}, "items": [
		{"material_key": "B", "description": "Plant", "unit_price": {"amount": %q, "currency": "EUR"}, "uom_code": "EA", "account_id": %q}]}`,
		e.DE, e.Supplier, largest, e.office), "")
	bigPath := "/v1/contracts/" + id(big)
	e.act(t, bigPath, "release", "")
	ok := lineBody(1, "2026-07-01", "10", "EA")
	for name, c := range map[string]struct {
		path, body string
		status     int
		code       string
		field      string
	}{
		"a contract of the kind contract": {plainPath, ok, 409, "procurement.not_a_scheduling_agreement", ""},
		"a terminated agreement":          {terminatedPath, ok, 409, "procurement.invalid_status", ""},
		"an item the agreement lacks":     {path, lineBody(3, "2026-07-01", "10", "EA"), 422, "procurement.invalid_contract_item", "contract_item_no"},
		"another unit than the item's":    {path, lineBody(1, "2026-07-01", "10", "BOX"), 422, "procurement.invalid_contract_item", "contract_item_no"},
		"a quantity of zero":              {path, lineBody(1, "2026-07-01", "0", "EA"), 422, "procurement.invalid_quantity", "quantity.value"},
		"a negative quantity":             {path, lineBody(1, "2026-07-01", "-1", "EA"), 422, "procurement.invalid_quantity", "quantity.value"},
		"the day before the validity":     {path, lineBody(1, "2025-12-31", "10", "EA"), 422, "procurement.schedule_line_outside_validity", "delivery_date"},
		"the day after the validity":      {path, lineBody(1, "2027-01-01", "10", "EA"), 422, "procurement.schedule_line_outside_validity", "delivery_date"},
		"a value out of range":            {bigPath, lineBody(1, "2026-07-01", "2", "EA"), 422, "procurement.amount_too_large", "quantity.value"},
		"seven decimals":                  {path, lineBody(1, "2026-07-01", "1.0000001", "EA"), 400, "request.invalid_body", ""},
		"no quantity":                     {path, `{"contract_item_no": 1, "delivery_date": "2026-07-01"}`, 400, "request.invalid_body", ""},
		"no contract with the id":         {"/v1/contracts/" + uuid.NewString(), ok, 404, "request.not_found", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(c.status, c.code, "POST", c.path+"/schedule-lines", c.body, "")
			if got := errorFields(p); got != c.field {
				t.Errorf("field errors: got %q (%v), want %q", got, p["errors"], c.field)
			}
		})
	}
	t.Run("the validity named", func(t *testing.T) {
		p := e.in(t).Problem(422, "procurement.schedule_line_outside_validity", "POST", path+"/schedule-lines", lineBody(1, "2027-01-01", "10", "EA"), "")
		if got := fmt.Sprint(p["detail"]); !strings.Contains(got, "valid from 2026-01-01 to 2026-12-31") {
			t.Errorf("detail: got %q, want the validity 2026-01-01 to 2026-12-31 named", got)
		}
	})
	t.Run("the first and the last day of the validity", func(t *testing.T) {
		e := e.in(t)
		e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-01-01", "10", "EA"), "")
		e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-12-31", "10", "EA"), "")
	})
	t.Run("a contract of the kind contract lists none", func(t *testing.T) {
		page, _ := e.in(t).Must(200, "GET", plainPath+"/schedule-lines", "", "")
		if got := listed(t, page, "line_no"); got != "" {
			t.Errorf("schedule of contract %v: got lines %s, want none", plain["document_number"], got)
		}
	})
	t.Run("a caller of another company", func(t *testing.T) {
		other := e.As(e.Reader(t, "Reader of 1000", e.US)).In(t)
		other.Problem(404, "request.not_found", "POST", path+"/schedule-lines", ok, "")
		other.Problem(404, "request.not_found", "GET", path+"/schedule-lines", "", "")
	})
	t.Run("a company no longer active", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'inactive', version = version + 1 WHERE id = $1`, e.DE)
		defer pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'active', version = version + 1 WHERE id = $1`, e.DE)
		e.in(t).Problem(422, "procurement.legal_entity_not_active", "POST", path+"/schedule-lines", ok, "")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.contract.manage'`, e.Tenant.ID)
		e.in(t).Problem(403, "identity.permission_missing", "POST", path+"/schedule-lines", ok, "")
		e.in(t).Must(200, "GET", path+"/schedule-lines", "", "")
	})
	if n := len(events(t, e, "schedule_line_changed")); n != 2 {
		t.Errorf("schedule_line_changed events after the refusals: got %d, want 2", n)
	}
}

// TestContractKinds: a contract is of the kind given, a contract when
// none is; the list finds each kind; a renewal has its contract's kind
// and an empty schedule.
func TestContractKinds(t *testing.T) {
	e := setup(t)
	plain, _ := e.activeContract(t)
	want(t, "contract created without a kind", plain, map[string]any{"kind": "contract"})
	named := e.contract(t, e.Supplier.String(), `, "kind": "contract"`)
	want(t, "contract created as a contract", named, map[string]any{"kind": "contract"})
	c, path := e.agreement(t)
	p := e.Problem(422, "procurement.invalid_kind", "POST", "/v1/contracts", fmt.Sprintf(`{"kind": "blanket", "legal_entity_id": %q, "supplier_id": %q,
		"currency": "EUR", "valid_from": "2026-01-01", "valid_to": "2026-12-31", "target_value": {"amount": "1.00", "currency": "EUR"}, "items": [
		{"material_key": "A", "description": "Paper", "unit_price": {"amount": "5.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q}]}`,
		e.DE, e.Supplier, e.office), "")
	wantField(t, p, "kind", "invalid")
	for query, want := range map[string]string{
		"kind=scheduling_agreement":               id(c),
		"kind=contract&order_by=document_number":  id(plain) + ", " + id(named),
		"kind=blanket":                            "",
		"kind=scheduling_agreement&status=draft":  "",
		"kind=scheduling_agreement&status=active": id(c),
	} {
		t.Run("listed with "+query, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", "/v1/contracts?"+query, "", "")
			if got := listed(t, page, "id"); got != want {
				t.Errorf("contracts: got %s, want %s", got, want)
			}
		})
	}
	all, _ := e.Must(200, "GET", "/v1/contracts?order_by=document_number", "", "")
	if got, want := listed(t, all, "kind"), "contract, contract, scheduling_agreement"; got != want {
		t.Errorf("kinds of every contract: got %s, want %s", got, want)
	}
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-07-01", "10", "EA"), "")
	r, _ := e.Must(201, "POST", path+":renew", renewal, "*")
	want(t, "renewal of an agreement", r, map[string]any{"kind": "scheduling_agreement", "renewal_of_id": c["id"], "status": "draft"})
	page, _ := e.Must(200, "GET", "/v1/contracts/"+id(r)+"/schedule-lines", "", "")
	if got := listed(t, page, "line_no"); got != "" {
		t.Errorf("schedule of the renewal: got lines %s, want none", got)
	}
	pr, _ := e.Must(201, "POST", "/v1/contracts/"+id(plain)+":renew", renewal, "*")
	want(t, "renewal of a contract", pr, map[string]any{"kind": "contract"})
}

// TestScheduleLineReplayed: the same line with its Idempotency-Key again
// answers the same line and enters nothing more.
func TestScheduleLineReplayed(t *testing.T) {
	e := setup(t)
	_, path := e.agreement(t)
	req := apitest.Req{Method: http.MethodPost, Path: path + "/schedule-lines", Body: lineBody(1, "2026-07-01", "10", "EA"), Tenant: e.Tenant.ID,
		As: e.Tenant.Human, Key: apitest.NewKey()}
	first, again := e.API.Do(req), e.API.Do(req)
	if got := fmt.Sprintf("%d %d", first.Code, again.Code); got != "201 201" {
		t.Fatalf("statuses: got %s, want 201 201", got)
	}
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", again.Header().Get("Idempotent-Replayed"))
	}
	if got, want := apitest.JSON(t, again), apitest.JSON(t, first); !reflect.DeepEqual(got, want) {
		t.Errorf("replayed body: got %v, want %v", got, want)
	}
	page, _ := e.Must(200, "GET", path+"/schedule-lines", "", "")
	if got := listed(t, page, "line_no"); got != "1" {
		t.Errorf("lines after the replay: got %s, want 1 alone", got)
	}
}

// scheduleLine enters a line of the contract through the handler.
func scheduleLine(h interface {
	CreateScheduleLine(context.Context, *gen.ScheduleLineCreate, gen.CreateScheduleLineParams) (gen.CreateScheduleLineRes, error)
}, contract uuid.UUID, date string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := h.CreateScheduleLine(ctx, &gen.ScheduleLineCreate{ContractItemNo: 1, DeliveryDate: dateOf(date), Quantity: gen.Quantity{Value: "10", Unit: "EA"}},
			gen.CreateScheduleLineParams{ID: contract})
		return err
	}
}

// TestScheduleLinesNumberedInTurn: two lines of one agreement entered at
// once serialize on the contract's row lock and are numbered 1 and 2.
func TestScheduleLinesNumberedInTurn(t *testing.T) {
	e := setup(t)
	h := endpoints()
	c, path := e.agreement(t)
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, scheduleLine(h, uuidOf(c), "2026-09-01"), scheduleLine(h, uuidOf(c), "2026-08-01"))
	if first != nil {
		t.Fatalf("first line: got %v, want nil", first)
	}
	if second != nil {
		t.Fatalf("second line: got %v, want nil", second)
	}
	page, _ := e.Must(200, "GET", path+"/schedule-lines", "", "")
	if got, want := lines(t, page), "2 2026-08-01 10 EA 50.00 EUR, 1 2026-09-01 10 EA 50.00 EUR"; got != want {
		t.Errorf("schedule: got %s, want %s", got, want)
	}
}

// TestScheduleDatabaseFailures calls the schedule's handlers outside a
// unit of work and with each of their database statements failing in
// turn.
func TestScheduleDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	c, path := e.agreement(t)
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-07-01", "10", "EA"), "")
	for name, call := range map[string]func(ctx context.Context) error{
		"enter a schedule line": scheduleLine(h, uuidOf(c), "2026-08-01"),
		"list schedule lines": func(ctx context.Context) error {
			_, err := h.ListScheduleLines(ctx, gen.ListScheduleLinesParams{ID: uuidOf(c)})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}
