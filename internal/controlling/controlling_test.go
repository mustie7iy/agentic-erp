package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// perms are what the tenant's people hold: the plan's, the ledger's entry
// read, which plan against actual and the orders' actuals need beside
// theirs, the orders', and posting journal entries to them.
var perms = []string{"controlling.plan.read", "controlling.plan.write", "controlling.plan.lock", "ledger.entry.read",
	"controlling.order.read", "controlling.order.write", "controlling.order.release", "ledger.entry.post"}

// plan is a tenant with company 2000's centers (fixture.CostCenters: Admin
// 1000, Sales 2000, Locked 3000, profit center PC1) and its accounts.
type plan struct {
	fixture.Env
	c                     fixture.Centers
	office, revenue, bank uuid.UUID // 520000 (expense), 400000 (revenue), 100000 (bank, balance sheet)
}

func setup(t *testing.T) plan {
	t.Helper()
	e := fixture.New(t, perms...)
	return plan{Env: e, c: e.CostCenters(t, nil), office: e.Account(t, "2000", "520000"), revenue: e.Account(t, "2000", "400000"),
		bank: e.Account(t, "2000", "100000")}
}

func (p plan) in(t *testing.T) plan {
	t.Helper()
	p.Client = p.In(t)
	return p
}

// as is p calling as another principal.
func (p plan) as(a pgtest.Actor) plan {
	p.Client = p.As(a)
	return p
}

// category creates a plan category and returns its id.
func (p plan) category(t *testing.T, code string) string {
	t.Helper()
	c, _ := p.Must(201, "POST", "/v1/plan-categories", fmt.Sprintf(`{"code": %q, "name": "Category %s"}`, code, code), "")
	return c["id"].(string)
}

// line is a batch line on the cost center (or, with pc, the profit
// center) in EUR.
func line(center uuid.UUID, account uuid.UUID, period int, amount string) string {
	return fmt.Sprintf(`{"cost_center_id": %q, "account_id": %q, "fiscal_period": %d, "amount": {"amount": %q, "currency": "EUR"}}`,
		center, account, period, amount)
}

func pcLine(center uuid.UUID, account uuid.UUID, period int, amount string) string {
	return fmt.Sprintf(`{"profit_center_id": %q, "account_id": %q, "fiscal_period": %d, "amount": {"amount": %q, "currency": "EUR"}}`,
		center, account, period, amount)
}

// batchBody is a batch of company 2000's 2026 plan.
func (p plan) batchBody(category, key string, lines ...string) string {
	return fmt.Sprintf(`{"plan_category_id": %q, "legal_entity_id": %q, "fiscal_year": 2026, "batch_key": %q, "lines": [%s]}`,
		category, p.DE, key, strings.Join(lines, ", "))
}

// batch applies a batch and returns the result.
func (p plan) batch(t *testing.T, category, key string, lines ...string) map[string]any {
	t.Helper()
	b, _ := p.Must(200, "POST", "/v1/plan-batches", p.batchBody(category, key, lines...), "")
	return b
}

// lines lists company 2000's current 2026 plan lines of a query as
// "center/account/period amount", the centers and accounts by their codes.
func (p plan) lines(t *testing.T, query string) []string {
	t.Helper()
	page, _ := p.Must(200, "GET", "/v1/plan-lines?legal_entity_id="+p.DE.String()+"&fiscal_year=2026"+query, "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		l := it.(map[string]any)
		center, _ := l["cost_center_id"].(string)
		if pc, ok := l["profit_center_id"].(string); ok {
			center = pc
		}
		q := ""
		if qty, ok := l["quantity"].(map[string]any); ok {
			q = fmt.Sprintf(" %s %s", qty["value"], qty["unit"])
		}
		out = append(out, fmt.Sprintf("%s/%s/%v %s%s", p.code(center), p.code(l["account_id"].(string)), l["fiscal_period"],
			l["amount"].(map[string]any)["amount"], q))
	}
	if tok, ok := page["next_page_token"].(string); ok {
		out = append(out, "next:"+tok)
	}
	return out
}

// code names the fixture's centers and accounts.
func (p plan) code(id string) string {
	for c, v := range map[string]uuid.UUID{"1000": p.c.Admin, "2000": p.c.Sales, "3000": p.c.Locked, "PC1": p.c.Profit, "520000": p.office,
		"400000": p.revenue, "100000": p.bank} {
		if v.String() == id {
			return c
		}
	}
	return id
}

