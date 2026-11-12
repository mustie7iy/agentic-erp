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
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The award of an RFQ (T82d, ADR-0069 §7). The RFQ of these tests asks
// for 40 chairs and 12.5 desks in EUR; the tests' clock is 15 June 2026.

// awardBody awards to the quotation with a contract for 2026-07-01 to
// 2027-06-30.
func awardBody(quotation string) string {
	return fmt.Sprintf(`{"quotation_id": %q, "valid_from": "2026-07-01", "valid_to": "2027-06-30"}`, quotation)
}

// quoted is a published RFQ of the fixture's two suppliers with a
// complete quotation of each: Tyrell's (BP90000001) at 20,666.67 and
// Cabot's (BP90000002), the cheaper, at 19,125.00. It returns the RFQ's
// path and the two quotations' ids.
func (e env) quoted(t *testing.T) (path, tyrells, cabots string) {
	t.Helper()
	path = "/v1/rfqs/" + e.publishedRFQ(t, "2026-07-31")
	a, _ := e.Must(201, "POST", path+"/quotations", quotationBody(e.Supplier, "Q-457", price(1, "412.50", "EUR", ""), price(2, "1000.00", "EUR", `, "price_unit": 3`)), "")
	b, _ := e.Must(201, "POST", path+"/quotations", quotationBody(e.Vendor, "ANG-9", price(1, "400.00", "EUR", ""), price(2, "250.00", "EUR", "")), "")
	return path, id(a), id(b)
}

// outcomes are an RFQ's quotations as "reference status version", as
// recorded.
func (e env) outcomes(t *testing.T, path string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", path+"/quotations", "", "")
	var out []string
	for _, it := range apitest.List(t, page, "items") {
		m := it.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v", m["supplier_reference"], m["status"], m["version"]))
	}
	return strings.Join(out, ", ")
}

