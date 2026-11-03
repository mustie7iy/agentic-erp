package closing_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// codes reads a page of closes or tasks as one field of each item.
func codes(t *testing.T, page map[string]any, format func(map[string]any) string) []string {
	t.Helper()
	var out []string
	for _, x := range apitest.List(t, page, "items") {
		out = append(out, format(x.(map[string]any)))
	}
	return out
}

func closeItem(m map[string]any) string {
	return fmt.Sprintf("%v %v/%v %v %v/%v overdue %v", m["company_code"], m["fiscal_year"], m["fiscal_period"], m["status"], m["tasks_done"], m["tasks_total"],
		m["tasks_overdue"])
}

// TestListPeriodCloses: the closes of the caller's companies by period
// end, filtered by company, year and status, paged.
func TestListPeriodCloses(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	c := at(t, e, time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC))
	open(t, c, e.US, 2, "")
	open(t, c, e.US, 1, "")
	open(t, c, e.DE, 1, "")
	page, _ := c.Must(200, "GET", "/v1/period-closes?page_size=2", "", "")
	wantLines(t, "first page", codes(t, page, closeItem), []string{"1000 2026/1 open 0/3 overdue 2", "2000 2026/1 open 0/2 overdue 2"})
	next, _ := c.Must(200, "GET", "/v1/period-closes?page_size=2&page_token="+apitest.String(t, page, "next_page_token"), "", "")
	wantLines(t, "second page", codes(t, next, closeItem), []string{"1000 2026/2 open 0/3 overdue 0"})
	if _, more := next["next_page_token"]; more {
		t.Errorf("second page: got a next page token, want none")
	}
	for query, want := range map[string][]string{
		"legal_entity_id=" + e.DE.String():          {"2000 2026/1 open 0/2 overdue 2"},
		"fiscal_year=2027":                          nil,
		"status=signed_off":                         nil,
		"order_by=created_at+desc&fiscal_year=2026": {"2000 2026/1 open 0/2 overdue 2", "1000 2026/1 open 0/3 overdue 2", "1000 2026/2 open 0/3 overdue 0"},
	} {
		t.Run(query, func(t *testing.T) {
			got, _ := c.In(t).Must(200, "GET", "/v1/period-closes?"+query, "", "")
			wantLines(t, query, codes(t, got, closeItem), want)
		})
	}
	reader, _ := e.As(e.Reader(t, "reader", e.DE)).Must(200, "GET", "/v1/period-closes", "", "")
	if got := len(apitest.List(t, reader, "items")); got != 1 {
		t.Errorf("a reader of company 2000: got %d closes, want 1", got)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/period-closes?order_by=status", "", "")
	year, _ := c.Must(200, "GET", "/v1/period-closes?fiscal_year=2026&page_size=2", "", "")
	yearRest, _ := c.Must(200, "GET", "/v1/period-closes?fiscal_year=2026&page_size=2&page_token="+apitest.String(t, year, "next_page_token"), "", "")
	wantLines(t, "a year's closes, paged", append(codes(t, year, closeItem), codes(t, yearRest, closeItem)...),
		[]string{"1000 2026/1 open 0/3 overdue 2", "2000 2026/1 open 0/2 overdue 2", "1000 2026/2 open 0/3 overdue 0"})
	byCreation, _ := c.Must(200, "GET", "/v1/period-closes?order_by=created_at&page_size=2", "", "")
	rest, _ := c.Must(200, "GET", "/v1/period-closes?order_by=created_at&page_size=2&page_token="+apitest.String(t, byCreation, "next_page_token"), "", "")
	wantLines(t, "by creation", append(codes(t, byCreation, closeItem), codes(t, rest, closeItem)...),
		[]string{"1000 2026/2 open 0/3 overdue 0", "1000 2026/1 open 0/3 overdue 2", "2000 2026/1 open 0/2 overdue 2"})
}

func taskItem(m map[string]any) string {
	return fmt.Sprintf("%v/%v %v %v %v", m["company_code"], m["fiscal_period"], m["code"], m["due_on"], m["status"])
}

// TestListCloseTasks: tasks across closes in due date, close and code
// order, by assignee (me or an id), status, company, close and due date,
// paged.
func TestListCloseTasks(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	c := at(t, e, time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC))
	us, tag := open(t, c, e.US, 1, "")
	de, _ := open(t, c, e.DE, 1, "")
	approver := e.Approver.ID
	c.Must(200, "PATCH", "/v1/period-closes/"+apitest.String(t, us, "id")+"/tasks/REPORT", fmt.Sprintf(`{"assignee_id": %q}`, approver), tag)
	_, tag = c.Must(200, "PATCH", "/v1/period-closes/"+apitest.String(t, us, "id")+"/tasks/REVIEW", fmt.Sprintf(`{"assignee_id": %q}`, e.Tenant.Human.ID), `"2"`)
	c.Must(200, "POST", "/v1/period-closes/"+apitest.String(t, us, "id")+"/tasks/REVIEW:complete", "", tag)

	// Company 2000's weekend is Friday and Saturday: its REVIEW is on
	// Thursday the 29th and its AP_CUTOFF on Monday 2 February.
	page, _ := c.Must(200, "GET", "/v1/close-tasks?page_size=3", "", "")
	wantLines(t, "first page", codes(t, page, taskItem), []string{"2000/1 REVIEW 2026-01-29 open", "1000/1 REVIEW 2026-01-30 done",
		"2000/1 AP_CUTOFF 2026-02-02 open"})
	next, _ := c.Must(200, "GET", "/v1/close-tasks?page_size=3&page_token="+apitest.String(t, page, "next_page_token"), "", "")
	wantLines(t, "second page", codes(t, next, taskItem), []string{"1000/1 AP_CUTOFF 2026-02-04 open", "1000/1 REPORT 2026-02-09 open"})
	for query, want := range map[string][]string{
		"assignee=me":                                  {"1000/1 REVIEW 2026-01-30 done"},
		"assignee=" + approver.String():                {"1000/1 REPORT 2026-02-09 open"},
		"status=open&legal_entity_id=" + e.US.String(): {"1000/1 AP_CUTOFF 2026-02-04 open", "1000/1 REPORT 2026-02-09 open"},
		"close_id=" + apitest.String(t, de, "id"):      {"2000/1 REVIEW 2026-01-29 open", "2000/1 AP_CUTOFF 2026-02-02 open"},
		"due_before=2026-01-30":                        {"2000/1 REVIEW 2026-01-29 open"},
	} {
		t.Run(query, func(t *testing.T) {
			got, _ := c.In(t).Must(200, "GET", "/v1/close-tasks?"+query, "", "")
			wantLines(t, query, codes(t, got, taskItem), want)
		})
	}
	mine, _ := c.Must(200, "GET", "/v1/close-tasks?assignee=me", "", "")
	item := apitest.List(t, mine, "items")[0].(map[string]any)
	wantFields(t, "a task of the worklist", item, map[string]any{"close_id": us["id"], "close_version": 4.0, "company_name": "Acme Corp US"})
	if item["assignee_name"] == nil {
		t.Errorf("a task of the worklist: got no assignee_name, want the assignee's name")
	}
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/close-tasks?assignee=me&page_token="+apitest.String(t, page, "next_page_token"), "", "")
}
