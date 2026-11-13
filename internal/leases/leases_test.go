package leases_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/leases"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"leases.contract.read", "leases.contract.write"}

const contracts = "/v1/lease-contracts"

// env is a configured tenant whose company 2000 (EUR) has a cost center
// in force from 2026, so lease contracts can be entered there with the
// fixture's supplier as lessor.
type env struct {
	fixture.Env
	center uuid.UUID // CC-LEASE of company 2000, in force from 2026-01-01
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{Env: fixture.New(t, perms...)}
	e.center = e.costCenter(t, e.DE, "CC-LEASE", "2026-01-01", nil)
	return e
}

// costCenter writes an active cost center of the legal entity in force
// from the day, until the day before to (nil: without end), without the
// tables' triggers, as fixture.CostCenter writes its centers.
func (e env) costCenter(t *testing.T, le uuid.UUID, code, from string, to *string) uuid.UUID {
	t.Helper()
	id := e.CostCenter(t, le, code)
	ctx := context.Background()
	admin := pgtest.Admin(t)
	_, err := admin.Exec(ctx, `SET session_replication_role = replica`)
	if err == nil {
		_, err = admin.Exec(ctx, `INSERT INTO ledger.cost_center_period (tenant_id, id, cost_center_id, legal_entity_id, valid_from, valid_to, category,
				change_id, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, 'ADMIN', gen_random_uuid(), $6, $6)`, e.Tenant.ID, id, le, from, to, e.Tenant.Human.ID)
	}
	if err == nil {
		_, err = admin.Exec(ctx, `RESET session_replication_role`)
	}
	if err != nil {
		t.Fatalf("period of cost center %s: %v", code, err)
	}
	return id
}

// lease is a contract of company 2000 with the fixture's supplier and
// cost center: the term in months, a payment of the amount in EUR at the
// frequency and timing, at the rate, from the commencement date.
type lease struct {
	commencement string
	term         int
	amount       string
	frequency    string
	timing       string
	rate         string
}

// office is three years of office space at 1000.00 a month in advance at
// 6 percent from 1 January 2026.
var office = lease{commencement: "2026-01-01", term: 36, amount: "1000.00", frequency: "monthly", timing: "advance", rate: "6"}

// body is the JSON of the lease's creation, with fields replaced or added
// by over.
func (e env) body(t *testing.T, l lease, over map[string]any) string {
	t.Helper()
	m := map[string]any{"legal_entity_id": e.DE, "lessor_id": e.Supplier, "commencement_date": l.commencement, "term_months": l.term,
		"payment_amount": map[string]any{"amount": l.amount, "currency": "EUR"}, "payment_frequency": l.frequency, "payment_timing": l.timing,
		"annual_discount_rate_percent": l.rate, "cost_center_id": e.center}
	for k, v := range over {
		m[k] = v
	}
	return mustJSON(t, m)
}

// create creates the lease and returns the contract.
func (e env) create(t *testing.T, l lease) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", contracts, e.body(t, l, nil), "")
	return got
}

func amount(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a, ok := m[field].(map[string]any)
	if !ok {
		t.Fatalf("%s: got %v, want an amount", field, m[field])
	}
	return fmt.Sprintf("%v %v", a["amount"], a["currency"])
}

// rows are the objects of the array field of m.
func rows(t *testing.T, m map[string]any, field string) []map[string]any {
	t.Helper()
	raw, _ := m[field].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

// field is the field the problem's first field error names.
func field(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	e, _ := errs[0].(map[string]any)
	return e["field"]
}

// fields checks each named field of m, one message per field.
func fields(t *testing.T, m, want map[string]any) {
	t.Helper()
	for f, w := range want {
		if m[f] != w {
			t.Errorf("%s: got %v, want %v", f, m[f], w)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *leases.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Leases(app.Config{Pages: pages, Now: time.Now})
}

// count is the number the query gives for the tenant, whose id is $1.
func (e env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	pgtest.Scan(t, query, append([]any{e.Tenant.ID}, args...), &n)
	return n
}

// events are the payloads of LeaseContractChanged for the contract, in the
// order they were written, each conforming to the event contract.
func (e env) events(t *testing.T, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "leases", Name: "lease_contract_changed"}) {
		if p["lease_contract_id"] == id {
			out = append(out, p)
		}
	}
	return out
}

func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}
