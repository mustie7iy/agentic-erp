package credit_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/credit"
)

// limitFor is the JSON of a credit limit of 500.00 EUR of the customer in
// company 2000 with the dates given, each "" when absent.
func (e env) limitFor(from, to, review string) string {
	body := fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "limit": {"amount": "500.00", "currency": "EUR"}`, e.DE, e.north)
	for f, v := range map[string]string{"valid_from": from, "valid_to": to, "next_review_on": review} {
		if v != "" {
			body += fmt.Sprintf(", %q: %q", f, v)
		}
	}
	return body + "}"
}

// dates are a limit's validity and review date as "from to review", "-"
// for what is absent.
func dates(m map[string]any) string {
	out := make([]string, 3)
	for i, f := range []string{"valid_from", "valid_to", "next_review_on"} {
		out[i] = "-"
		if v, ok := m[f]; ok {
			out[i] = fmt.Sprint(v)
		}
	}
	return strings.Join(out, " ")
}

// TestValidityAndReview: a limit is set with a validity and a next review
// date, which it reads back with; a validity ending before it starts is
// refused; a change sets, moves or clears the dates as a merge patch does,
// and is audited and published with them; a change that leaves them as
// they are is refused (T77c acceptance).
func TestValidityAndReview(t *testing.T) {
	e := setup(t)
	t.Run("a validity ending before it starts", func(t *testing.T) {
		p := e.In(t).Problem(422, "credit.invalid_validity", "POST", "/v1/credit-limits", e.limitFor("2027-02-01", "2027-01-31", ""), "")
		if f := field(p); f != "valid_to" {
			t.Errorf("field: got %v, want valid_to", f)
		}
	})
	set, tag := e.Must(201, "POST", "/v1/credit-limits", e.limitFor("2027-01-01", "2027-12-31", "2027-10-01"), "")
	if d := dates(set); d != "2027-01-01 2027-12-31 2027-10-01" {
		t.Errorf("set: got %s, want 2027-01-01 2027-12-31 2027-10-01", d)
	}
	id := apitest.String(t, set, "id")
	path := "/v1/credit-limits/" + id
	for _, c := range []struct{ name, body, want string }{
		{"the review moved", `{"next_review_on": "2027-04-01"}`, "2027-01-01 2027-12-31 2027-04-01"},
		{"the end cleared", `{"valid_to": null}`, "2027-01-01 - 2027-04-01"},
		{"the start cleared and an end set", `{"valid_from": null, "valid_to": "2027-06-30"}`, "- 2027-06-30 2027-04-01"},
		{"the review cleared with the amount", `{"next_review_on": null, "limit": {"amount": "400.00", "currency": "EUR"}}`, "- 2027-06-30 -"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, next := e.In(t).Must(200, "PATCH", path, c.body, tag)
			tag = next
			if d := dates(got); d != c.want {
				t.Errorf("got %s, want %s", d, c.want)
			}
		})
	}
	t.Run("a start after the end", func(t *testing.T) {
		p := e.In(t).Problem(422, "credit.invalid_validity", "PATCH", path, `{"valid_from": "2027-07-01"}`, tag)
		if f := field(p); f != "valid_to" {
			t.Errorf("field: got %v, want valid_to", f)
		}
	})
	t.Run("the dates it has", func(t *testing.T) {
		e.In(t).Problem(422, "credit.nothing_to_change", "PATCH", path, `{"valid_from": null, "valid_to": "2027-06-30"}`, tag)
	})
	t.Run("nothing", func(t *testing.T) {
		e.In(t).Problem(400, "request.invalid_body", "PATCH", path, `{}`, tag)
	})
	audits := e.audited(t, id)
	if len(audits) != 5 {
		t.Fatalf("audit records: got %d, want 5: the limit set and changed four times", len(audits))
	}
	if want := `"valid_to": "2027-06-30", "valid_from": null, `; !strings.Contains(audits[3], want) || !strings.Contains(audits[3], `"valid_from_before": "2027-01-01"`) {
		t.Errorf("the third change's audit record: got %s, want the dates after (%s) and before", audits[3], want)
	}
	events := e.changed(t)
	if got := fmt.Sprintf("%v %v %v", events[0]["valid_from"], events[0]["valid_to"], events[0]["next_review_on"]); got != "2027-01-01 2027-12-31 2027-10-01" {
		t.Errorf("CreditLimitChanged of the limit set: got %s, want its dates", got)
	}
	if last := events[len(events)-1]; fmt.Sprintf("%v %v %v", last["valid_from"], last["valid_to"], last["next_review_on"]) != "<nil> 2027-06-30 <nil>" {
		t.Errorf("CreditLimitChanged of the last change: got %v %v %v, want <nil> 2027-06-30 <nil>", last["valid_from"], last["valid_to"], last["next_review_on"])
	}
}

// TestWorklist: due_by lists the limits whose next review or end of
// validity is on or before the day, those past it too, and no limit
// without either (T77c).
func TestWorklist(t *testing.T) {
	e := setup(t)
	e.Must(201, "POST", "/v1/credit-limits", e.limitFor("", "2027-06-30", ""), "")
	e.Must(201, "POST", "/v1/credit-limits", limitOf(e.US, e.north, "900.00", "USD"), "")
	east := fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "limit": {"amount": "100.00", "currency": "EUR"}, "next_review_on": "2027-03-31"}`, e.DE, e.east)
	e.Must(201, "POST", "/v1/credit-limits", east, "")
	for _, c := range []struct{ name, due, want string }{
		{"before either date", "2027-03-30", ""},
		{"on the review date", "2027-03-31", "BP70000003/2000"},
		{"on the end of validity", "2027-06-30", "BP70000002/2000 BP70000003/2000"},
		{"long after both", "2030-01-01", "BP70000002/2000 BP70000003/2000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/credit-limits?due_by="+c.due, "", "")
			var got []string
			for _, l := range objects(t, page, "items") {
				got = append(got, fmt.Sprintf("%v/%v", l["customer_number"], l["company_code"]))
			}
			if g := strings.Join(got, " "); g != c.want {
				t.Errorf("got %q, want %q", g, c.want)
			}
		})
	}
}

