package receivables_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/csvcell"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// centerScene is agingScene with two customer items on profit center PC10
// of segment SEG1, posted by manual entries on 2 April without terms:
// Northwind's of 70.00 and that of a customer named as a formula would
// be, "=Evil", of 30.00. Every other item carries no profit center.
func centerScene(t *testing.T) (env, fixtureCenters) {
	t.Helper()
	e := setup(t)
	for _, p := range []string{"ledger.cost_center.read", "ledger.cost_center.change"} { // what Centers needs, granted as CostCenters grants it
		pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT tenant_id, gen_random_uuid(), id, $2, current_date - 1, $3 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`,
			e.Tenant.ID, p, e.Tenant.Human.ID)
	}
	e.Controller(t)
	c := e.Centers(t)
	agingItems(t, e)
	evil := e.partner(t, "BP70000003", "=Evil", "active", "customer")
	e.companyData(t, evil, "NT30")
	for _, it := range []struct {
		customer uuid.UUID
		amount   money.Minor
	}{{e.customer, 7000}, {evil, 3000}} {
		e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
			return postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date(t, "2026-04-02"),
				PostingDate: date(t, "2026-04-02"), Currency: "EUR", SourceContext: "ledger", Lines: []ledger.PostingLine{
					{AccountID: e.receivable, Amount: it.amount, PartnerID: &it.customer, ProfitCenterID: &c.Profit},
					{AccountID: e.revenue, Amount: -it.amount, ProfitCenterID: &c.Profit}}})
		})
	}
	return e, fixtureCenters{profit: c.Profit, segment: c.Segment, evil: evil}
}

// fixtureCenters are the scene's profit center, segment and second
// customer.
type fixtureCenters struct{ profit, segment, evil uuid.UUID }

// TestAgingByCenter (T75m acceptance): each item is in exactly one group
// by its line's own profit center or segment, the row without one first;
// the groups add up to the totals; a group's customers are counted once
// each; a filter selects the items carrying the center, with a grouping
// too; an unknown center selects nothing.
func TestAgingByCenter(t *testing.T) {
	e, c := centerScene(t)
	got := e.aging(t, "key_date=2026-05-15&group_by=profit_center")
	fields(t, "the report", got, map[string]any{"group_by": "profit_center"})
	if rows := items(t, got); len(rows) != 0 {
		t.Errorf("items: got %d, want none when grouped", len(rows))
	}
	groups := items(t, map[string]any{"items": got["groups"]})
	if len(groups) != 2 {
		t.Fatalf("groups: got %d, want the row without a profit center and PC10", len(groups))
	}
	if _, named := groups[0]["profit_center_id"]; named {
		t.Errorf("the first group: got %v, want the row without a profit center", groups[0]["profit_center_id"])
	}
	fields(t, "the row without", groups[0], map[string]any{"customer_count": 2, "item_count": 5, "amount": eur("315.00")})
	fields(t, "PC10's row", groups[1], map[string]any{"profit_center_id": c.profit, "profit_center_code": "PC10", "profit_center_name": "Profit center ten",
		"customer_count": 2, "item_count": 2, "amount": eur("100.00")})
	// The items without terms are due on their document date, 2 April:
	// 43 days overdue on 15 May.
	if b, want := buckets(t, groups[1]), "not_due 0 0.00 EUR; overdue[1-30] 0 0.00 EUR; overdue[31-60] 2 100.00 EUR; overdue[61-90] 0 0.00 EUR; "+
		"overdue[91-<nil>] 0 0.00 EUR; no_due_date 0 0.00 EUR; "; b != want {
		t.Errorf("PC10's buckets: got %s, want %s", b, want)
	}
	totals, _ := got["totals"].(map[string]any)
	fields(t, "the totals", totals, map[string]any{"customer_count": 3, "item_count": 7, "amount": eur("415.00")})
	for _, c := range []struct {
		name, query string
		groups      int
		count       int
		amount      string
	}{
		{"by segment", "group_by=segment", 2, 7, "415.00"},
		{"PC10's items", "profit_center_id=" + c.profit.String(), 0, 2, "100.00"},
		{"SEG1's items by profit center", "segment_id=" + c.segment.String() + "&group_by=profit_center", 1, 2, "100.00"},
		{"PC10 and SEG1", "profit_center_id=" + c.profit.String() + "&segment_id=" + c.segment.String(), 0, 2, "100.00"},
		{"an unknown profit center", "profit_center_id=" + uuid.NewString() + "&group_by=profit_center", 0, 0, "0.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := e.aging(t, "key_date=2026-05-15&"+c.query)
			if groups, _ := got["groups"].([]any); len(groups) != c.groups {
				t.Errorf("groups: got %d, want %d", len(groups), c.groups)
			}
			totals, _ := got["totals"].(map[string]any)
			fields(t, "the totals", totals, map[string]any{"item_count": c.count, "amount": eur(c.amount)})
		})
	}
	t.Run("the segment's row", func(t *testing.T) {
		groups, _ := e.aging(t, "key_date=2026-05-15&group_by=segment")["groups"].([]any)
		seg, _ := groups[len(groups)-1].(map[string]any)
		fields(t, "SEG1's row", seg, map[string]any{"segment_id": c.segment, "segment_code": "SEG1", "segment_name": "Segment one", "item_count": 2})
		if _, named := seg["profit_center_id"]; named {
			t.Errorf("SEG1's row: got a profit center %v, want none", seg["profit_center_id"])
		}
	})
	t.Run("the customers' rows of PC10", func(t *testing.T) {
		rows := items(t, e.aging(t, "key_date=2026-05-15&profit_center_id="+c.profit.String()))
		if len(rows) != 2 {
			t.Fatalf("customers: got %d, want Northwind and =Evil", len(rows))
		}
		fields(t, "Northwind's row", rows[0], map[string]any{"customer_id": e.customer, "item_count": 1, "amount": eur("70.00")})
	})
}

// TestAgingGroupPages: groups come in cursor pages, the row without a
// center first; a token is good only for the grouping and filters it was
// given for; a group_by or format the contract does not list is refused.
func TestAgingGroupPages(t *testing.T) {
	e, c := centerScene(t)
	first := e.aging(t, "key_date=2026-05-15&group_by=profit_center&page_size=1")
	token, _ := first["next_page_token"].(string)
	if groups, _ := first["groups"].([]any); len(groups) != 1 || token == "" {
		t.Fatalf("first page: got %v with token %q, want one group and a token", groups, token)
	}
	second := e.aging(t, "key_date=2026-05-15&group_by=profit_center&page_size=1&page_token="+token)
	groups, _ := second["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["profit_center_code"] != "PC10" {
		t.Errorf("second page: got %v, want PC10", groups)
	}
	if _, more := second["next_page_token"]; more {
		t.Errorf("second page: got a token, want none on the last page")
	}
	path := "/v1/receivables-aging?legal_entity_id=" + e.DE.String() + "&key_date=2026-05-15"
	for name, query := range map[string]string{
		"another grouping":      "&group_by=segment&page_size=1&page_token=" + token,
		"a profit center added": "&group_by=profit_center&profit_center_id=" + c.profit.String() + "&page_token=" + token,
		"a segment added":       "&group_by=profit_center&segment_id=" + c.segment.String() + "&page_token=" + token,
	} {
		t.Run("a token used with "+name, func(t *testing.T) {
			e.Problem(400, "request.invalid_page_token", "GET", path+query, "", "")
		})
	}
	for param, value := range map[string]string{"group_by": "cost_center", "format": "xlsx"} {
		t.Run(param, func(t *testing.T) {
			if p := e.Problem(400, "request.invalid_parameter", "GET", path+"&"+param+"="+value, "", ""); field(p) != param {
				t.Errorf("the field refused: got %v, want %s", field(p), param)
			}
		})
	}
}

// csvOf reads the aging as CSV: its body without the byte order mark,
// which it must begin with, and its next page's token.
func (e env) csvOf(t *testing.T, query string) (string, string) {
	t.Helper()
	r := e.API.Do(apitest.Req{Method: http.MethodGet, Path: "/v1/receivables-aging?legal_entity_id=" + e.DE.String() + "&format=csv&" + query,
		Tenant: e.Tenant.ID, As: e.Tenant.Human})
	body, err := io.ReadAll(r.Body)
	if err != nil || r.Code != 200 {
		t.Fatalf("CSV: got %d %v, want 200", r.Code, err)
	}
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type: got %q, want text/csv", ct)
	}
	text, bom := strings.CutPrefix(string(body), csvcell.ByteOrderMark)
	if !bom {
		t.Errorf("the CSV: got %q at its start, want a byte order mark", body[:min(len(body), 3)])
	}
	return text, r.Header().Get("Next-Page-Token")
}

// TestAgingCSV (T75m acceptance): the CSV holds the JSON's figures: the
// header with a column per bucket, a row per row of the page, the totals'
// row on the last page only; a text beginning as a formula is kept as
// text; the next page's token in Next-Page-Token.
func TestAgingCSV(t *testing.T) {
	e, c := centerScene(t)
	got, next := e.csvOf(t, "key_date=2026-05-15&group_by=profit_center&aging_days=30")
	want := "\"row\",\"id\",\"code\",\"name\",\"currency\",\"customer_count\",\"item_count\",\"amount\",\"not_due\",\"overdue_1_30\",\"overdue_31_plus\",\"no_due_date\"\n" +
		"\"profit_center\",,,,\"EUR\",2,5,315.00,250.00,0.00,65.00,0.00\n" +
		"\"profit_center\",\"" + c.profit.String() + "\",\"PC10\",\"Profit center ten\",\"EUR\",2,2,100.00,0.00,0.00,100.00,0.00\n" +
		"\"total\",,,,\"EUR\",3,7,415.00,250.00,0.00,165.00,0.00\n"
	if got != want || next != "" {
		t.Errorf("the CSV by profit center: got %q with token %q, want %q and none", got, next, want)
	}
	got, next = e.csvOf(t, "key_date=2026-05-15&profit_center_id="+c.profit.String()+"&page_size=1")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || next == "" || !strings.HasPrefix(lines[1], "\"customer\",\""+e.customer.String()+"\",\"BP70000001\",\"Northwind Traders\",\"EUR\",1,1,70.00,") {
		t.Fatalf("the first page of PC10's customers: got %q with token %q, want the header, Northwind and a token, no totals", got, next)
	}
	got, next = e.csvOf(t, "key_date=2026-05-15&profit_center_id="+c.profit.String()+"&page_size=1&page_token="+next)
	lines = strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 || next != "" || !strings.HasPrefix(lines[1], "\"customer\",\""+c.evil.String()+"\",\"BP70000003\",\"'=Evil\",") ||
		!strings.HasPrefix(lines[2], "\"total\",,,,\"EUR\",2,2,100.00,") {
		t.Errorf("the last page: got %q with token %q, want =Evil kept as text and the totals", got, next)
	}
}
