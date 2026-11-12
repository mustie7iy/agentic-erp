package procurement_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// The call-off of a schedule line (ADR-0070 §4, T83b): a draft purchase
// order against the agreement, created once through the order's own path.

// scheduled is an active scheduling agreement of 2026 with two lines:
// line 1, 3 BOX of item B due 2026-08-15, and line 2, 10 EA of item A due
// 2026-07-01. It returns the agreement and its path.
func (e env) scheduled(t *testing.T) (map[string]any, string) {
	t.Helper()
	c, path := e.agreement(t)
	e.Must(201, "POST", path+"/schedule-lines", lineBody(2, "2026-08-15", "3", "BOX"), "")
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-07-01", "10", "EA"), "")
	return c, path
}

// callOff is the path of a line's call-off.
func callOff(path string, no int) string {
	return fmt.Sprintf("%s/schedule-lines/%d:callOff", path, no)
}

func TestCallOffScheduleLine(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	c, path := e.scheduled(t)
	l, tag := e.Must(200, "POST", callOff(path, 1), "", `"1"`)
	if tag != `"2"` {
		t.Errorf("called-off line's ETag: got %s, want \"2\"", tag)
	}
	want(t, "called-off line", l, map[string]any{"line_no": float64(1), "status": "called_off", "version": float64(2), "contract_id": c["id"],
		"purchase_order_document_number": "PO100000"})
	o, _ := e.Must(200, "GET", "/v1/purchase-orders/"+fmt.Sprint(l["purchase_order_id"]), "", "")
	// The order is dated the API's day (setupIn2026), with the supplier's terms.
	want(t, "call-off order", o, map[string]any{"status": "draft", "document_number": "PO100000", "legal_entity_id": c["legal_entity_id"],
		"supplier_id": c["supplier_id"], "currency": "EUR", "contract_id": c["id"], "order_date": "2026-06-15", "requisition_id": nil})
	if got := amountOf(t, o, "total_amount"); got != "45.00 EUR" {
		t.Errorf("call-off order's total: got %s, want 45.00 EUR, the line's value", got)
	}
	item := orderItem(t, o, 0)
	want(t, "call-off order's item", item, map[string]any{"description": "Toner", "account_id": e.office, "contract_item_no": float64(2),
		"delivery_date": "2026-08-15", "price_unit": float64(2)})
	if got := fmt.Sprint(item["quantity"]); got != "map[unit:BOX value:3]" {
		t.Errorf("call-off order's quantity: got %s, want 3 BOX", got)
	}
	if got := amountOf(t, item, "unit_price"); got != "30.00 EUR" {
		t.Errorf("call-off order's price: got %s, want the agreement's, 30.00 EUR", got)
	}
	if got := len(apitest.List(t, o, "items")); got != 1 {
		t.Errorf("call-off order's items: got %d, want 1", got)
	}
	// The schedule shows the outcome; the other line is still to call off.
	page, _ := e.Must(200, "GET", path+"/schedule-lines", "", "")
	var shown []string
	for _, it := range apitest.List(t, page, "items") {
		m := it.(map[string]any)
		shown = append(shown, fmt.Sprintf("%v %v %v", m["line_no"], m["status"], m["purchase_order_document_number"]))
	}
	if got, want := strings.Join(shown, ", "), "2 scheduled <nil>, 1 called_off PO100000"; got != want {
		t.Errorf("schedule after the call-off: got %s, want %s", got, want)
	}
	for query, want := range map[string]string{"status=scheduled": "2", "status=called_off": "1", "status=other": "", "status=called_off&line_no=2": ""} {
		t.Run("listed with "+query, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", path+"/schedule-lines?"+query, "", "")
			if got := listed(t, page, "line_no"); got != want {
				t.Errorf("lines: got %s, want %s", got, want)
			}
		})
	}
	// Once: a second call-off names the order, also at the version the first one read.
	for _, tag := range []string{"*", `"1"`, `"2"`} {
		p := e.Problem(409, "procurement.schedule_line_called_off", "POST", callOff(path, 1), "", tag)
		if got := fmt.Sprint(p["detail"]); !strings.Contains(got, "PO100000") {
			t.Errorf("second call-off's detail with If-Match %s: got %q, want the order PO100000 named", tag, got)
		}
	}
	// The agreement's value is consumed when the order is released, by the order's value.
	before, _ := e.Must(200, "GET", path, "", "")
	if got := amountOf(t, before, "consumed_value"); got != "0.00 EUR" {
		t.Errorf("consumed value after the call-off: got %s, want 0.00 EUR", got)
	}
	released := e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
	want(t, "released call-off order", released, map[string]any{"status": "released"})
	after, _ := e.Must(200, "GET", path, "", "")
	if got := amountOf(t, after, "consumed_value"); got != "45.00 EUR" {
		t.Errorf("consumed value after the order's release: got %s, want 45.00 EUR", got)
	}
	// Received as any order.
	if code, r := e.receipt(t, id(o), "1:3:BOX"); code != 201 {
		t.Errorf("goods receipt of the call-off order: got %d %v, want 201", code, r)
	}
}