// zoned is the configuration with company 2000 in Auckland's time zone,
// so its day is ahead of UTC's.
func zoned(doc *config.Config) {
	receivables(doc)
	doc.Company("2000").TimeZone = "Pacific/Auckland"
}

// TestValidOnTheCompanysDay: the exposure and the check at an order's
// confirmation use a limit only on the company's days within its
// validity, judged in its time zone: on its first and last day it is used,
// the day before it starts and the day after it ends the customer is
// treated as one without a limit, and the exposure says which day it judged
// and still names the limit (T77c acceptance).
func TestValidOnTheCompanysDay(t *testing.T) {
	e := setupWith(t, zoned)
	e.story(t) // Northwind owes 290.00 EUR
	set, tag := e.Must(201, "POST", "/v1/credit-limits", e.limitFor("", "", ""), "")
	id := apitest.String(t, set, "id")
	h := endpoints(t)
	// 20:00 UTC on 30 June is 1 July in Auckland.
	h.Now = func() time.Time { return time.Date(2026, 6, 30, 20, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		name, body string
		valid      bool
	}{
		{"open at both ends", `{"next_review_on": "2026-07-01"}`, true},
		{"from the company's day", `{"valid_from": "2026-07-01"}`, true},
		{"until the company's day", `{"valid_from": null, "valid_to": "2026-07-01"}`, true},
		{"ended on UTC's day", `{"valid_to": "2026-06-30"}`, false},
		{"from the day after", `{"valid_from": "2026-07-02", "valid_to": null}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, next := e.In(t).Must(200, "PATCH", "/v1/credit-limits/"+id, c.body, tag)
			tag = next
			var x, checked credit.Exposure
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				if x, err = h.ExposureOf(ctx, e.DE, e.north, nil); err != nil {
					return err
				}
				checked, err = h.Check(ctx, e.DE, e.north, 30000)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%s %v %v", x.Day.Format(time.DateOnly), x.LimitID != nil && x.LimitID.String() == id, x.Limit != nil); got != fmt.Sprintf("2026-07-01 true %v", c.valid) {
				t.Errorf("exposure: got %s, want 2026-07-01 true %v (day, limit named, limit used)", got, c.valid)
			}
			if got := fmt.Sprintf("%v %v", checked.Limit != nil, checked.Within); got != fmt.Sprintf("%v %v", c.valid, !c.valid) {
				t.Errorf("check of 300.00: got %s, want %v %v (limit used, within)", got, c.valid, !c.valid)
			}
		})
	}
	t.Run("through the API", func(t *testing.T) {
		e.In(t).Must(200, "PATCH", "/v1/credit-limits/"+id, `{"valid_from": "2099-01-01"}`, tag)
		got := e.exposure(t, e.DE, e.north, "&amount=1000000.00")
		if got["credit_limit_id"] != id || got["has_limit"] != false || got["is_within_limit"] != true {
			t.Errorf("got credit_limit_id %v, has_limit %v, within %v; want %s, false, true: the limit starts in the future",
				got["credit_limit_id"], got["has_limit"], got["is_within_limit"], id)
		}
		if _, err := time.Parse(time.DateOnly, fmt.Sprint(got["day"])); err != nil {
			t.Errorf("day: got %v, want a date", got["day"])
		}
	})
}