// TestAwardRFQ: the RFQ is awarded to Tyrell's quotation, which is not
// the lowest: a draft contract with Tyrell carries the RFQ's items at the
// quoted prices and the quotation's total, the quotations show the
// outcome, and the contract is released as any other.
func TestAwardRFQ(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	path, tyrells, _ := e.quoted(t)
	_, tag := e.Must(200, "GET", path, "", "")
	r, awardedTag := e.Must(200, "POST", path+":award", awardBody(tyrells), tag)
	contract := "/v1/contracts/" + fmt.Sprint(r["contract_id"])
	t.Run("the RFQ awarded", func(t *testing.T) {
		for field, want := range map[string]any{"status": "awarded", "version": float64(3), "contract_document_number": "CT100000"} {
			if r[field] != want {
				t.Errorf("%s: got %v, want %v", field, r[field], want)
			}
		}
		if awardedTag != `"3"` {
			t.Errorf("etag: got %s, want \"3\"", awardedTag)
		}
		got, _ := e.in(t).Must(200, "GET", path, "", "")
		if got["contract_id"] != r["contract_id"] {
			t.Errorf("read contract_id: got %v, want %v", got["contract_id"], r["contract_id"])
		}
		if got["status"] != "awarded" {
			t.Errorf("read status: got %v, want awarded", got["status"])
		}
	})
	t.Run("the contract", func(t *testing.T) {
		c, _ := e.in(t).Must(200, "GET", contract, "", "")
		for field, want := range map[string]any{"status": "draft", "document_number": "CT100000", "legal_entity_id": e.DE.String(),
			"supplier_id": e.Supplier.String(), "currency": "EUR", "valid_from": "2026-07-01", "valid_to": "2027-06-30", "rfq_id": id(r),
			"rfq_document_number": "RQ100000"} {
			if c[field] != want {
				t.Errorf("%s: got %v, want %v", field, c[field], want)
			}
		}
		if got := amountOf(t, c, "target_value"); got != "20666.67 EUR" {
			t.Errorf("target_value: got %s, want 20666.67 EUR", got)
		}
		var lines []string
		for _, it := range apitest.List(t, c, "items") {
			m := it.(map[string]any)
			lines = append(lines, fmt.Sprintf("%v %v %v %v %s per %v on %v", m["item_no"], m["material_key"], m["description"], m["uom_code"],
				amountOf(t, m, "unit_price"), m["price_unit"], m["account_id"]))
		}
		want := fmt.Sprintf("1 CHAIR Item CHAIR EA 412.50 EUR per 1 on %s; 2 DESK Item DESK EA 1000.00 EUR per 3 on %s", e.office, e.office)
		if got := strings.Join(lines, "; "); got != want {
			t.Errorf("items: got %s, want %s", got, want)
		}
	})
	t.Run("the quotations' outcomes", func(t *testing.T) {
		if got, want := e.in(t).outcomes(t, path), "Q-457 awarded 2, ANG-9 not_awarded 2"; got != want {
			t.Errorf("quotations: got %s, want %s", got, want)
		}
		c, _ := e.in(t).Must(200, "GET", path+"/comparison", "", "")
		rows := comparedRows(t, c)
		if len(rows) != 2 || !strings.HasPrefix(rows[0], "BP90000002 rank 1 ANG-9 not_awarded") || !strings.HasPrefix(rows[1], "BP90000001 rank 2 Q-457 awarded") {
			t.Errorf("comparison after the award: got %v, want ANG-9 not_awarded first, then Q-457 awarded", rows)
		}
	})
	t.Run("events and audit", func(t *testing.T) {
		awarded := apitest.CompanyNamed(t, events(t, e, "rfq_awarded"))
		want := map[string]any{"rfq_id": id(r), "document_number": "RQ100000", "legal_entity_id": e.DE.String(), "legal_entity_code": "2000",
			"supplier_quotation_id": tyrells, "supplier_id": e.Supplier.String(), "supplier_number": "BP90000001", "supplier_reference": "Q-457",
			"contract_id": r["contract_id"], "contract_document_number": "CT100000", "valid_from": "2026-07-01", "valid_to": "2027-06-30",
			"total": map[string]any{"amount": "20666.67", "currency": "EUR"}}
		if len(awarded) != 1 || !reflect.DeepEqual(awarded[0], want) {
			t.Errorf("rfq_awarded events: got %v, want one, %v (the supplier by id and number, no name)", awarded, want)
		}
		changes := events(t, e, "rfq_changed")
		if last := changes[len(changes)-1]; last["change"] != "awarded" || last["status"] != "awarded" {
			t.Errorf("last rfq_changed event: got %v, want the award", last)
		}
		if n := len(events(t, e, "rfq_published")); n != 1 {
			t.Errorf("rfq_published events: got %d, want 1 (the award publishes none)", n)
		}
		for _, name := range []string{tyrell, cabot} {
			apitest.NeverPublished(t, e.Tenant, "procurement", name)
		}
		if got := audits(t, e, "procurement.rfq.award", id(r)); got != 1 {
			t.Errorf("audit records of the award: got %d, want 1", got)
		}
		if got := audits(t, e, "procurement.contract.create", fmt.Sprint(r["contract_id"])); got != 1 {
			t.Errorf("audit records of the contract's creation: got %d, want 1", got)
		}
	})
	t.Run("awarded once, and closed to quotations", func(t *testing.T) {
		e.in(t).Problem(409, "procurement.invalid_status", "POST", path+":award", awardBody(tyrells), awardedTag)
		e.in(t).Problem(409, "procurement.invalid_status", "POST", path+"/quotations", quotationBody(e.Vendor, "ANG-10", price(1, "1.00", "EUR", "")), "")
		list, _ := e.in(t).Must(200, "GET", "/v1/rfqs?status=awarded", "", "")
		if got := apitest.List(t, list, "items"); len(got) != 1 || got[0].(map[string]any)["contract_id"] != r["contract_id"] {
			t.Errorf("awarded RFQs: got %v, want the one with its contract", got)
		}
	})
	t.Run("the contract is released as any other", func(t *testing.T) {
		c := e.in(t).act(t, contract, "release", "")
		if c["status"] != "active" || c["rfq_id"] != id(r) {
			t.Errorf("released contract: got status %v of RFQ %v, want active of %s", c["status"], c["rfq_id"], id(r))
		}
		list, _ := e.in(t).Must(200, "GET", "/v1/contracts?supplier_id="+e.Supplier.String(), "", "")
		if got := apitest.List(t, list, "items"); len(got) != 1 || got[0].(map[string]any)["rfq_document_number"] != "RQ100000" {
			t.Errorf("the supplier's contracts: got %v, want the one of RFQ RQ100000", got)
		}
	})
}