// TestCallOffOrderCancelled: a line whose order is cancelled stays called
// off and names that order; it is not reopened.
func TestCallOffOrderCancelled(t *testing.T) {
	e := setupIn2026(t)
	_, path := e.scheduled(t)
	l, _ := e.Must(200, "POST", callOff(path, 1), "", "*")
	order := "/v1/purchase-orders/" + fmt.Sprint(l["purchase_order_id"])
	cancelled := e.act(t, order, "cancel", "")
	want(t, "cancelled call-off order", cancelled, map[string]any{"status": "cancelled"})
	page, _ := e.Must(200, "GET", path+"/schedule-lines?status=called_off", "", "")
	if got, want := listed(t, page, "purchase_order_id"), fmt.Sprint(l["purchase_order_id"]); got != want {
		t.Errorf("orders of the lines called off: got %s, want the cancelled order %s", got, want)
	}
	e.Problem(409, "procurement.schedule_line_called_off", "POST", callOff(path, 1), "", "*")
}

// TestCallOffRecorded: a call-off is audited once on the line, beside the
// order's own record, and published as ScheduleLineCalledOff and
// ScheduleLineChanged, naming the supplier by id and number, never by
// name.
func TestCallOffRecorded(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	c, path := e.scheduled(t)
	l, _ := e.Must(200, "POST", callOff(path, 1), "", "*")
	line := map[string]any{"schedule_line_id": l["id"], "contract_id": c["id"], "document_number": c["document_number"],
		"legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "supplier_id": e.Supplier.String(), "supplier_number": "BP90000001",
		"line_no": float64(1), "contract_item_no": float64(2), "material_key": "B", "delivery_date": "2026-08-15",
		"purchase_order_id": l["purchase_order_id"], "purchase_order_document_number": "PO100000"}
	called := events(t, e, "schedule_line_called_off")
	if len(called) != 1 {
		t.Fatalf("schedule_line_called_off events: got %d, want 1", len(called))
	}
	want(t, "ScheduleLineCalledOff", called[0], line)
	want(t, "ScheduleLineCalledOff", called[0], map[string]any{"order_date": "2026-06-15"})
	if got := amountOf(t, called[0], "amount"); got != "45.00 EUR" {
		t.Errorf("ScheduleLineCalledOff amount: got %s, want 45.00 EUR", got)
	}
	if got := fmt.Sprint(called[0]["quantity"]); got != "map[unit:BOX value:3]" {
		t.Errorf("ScheduleLineCalledOff quantity: got %s, want 3 BOX", got)
	}
	changed := events(t, e, "schedule_line_changed")
	if len(changed) != 3 {
		t.Fatalf("schedule_line_changed events: got %d, want 3 (two lines entered, one called off)", len(changed))
	}
	want(t, "ScheduleLineChanged of the call-off", changed[2], line)
	want(t, "ScheduleLineChanged of the call-off", changed[2], map[string]any{"status": "called_off", "change": "called_off"})
	want(t, "ScheduleLineChanged of the line entered", changed[0], map[string]any{"status": "scheduled", "change": "created", "purchase_order_id": nil})
	apitest.NeverPublished(t, e.Tenant, "procurement", tyrell)
	if n := audits(t, e, "procurement.schedule_line.call_off", id(l)); n != 1 {
		t.Errorf("procurement.schedule_line.call_off audit records: got %d, want 1", n)
	}
	if n := audits(t, e, "procurement.purchase_order.create", fmt.Sprint(l["purchase_order_id"])); n != 1 {
		t.Errorf("procurement.purchase_order.create audit records of the call-off order: got %d, want 1", n)
	}
}

