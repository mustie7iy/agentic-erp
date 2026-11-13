package leases_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCreateContract (T88a; ADR-0073 §3-§5): a lease contract of three
// years paid monthly in advance is created as a draft, numbered from the
// company's lease_contract range, with its 36 payments on the first of
// each month, and reads back the same with what it names by id; the list
// gives it without its payments. One audit record and one
// LeaseContractChanged name the lessor by id and number; nothing is
// posted.
func TestCreateContract(t *testing.T) {
	e := setup(t)
	entries := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`)
	got, headers := e.Must(201, "POST", contracts, e.body(t, office, nil), "")
	id := got["id"].(string)
	if want := `"1"`; headers != want {
		t.Errorf("ETag: got %s, want %s", headers, want)
	}
	fields(t, got, map[string]any{"status": "draft", "version": float64(1), "legal_entity_id": e.DE.String(), "company_code": "2000",
		"company_name": "Acme GmbH", "lessor_id": e.Supplier.String(), "lessor_number": "BP90000001", "lessor_name": "BP90000001",
		"commencement_date": "2026-01-01", "end_date": "2028-12-31", "term_months": float64(36), "payment_frequency": "monthly",
		"payment_timing": "advance", "annual_discount_rate_percent": "6", "cost_center_id": e.center.String(), "cost_center_code": "CC-LEASE",
		"currency": "EUR", "payment_count": float64(36)})
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "LC") {
		t.Errorf("document_number: got %q, want one of the lease_contract range, prefix LC", n)
	}
	if got, want := amount(t, got, "payment_amount"), "1000.00 EUR"; got != want {
		t.Errorf("payment_amount: got %s, want %s", got, want)
	}
	if got, want := amount(t, got, "total_payments"), "36000.00 EUR"; got != want {
		t.Errorf("total_payments: got %s, want %s", got, want)
	}
	payments := rows(t, got, "payments")
	if len(payments) != 36 {
		t.Fatalf("payments: got %d, want 36", len(payments))
	}
	for i, want := range map[int]string{0: "1 2026-01-01 1000.00 EUR", 1: "2 2026-02-01 1000.00 EUR", 35: "36 2028-12-01 1000.00 EUR"} {
		if got := fmt.Sprintf("%v %v %s", payments[i]["payment_no"], payments[i]["due_date"], amount(t, payments[i], "amount")); got != want {
			t.Errorf("payment %d: got %s, want %s", i+1, got, want)
		}
	}
	read, _ := e.Must(200, "GET", contracts+"/"+id, "", "")
	if got, want := mustJSON(t, read), mustJSON(t, got); got != want {
		t.Errorf("read back: got %s, want %s", got, want)
	}
	page, _ := e.Must(200, "GET", contracts+"?legal_entity_id="+e.DE.String(), "", "")
	items := rows(t, page, "items")
	if len(items) != 1 || items[0]["id"] != id {
		t.Fatalf("list: got %v, want the contract", items)
	}
	if _, ok := items[0]["payments"]; ok {
		t.Errorf("list item: got payments %v, want none", items[0]["payments"])
	}
	if got, want := fmt.Sprintf("%v %s", items[0]["payment_count"], amount(t, items[0], "total_payments")), "36 36000.00 EUR"; got != want {
		t.Errorf("list item's payments: got %s, want %s", got, want)
	}
	events := e.events(t, id)
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	fields(t, events[0], map[string]any{"document_number": got["document_number"], "legal_entity_code": "2000", "lessor_id": e.Supplier.String(),
		"lessor_number": "BP90000001", "commencement_date": "2026-01-01", "term_months": float64(36), "payment_frequency": "monthly",
		"payment_timing": "advance", "payment_count": float64(36), "status": "draft", "status_before": nil, "version": float64(1)})
	if _, ok := events[0]["lessor_name"]; ok {
		t.Error("event: got the lessor's name, want its id and number only")
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'leases.contract.create' AND object_id = $2`, id); n != 1 {
		t.Errorf("audit records: got %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`); n != entries {
		t.Errorf("journal entries: got %d, want %d: a draft posts nothing", n, entries)
	}
}