// TestAwardRFQRefusals: what an award is refused for, each by the
// request's own field, and that a refused award changes nothing.
func TestAwardRFQRefusals(t *testing.T) {
	e := setupIn2026(t)
	path, tyrells, _ := e.quoted(t)
	other := "/v1/rfqs/" + e.publishedRFQ(t, "2026-07-31")
	part, _ := e.Must(201, "POST", other+"/quotations", quotationBody(e.Vendor, "PART", price(1, "400.00", "EUR", "")), "")
	late, _ := e.Must(201, "POST", other+"/quotations", quotationUntil(e.Supplier, "LATE", "2026-06-14", price(1, "1.00", "EUR", ""), price(2, "1.00", "EUR", "")), "")
	draft := "/v1/rfqs/" + id(e.rfq(t, "2026-07-31"))
	for name, c := range map[string]struct {
		path, body, tag string
		status          int
		code, field     string
	}{
		"a draft RFQ":                         {draft, awardBody(tyrells), "*", 409, "procurement.invalid_status", ""},
		"another version":                     {path, awardBody(tyrells), `"9"`, 412, "request.version_mismatch", ""},
		"no version":                          {path, awardBody(tyrells), "", 428, "request.precondition_required", ""},
		"a quotation of another RFQ":          {other, awardBody(tyrells), "*", 422, "procurement.quotation_not_of_rfq", "quotation_id"},
		"an incomplete quotation":             {other, awardBody(id(part)), "*", 422, "procurement.quotation_incomplete", "quotation_id"},
		"an expired quotation":                {other, awardBody(id(late)), "*", 422, "procurement.quotation_expired", "quotation_id"},
		"validity that ends before it starts": {path, fmt.Sprintf(`{"quotation_id": %q, "valid_from": "2026-07-01", "valid_to": "2026-06-30"}`, tyrells), "*", 422, "procurement.invalid_validity", "valid_to"},
		"no quotation":                        {path, `{"valid_from": "2026-07-01", "valid_to": "2027-06-30"}`, "*", 400, "request.invalid_body", ""},
		"no RFQ with the id":                  {"/v1/rfqs/" + uuid.NewString(), awardBody(tyrells), "*", 404, "request.not_found", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(c.status, c.code, "POST", c.path+":award", c.body, c.tag)
			if c.field == "" {
				return
			}
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 || errs[0].(map[string]any)["field"] != c.field {
				t.Errorf("field errors: got %v, want one on %s", p["errors"], c.field)
			}
		})
	}
	t.Run("the expired quotation's refusal names the day", func(t *testing.T) {
		p := e.in(t).Problem(422, "procurement.quotation_expired", "POST", other+":award", awardBody(id(late)), "*")
		if detail := fmt.Sprint(p["detail"]); !strings.Contains(detail, "2026-06-14") || !strings.Contains(detail, "2026-06-15 in UTC") {
			t.Errorf("detail: got %q, want the quotation's last day and 2026-06-15 in UTC", detail)
		}
	})
	t.Run("a supplier blocked since", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Supplier)
		p := e.in(t).Problem(422, "procurement.supplier_not_usable", "POST", path+":award", awardBody(tyrells), "*")
		if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "quotation_id" {
			t.Errorf("field errors: got %v, want one on quotation_id", p["errors"])
		}
		pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'active', version = version + 1 WHERE id = $1`, e.Supplier)
	})
	t.Run("a caller of another company", func(t *testing.T) {
		e.As(e.Reader(t, "Reader of 1000", e.US)).In(t).Problem(404, "request.not_found", "POST", path+":award", awardBody(tyrells), "*")
	})
	t.Run("an agent", func(t *testing.T) {
		agent := e.in(t).Client
		agent.As = e.Tenant.Agent
		agent.Problem(403, "identity.agent_not_allowed", "POST", path+":award", awardBody(tyrells), "*")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.rfq.award'`, e.Tenant.ID)
		e.in(t).Problem(403, "identity.permission_missing", "POST", path+":award", awardBody(tyrells), "*")
	})
	t.Run("nothing changed", func(t *testing.T) {
		if got, want := e.in(t).outcomes(t, path), "Q-457 recorded 1, ANG-9 recorded 1"; got != want {
			t.Errorf("quotations: got %s, want %s", got, want)
		}
		if n := len(events(t, e, "rfq_awarded")); n != 0 {
			t.Errorf("rfq_awarded events: got %d, want 0", n)
		}
		list, _ := e.in(t).Must(200, "GET", "/v1/contracts", "", "")
		if got := apitest.List(t, list, "items"); len(got) != 0 {
			t.Errorf("contracts: got %v, want none", got)
		}
	})
}