func TestCallOffRefusals(t *testing.T) {
	e := setupIn2026(t)
	_, path := e.scheduled(t)
	_, plainPath := e.activeContract(t)
	terminated, terminatedPath := e.scheduled(t)
	e.Must(200, "POST", terminatedPath+":terminate", `{"reason": "Supplier closed"}`, fmt.Sprintf(`"%v"`, terminated["version"]))
	for name, c := range map[string]struct {
		path, tag string
		status    int
		code      string
	}{
		"a contract of the kind contract": {callOff(plainPath, 1), "*", 409, "procurement.not_a_scheduling_agreement"},
		"a terminated agreement":          {callOff(terminatedPath, 1), "*", 409, "procurement.invalid_status"},
		"a line the agreement lacks":      {callOff(path, 3), "*", 404, "request.not_found"},
		"line number zero":                {callOff(path, 0), "*", 400, "request.invalid_parameter"},
		"another version of the line":     {callOff(path, 1), `"7"`, 412, "request.version_mismatch"},
		"no version":                      {callOff(path, 1), "", 428, "request.precondition_required"},
		"no contract with the id":         {callOff("/v1/contracts/"+uuid.NewString(), 1), "*", 404, "request.not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(c.status, c.code, "POST", c.path, "", c.tag)
		})
	}
	t.Run("a caller of another company", func(t *testing.T) {
		e.As(e.Reader(t, "Reader of 1000", e.US)).In(t).Problem(404, "request.not_found", "POST", callOff(path, 1), "", "*")
	})
	t.Run("the order's own checks", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
		defer pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = false, version = version + 1 WHERE partner_id = $1`, e.Supplier)
		p := e.in(t).Problem(422, "procurement.supplier_not_usable", "POST", callOff(path, 1), "", "*")
		wantField(t, p, "supplier_id", "invalid")
	})
	t.Run("without the permission to create orders", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.order.create'`, e.Tenant.ID)
		e.in(t).Problem(403, "identity.permission_missing", "POST", callOff(path, 1), "", "*")
	})
	if n := len(events(t, e, "schedule_line_called_off")); n != 0 {
		t.Errorf("schedule_line_called_off events after the refusals: got %d, want 0", n)
	}
	list, _ := e.Must(200, "GET", "/v1/purchase-orders", "", "")
	if got := listed(t, list, "id"); got != "" {
		t.Errorf("purchase orders after the refusals: got %s, want none", got)
	}
}

// TestCallOffNeedsLevelTwo: an agent at autonomy level 1, which drafts
// orders, calls no line off: a call-off reserves the line (ADR-0036).
func TestCallOffNeedsLevelTwo(t *testing.T) {
	e := setupIn2026(t)
	_, path := e.scheduled(t)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	agent := e.As(e.Tenant.Agent)
	pgtest.Exec(t, `UPDATE identity.agent_profile SET autonomy_level = 1, version = version + 1 WHERE tenant_id = $1 AND principal_id = $2`, e.Tenant.ID, e.Tenant.Agent.ID)
	agent.Problem(403, "identity.autonomy_insufficient", "POST", callOff(path, 1), "", "*")
	pgtest.Exec(t, `UPDATE identity.agent_profile SET autonomy_level = 2, version = version + 1 WHERE tenant_id = $1 AND principal_id = $2`, e.Tenant.ID, e.Tenant.Agent.ID)
	l, _ := agent.Must(200, "POST", callOff(path, 1), "", "*")
	want(t, "line called off by an agent at level 2", l, map[string]any{"status": "called_off"})
}

// TestCallOffValidity: an agreement is called off only on a day of its
// validity, the legal entity's date in its company's time zone.
func TestCallOffValidity(t *testing.T) {
	e := setup(t)
	_, path := e.scheduled(t)
	on := func(day time.Time) env {
		at := e
		at.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return day } })
		return at
	}
	for name, c := range map[string]struct {
		now  time.Time
		want string
	}{
		"the day before it starts": {time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC), "the legal entity's date today is 2025-12-31"},
		"the day after it ends":    {time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), "the legal entity's date today is 2027-01-01"},
	} {
		t.Run(name, func(t *testing.T) {
			p := on(c.now).in(t).Problem(409, "procurement.agreement_not_valid", "POST", callOff(path, 1), "", "*")
			if got := fmt.Sprint(p["detail"]); !strings.Contains(got, c.want) || !strings.Contains(got, "valid from 2026-01-01 to 2026-12-31") {
				t.Errorf("detail: got %q, want the validity and %q", got, c.want)
			}
		})
	}
	t.Run("its last day", func(t *testing.T) {
		l, _ := on(time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC)).in(t).Must(200, "POST", callOff(path, 1), "", "*")
		o, _ := e.in(t).Must(200, "GET", "/v1/purchase-orders/"+fmt.Sprint(l["purchase_order_id"]), "", "")
		want(t, "order called off on the last day", o, map[string]any{"order_date": "2026-12-31"})
	})
}