// TestPaymentDates (T88a; ADR-0073 §4): a payment in advance is due on its
// interval's first day, one in arrears on its last; intervals are counted
// from the commencement date, on its day of the month or the month's last
// day when it has fewer, so a lease from the 31st does not drift.
func TestPaymentDates(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name string
		l    lease
		want string
		end  string
	}{
		{"monthly in arrears from the first", lease{"2026-01-01", 3, "10.00", "monthly", "arrears", "5"}, "2026-01-31 2026-02-28 2026-03-31", "2026-03-31"},
		{"monthly in advance from the 31st", lease{"2026-01-31", 3, "10.00", "monthly", "advance", "5"}, "2026-01-31 2026-02-28 2026-03-31", "2026-04-29"},
		{"monthly in arrears from the 31st", lease{"2026-01-31", 3, "10.00", "monthly", "arrears", "5"}, "2026-02-27 2026-03-30 2026-04-29", "2026-04-29"},
		{"quarterly in advance", lease{"2026-01-31", 12, "10.00", "quarterly", "advance", "5"}, "2026-01-31 2026-04-30 2026-07-31 2026-10-31", "2027-01-30"},
		{"yearly in arrears over a leap year", lease{"2026-03-01", 24, "10.00", "yearly", "arrears", "5"}, "2027-02-28 2028-02-29", "2028-02-29"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.create(t, c.l)
			var dates []string
			for _, p := range rows(t, got, "payments") {
				dates = append(dates, fmt.Sprint(p["due_date"]))
			}
			if got := strings.Join(dates, " "); got != c.want {
				t.Errorf("due dates: got %s, want %s", got, c.want)
			}
			if got["end_date"] != c.end {
				t.Errorf("end date: got %v, want %s", got["end_date"], c.end)
			}
		})
	}
}