// post posts the lines in company 2000 on the date, as a context does.
func (p plan) post(t *testing.T, day string, lines ...ledger.PostingLine) {
	t.Helper()
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	if err := apitest.Committed(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		_, err := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}.Post(ctx,
			ledger.Posting{LegalEntityID: p.DE, DocumentType: "SA", DocumentDate: d, PostingDate: d, Currency: "EUR", SourceContext: "payables",
				SourceDocumentID: uuid.New(), Lines: lines})
		return err
	}); err != nil {
		t.Fatalf("post on %s: %v", day, err)
	}
}

func on(account uuid.UUID, amount money.Minor, cc *uuid.UUID) ledger.PostingLine {
	return ledger.PostingLine{AccountID: account, Amount: amount, CostCenterID: cc}
}

// events are the controlling events of a name the tenant published.
func (p plan) events(t *testing.T, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, p.Tenant, outbox.Event{Context: "controlling", Name: name})
}

// audited lists the tenant's controlling audit actions in order.
func (p plan) audited(t *testing.T) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT coalesce(string_agg(action, ' ' ORDER BY seq), '') FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'controlling.%'`,
		[]any{p.Tenant.ID}, &got)
	return got
}

// endpoints is the context as the API wires it.
func endpoints(t *testing.T) *controlling.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return &controlling.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Ledger: ledger.AssignmentBalances{}}
}

func want[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func wantList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestPlanCategories(t *testing.T) {
	p := setup(t)
	id := p.category(t, "PLAN")
	path := "/v1/plan-categories/" + id
	got, tag := p.Must(200, "GET", path, "", "")
	want(t, "new category's lock", got["locked"], any(false))
	want(t, "etag", tag, `"1"`)
	p.Problem(409, "controlling.plan_category_exists", "POST", "/v1/plan-categories", `{"code": "PLAN", "name": "Again"}`, "")
	p.Blank("name", "POST", "/v1/plan-categories", `{"code": "BLANK", "name": "   "}`, "")
	p.category(t, "FORECAST")
	page, _ := p.Must(200, "GET", "/v1/plan-categories?code=PLAN", "", "")
	want(t, "categories with code PLAN", len(page["items"].([]any)), 1)
	page, _ = p.Must(200, "GET", "/v1/plan-categories?page_size=1", "", "")
	want(t, "first category by code", page["items"].([]any)[0].(map[string]any)["code"], any("FORECAST"))
	p.Problem(404, "request.not_found", "GET", "/v1/plan-categories/"+uuid.NewString(), "", "")
	p.Problem(400, "request.invalid_order_by", "GET", "/v1/plan-categories?order_by=name", "", "")

	p.Problem(412, "request.version_mismatch", "POST", path+":lock", "", `"9"`)
	locked, tag := p.Must(200, "POST", path+":lock", "", tag)
	want(t, "locked", locked["locked"], any(true))
	p.Problem(409, "controlling.plan_category_locked", "POST", path+":lock", "", tag)
	open, tag := p.Must(200, "POST", path+":unlock", "", tag)
	want(t, "unlocked", open["locked"], any(false))
	p.Problem(409, "controlling.plan_category_open", "POST", path+":unlock", "", tag)
	p.Problem(404, "request.not_found", "POST", "/v1/plan-categories/"+uuid.NewString()+":lock", "", `"1"`)

	var changes []string
	for _, ev := range p.events(t, "plan_category_changed") {
		changes = append(changes, fmt.Sprint(ev["code"], " ", ev["change"], " ", ev["locked"]))
	}
	wantList(t, "events", changes, []string{"PLAN created false", "FORECAST created false", "PLAN locked true", "PLAN unlocked false"})
	want(t, "audit", p.audited(t), "controlling.plan_category.create controlling.plan_category.create controlling.plan_category.lock "+
		"controlling.plan_category.unlock")
}

// TestPlanCategoryLockIsHuman: agents may not lock, unlock or create a
// category (x-agent: false; controlling.plan.lock is an approval
// permission), and a person without the lock permission may not lock.
func TestPlanCategoryLockIsHuman(t *testing.T) {
	p := setup(t)
	path := "/v1/plan-categories/" + p.category(t, "PLAN")
	agent := p.As(p.Tenant.Agent)
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":lock", "", `"1"`)
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":unlock", "", `"1"`)
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/plan-categories", `{"code": "AGENT", "name": "A"}`, "")
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/plan-batches", p.batchBody(uuid.NewString(), "k", line(p.c.Admin, p.office, 1, "1.00")), "")
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'controlling.plan.lock'`, p.Tenant.ID)
	p.Problem(403, "identity.permission_missing", "POST", path+":lock", "", `"1"`)
}