// TestCallOffExceedingTheAgreement: an order above what remains of the
// agreement's target value is refused at its release, as for any
// contract; the call-off itself is not.
func TestCallOffExceedingTheAgreement(t *testing.T) {
	e := setupIn2026(t)
	_, path := e.agreement(t) // target value 1000.00
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-07-01", "150", "EA"), "")
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2026-08-01", "60", "EA"), "")
	first, _ := e.Must(200, "POST", callOff(path, 1), "", "*")
	second, _ := e.Must(200, "POST", callOff(path, 2), "", "*")
	e.act(t, "/v1/purchase-orders/"+fmt.Sprint(first["purchase_order_id"]), "release", "")
	e.Problem(409, "procurement.contract_exceeded", "POST", "/v1/purchase-orders/"+fmt.Sprint(second["purchase_order_id"])+":release", "", "*")
	c, _ := e.Must(200, "GET", path, "", "")
	if got := amountOf(t, c, "consumed_value"); got != "750.00 EUR" {
		t.Errorf("consumed value: got %s, want 750.00 EUR, the first order's", got)
	}
}

// TestCallOffReplayed: the same call-off with its Idempotency-Key again
// answers the same line and order and creates nothing more. The key is
// kept by the wall clock, so the test runs on it, with an agreement valid
// on every day a run reaches.
func TestCallOffReplayed(t *testing.T) {
	e := setup(t)
	c, _ := e.Must(201, "POST", "/v1/contracts", fmt.Sprintf(`{"kind": "scheduling_agreement", "legal_entity_id": %q, "supplier_id": %q, "currency": "EUR",
		"valid_from": "2026-01-01", "valid_to": "2999-12-31", "target_value": {"amount": "1000.00", "currency": "EUR"}, "items": [
		{"material_key": "A", "description": "Paper", "unit_price": {"amount": "5.00", "currency": "EUR"}, "uom_code": "EA", "account_id": %q}]}`,
		e.DE, e.Supplier, e.office), "")
	path := "/v1/contracts/" + id(c)
	e.act(t, path, "release", "")
	e.Must(201, "POST", path+"/schedule-lines", lineBody(1, "2999-07-01", "10", "EA"), "")
	req := apitest.Req{Method: http.MethodPost, Path: callOff(path, 1), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
	first, again := e.API.Do(req), e.API.Do(req)
	if got := fmt.Sprintf("%d %d", first.Code, again.Code); got != "200 200" {
		t.Fatalf("statuses: got %s, want 200 200 (%s)", got, again.Body)
	}
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", again.Header().Get("Idempotent-Replayed"))
	}
	if got, want := apitest.JSON(t, again), apitest.JSON(t, first); !reflect.DeepEqual(got, want) {
		t.Errorf("replayed body: got %v, want %v", got, want)
	}
	list, _ := e.Must(200, "GET", "/v1/purchase-orders", "", "")
	if got, want := listed(t, list, "id"), fmt.Sprint(apitest.JSON(t, first)["purchase_order_id"]); got != want {
		t.Errorf("purchase orders: got %s, want the call-off's alone, %s", got, want)
	}
}

// calling calls line no of the contract off through the handler, at the
// version tag.
func calling(h *procurement.Endpoints, contract uuid.UUID, no int32, tag string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := h.CallOffScheduleLine(ctx, gen.CallOffScheduleLineParams{ID: contract, LineNo: no, IfMatch: tag})
		return err
	}
}

// TestCallOffOnce: two call-offs of one line at once, both at the version
// their callers read, serialize on the contract's row lock; the second
// finds the line called off and is told so, not that the line changed,
// and one order exists.
func TestCallOffOnce(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	c, path := e.scheduled(t)
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, calling(h, uuidOf(c), 1, `"1"`), calling(h, uuidOf(c), 1, `"1"`))
	if first != nil {
		t.Fatalf("first call-off: %v", first)
	}
	if got := problem.From(second).Code; got != "procurement.schedule_line_called_off" {
		t.Errorf("second call-off: got %s (%v), want procurement.schedule_line_called_off", got, second)
	}
	list, _ := e.Must(200, "GET", "/v1/purchase-orders", "", "")
	if got := apitest.List(t, list, "items"); len(got) != 1 {
		t.Errorf("purchase orders: got %d, want 1", len(got))
	}
	if n := len(events(t, e, "schedule_line_called_off")); n != 1 {
		t.Errorf("schedule_line_called_off events: got %d, want 1", n)
	}
	page, _ := e.Must(200, "GET", path+"/schedule-lines?status=called_off", "", "")
	if got := listed(t, page, "line_no"); got != "1" {
		t.Errorf("lines called off: got %s, want 1", got)
	}
}

// TestCallOffDatabaseFailures calls the call-off outside a unit of work
// and with each of its database statements failing in turn.
func TestCallOffDatabaseFailures(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	c, _ := e.scheduled(t)
	call := calling(h, uuidOf(c), 1, "*")
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "call off a schedule line", call)
}