// TestCreateRefusals (T88a; ADR-0073 §3): what the contract cannot be is
// refused naming the request's field, and nothing is written.
func TestCreateRefusals(t *testing.T) {
	e := setup(t)
	other := e.costCenter(t, e.US, "CC-US", "2026-01-01", nil)
	later := e.costCenter(t, e.DE, "CC-LATER", "2027-01-01", nil)
	stranger := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, 'BP70000001', 'organization', 'Not a supplier', 'DE', 'active', $3, $3)`, e.Tenant.ID, stranger, e.Tenant.Human.ID)
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`,
		e.Tenant.ID, e.Vendor)
	money := func(a, c string) map[string]any { return map[string]any{"amount": a, "currency": c} }
	cases := []struct {
		name   string
		l      lease
		over   map[string]any
		status int
		code   string
		field  any // nil: none
	}{
		{"an unknown frequency", lease{"2026-01-01", 12, "10.00", "weekly", "advance", "5"}, nil, 422, "leases.unknown_frequency", "payment_frequency"},
		{"an unknown timing", lease{"2026-01-01", 12, "10.00", "monthly", "midway", "5"}, nil, 422, "leases.unknown_timing", "payment_timing"},
		{"a term the frequency does not divide", lease{"2026-01-01", 10, "10.00", "quarterly", "advance", "5"}, nil, 422,
			"leases.frequency_not_dividing_term", "term_months"},
		{"a rate of zero", lease{"2026-01-01", 12, "10.00", "monthly", "advance", "0.000"}, nil, 422, "leases.invalid_discount_rate", "annual_discount_rate_percent"},
		{"a rate above 100", lease{"2026-01-01", 12, "10.00", "monthly", "advance", "100.000001"}, nil, 422, "leases.invalid_discount_rate",
			"annual_discount_rate_percent"},
		{"a rate with seven decimals", lease{"2026-01-01", 12, "10.00", "monthly", "advance", "4.1234567"}, nil, 422, "leases.invalid_discount_rate",
			"annual_discount_rate_percent"},
		{"a rate that is no decimal", lease{"2026-01-01", 12, "10.00", "monthly", "advance", "1e2"}, nil, 422, "leases.invalid_discount_rate",
			"annual_discount_rate_percent"},
		{"a negative rate", lease{"2026-01-01", 12, "10.00", "monthly", "advance", "-5"}, nil, 422, "leases.invalid_discount_rate", "annual_discount_rate_percent"},
		{"a term of no months", office, map[string]any{"term_months": 0}, 400, "request.invalid_body", nil},
		{"a term of 601 months", office, map[string]any{"term_months": 601}, 400, "request.invalid_body", nil},
		{"another company's currency", office, map[string]any{"payment_amount": money("10.00", "USD")}, 422, "leases.foreign_currency", "payment_amount.currency"},
		{"a payment of zero", office, map[string]any{"payment_amount": money("0.00", "EUR")}, 422, "leases.payment_not_positive", "payment_amount.amount"},
		{"a payment with three decimals", office, map[string]any{"payment_amount": money("10.001", "EUR")}, 400, "request.invalid_amount", "payment_amount.amount"},
		{"payments beyond an amount's range", lease{"2026-01-01", 2, "90000000000000000.00", "monthly", "advance", "5"}, nil, 422,
			"leases.amount_too_large", "payment_amount.amount"},
		{"an unknown company", office, map[string]any{"legal_entity_id": uuid.New()}, 422, "request.reference_not_found", "legal_entity_id"},
		{"an unknown lessor", office, map[string]any{"lessor_id": uuid.New()}, 422, "leases.lessor_not_usable", "lessor_id"},
		{"a lessor that is no supplier", office, map[string]any{"lessor_id": stranger}, 422, "leases.lessor_not_usable", "lessor_id"},
		{"a lessor blocked for posting", office, map[string]any{"lessor_id": e.Vendor}, 422, "leases.lessor_not_usable", "lessor_id"},
		{"an unknown cost center", office, map[string]any{"cost_center_id": uuid.New()}, 422, "leases.cost_center_invalid", "cost_center_id"},
		{"another company's cost center", office, map[string]any{"cost_center_id": other}, 422, "leases.cost_center_invalid", "cost_center_id"},
		{"a cost center not yet in force", office, map[string]any{"cost_center_id": later}, 422, "leases.cost_center_invalid", "cost_center_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.Problem(c.status, c.code, "POST", contracts, e.body(t, c.l, c.over), "")
			if got := field(got); got != c.field {
				t.Errorf("field: got %v, want %v", got, c.field)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM leases.contract WHERE tenant_id = $1`); n != 0 {
		t.Errorf("contracts: got %d, want 0", n)
	}
	t.Run("a cost center in force from the commencement date", func(t *testing.T) {
		e.create(t, lease{"2027-01-01", 12, "10.00", "monthly", "advance", "5"})
	})
}

// TestCompanyAccess (T88a; 04 §4.9): a person who may act in company 1000
// only does not see company 2000's contracts, by id or in the list, and
// cannot create one there.
func TestCompanyAccess(t *testing.T) {
	e := setup(t)
	id := e.create(t, office)["id"].(string)
	other := e.As(e.Reader(t, "uma", e.US))
	other.Problem(404, "request.not_found", "GET", contracts+"/"+id, "", "")
	page, _ := other.Must(200, "GET", contracts, "", "")
	if items := rows(t, page, "items"); len(items) != 0 {
		t.Errorf("list: got %d contracts, want none", len(items))
	}
	got := other.Problem(422, "request.reference_not_found", "POST", contracts, e.body(t, office, nil), "")
	if field(got) != "legal_entity_id" {
		t.Errorf("field: got %v, want legal_entity_id", field(got))
	}
	e.Problem(404, "request.not_found", "GET", contracts+"/"+uuid.NewString(), "", "")
}

// TestListContracts (T88a): the list filters by company, lessor and
// status and pages by number.
func TestListContracts(t *testing.T) {
	e := setup(t)
	first := e.create(t, office)
	second := e.create(t, lease{"2026-02-01", 12, "50.00", "quarterly", "arrears", "4.25"})
	vendor, _ := e.Must(201, "POST", contracts, e.body(t, office, map[string]any{"lessor_id": e.Vendor}), "")
	ids := func(path string) string {
		t.Helper()
		page, _ := e.Must(200, "GET", path, "", "")
		var out []string
		for _, it := range rows(t, page, "items") {
			out = append(out, it["document_number"].(string))
		}
		return strings.Join(out, " ")
	}
	all := strings.Join([]string{first["document_number"].(string), second["document_number"].(string), vendor["document_number"].(string)}, " ")
	cases := []struct{ name, path, want string }{
		{"all, by number", contracts, all},
		{"of the company", contracts + "?legal_entity_id=" + e.DE.String(), all},
		{"of another company", contracts + "?legal_entity_id=" + e.US.String(), ""},
		{"of a lessor", contracts + "?lessor_id=" + e.Vendor.String(), vendor["document_number"].(string)},
		{"drafts", contracts + "?status=draft", all},
		{"of a status none has", contracts + "?status=active", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ids(c.path); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("paged", func(t *testing.T) {
		page, _ := e.Must(200, "GET", contracts+"?page_size=2", "", "")
		token, _ := page["next_page_token"].(string)
		if len(rows(t, page, "items")) != 2 || token == "" {
			t.Fatalf("first page: got %v, want two contracts and a token", page)
		}
		if got, want := ids(contracts+"?page_size=2&page_token="+token), vendor["document_number"]; got != want {
			t.Errorf("second page: got %q, want %q", got, want)
		}
	})
}