// TestAwardRFQReplayed: the same award with its Idempotency-Key again
// answers the same RFQ and contract and creates nothing more. The key is
// kept by the wall clock, so the test runs on it, with dates no run
// reaches.
func TestAwardRFQReplayed(t *testing.T) {
	e := setup(t)
	path := "/v1/rfqs/" + e.publishedRFQ(t, "2999-12-31")
	q, _ := e.Must(201, "POST", path+"/quotations", fmt.Sprintf(`{"supplier_id": %q, "supplier_reference": "Q", "valid_until": "2999-12-31", "items": %s}`,
		e.Supplier, items(price(1, "412.50", "EUR", ""), price(2, "80.00", "EUR", ""))), "")
	req := apitest.Req{Method: http.MethodPost, Path: path + ":award", Body: fmt.Sprintf(`{"quotation_id": %q, "valid_from": "2999-01-01", "valid_to": "2999-12-31"}`, id(q)),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: "*"}
	first, again := e.API.Do(req), e.API.Do(req)
	if first.Code != 200 || again.Code != 200 {
		t.Fatalf("statuses: got %d and %d, want 200 twice", first.Code, again.Code)
	}
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", again.Header().Get("Idempotent-Replayed"))
	}
	if got, want := apitest.JSON(t, again), apitest.JSON(t, first); !reflect.DeepEqual(got, want) {
		t.Errorf("replayed body: got %v, want %v", got, want)
	}
	list, _ := e.Must(200, "GET", "/v1/contracts", "", "")
	if got := apitest.List(t, list, "items"); len(got) != 1 || got[0].(map[string]any)["id"] != apitest.JSON(t, first)["contract_id"] {
		t.Errorf("contracts: got %v, want the award's alone", got)
	}
}

// TestAwardRFQOnce: two awards of one RFQ at once, to different
// quotations, serialize on the RFQ's row lock; the second finds it
// awarded and is refused, and one contract exists.
func TestAwardRFQOnce(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	path, tyrells, cabots := e.quoted(t)
	rfq := uuid.MustParse(strings.TrimPrefix(path, "/v1/rfqs/"))
	award := func(quotation string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.AwardRfq(ctx, &gen.RfqAward{QuotationID: uuid.MustParse(quotation), ValidFrom: dateOf("2026-07-01"), ValidTo: dateOf("2027-06-30")},
				gen.AwardRfqParams{ID: rfq, IfMatch: "*"})
			return err
		}
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, award(tyrells), award(cabots))
	if first != nil {
		t.Fatalf("first award: %v", first)
	}
	if got := problem.From(second).Code; got != "procurement.invalid_status" {
		t.Errorf("second award: got %s (%v), want procurement.invalid_status", got, second)
	}
	if got, want := e.outcomes(t, path), "Q-457 awarded 2, ANG-9 not_awarded 2"; got != want {
		t.Errorf("quotations: got %s, want %s", got, want)
	}
	if n := len(events(t, e, "rfq_awarded")); n != 1 {
		t.Errorf("rfq_awarded events: got %d, want 1", n)
	}
	list, _ := e.Must(200, "GET", "/v1/contracts", "", "")
	if got := apitest.List(t, list, "items"); len(got) != 1 {
		t.Errorf("contracts: got %d, want 1", len(got))
	}
}

// TestAwardRFQDatabaseFailures calls the award outside a unit of work and
// with each of its database statements failing in turn.
func TestAwardRFQDatabaseFailures(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	path, tyrells, _ := e.quoted(t)
	rfq := uuid.MustParse(strings.TrimPrefix(path, "/v1/rfqs/"))
	call := func(ctx context.Context) error {
		_, err := h.AwardRfq(ctx, &gen.RfqAward{QuotationID: uuid.MustParse(tyrells), ValidFrom: dateOf("2026-07-01"), ValidTo: dateOf("2027-06-30")},
			gen.AwardRfqParams{ID: rfq, IfMatch: "*"})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "award RFQ", call)
}
