package collections_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// lastHeld is an end date a block holds through for every key date of
// the tests' runs but the latest a run may name, whatever today is.
const lastHeld = "2099-12-31"

// blockOf is the JSON of a dunning block of the customer, of line 1 of the
// entry when it is not nil, ending on validTo.
func blockOf(company, customer uuid.UUID, entry *ledger.Entry, validTo string) string {
	item := ""
	if entry != nil {
		item = fmt.Sprintf(`, "fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1`, entry.ID)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q%s, "reason": "Payment plan agreed", "valid_to": %q}`, company, customer, item, validTo)
}

// place places a block and returns its id and entity tag.
func (e env) place(t *testing.T, body string) (id, tag string) {
	t.Helper()
	got, tag := e.Must(201, "POST", "/v1/dunning-blocks", body, "")
	return apitest.String(t, got, "id"), tag
}

// heldBack are a run's blocked items as "reference block", by customer
// and item.
func heldBack(t *testing.T, run map[string]any) string {
	t.Helper()
	var out []string
	for _, b := range objects(t, run, "blocked_items") {
		out = append(out, fmt.Sprintf("%v %v", b["reference"], b["dunning_block_id"]))
	}
	return strings.Join(out, ", ")
}

// TestDunningBlocks (T76e acceptance): a block of an item and a block of a
// customer read back with what they name; a run at a key date up to their
// end date leaves the blocked items out of its notices and totals and
// names them with their blocks, a run after it duns them; a released
// block holds no more, and is released once; a proposal confirmed after a
// block was placed on one of its items is stale. Each change is audited.
func TestDunningBlocks(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	item, itemTag := e.place(t, blockOf(e.DE, e.north, &entries[0], lastHeld))
	whole, _ := e.place(t, blockOf(e.DE, e.south, nil, lastHeld))
	got, _ := e.Must(200, "GET", "/v1/dunning-blocks/"+item, "", "")
	want := "status active, customer_number BP70000002, customer_name Northwind Traders, company_code 2000, fiscal_year 2026, line_no 1, " +
		"document_number " + entries[0].DocumentNumber + ", reason Payment plan agreed, valid_to " + lastHeld + ", version 1"
	if g := fields(t, got, "status", "customer_number", "customer_name", "company_code", "fiscal_year", "line_no", "document_number", "reason", "valid_to",
		"version"); g != want {
		t.Errorf("the item's block: got %s, want %s", g, want)
	}
	if got["journal_entry_id"] != entries[0].ID.String() || got["released_at"] != nil {
		t.Errorf("the item's block names %v, released %v, want %s and not released", got["journal_entry_id"], got["released_at"], entries[0].ID)
	}
	if got, _ := e.Must(200, "GET", "/v1/dunning-blocks/"+whole, "", ""); got["journal_entry_id"] != nil || got["customer_name"] != "Southwind Ltd" {
		t.Errorf("the customer's block: got item %v of %v, want no item of Southwind Ltd", got["journal_entry_id"], got["customer_name"])
	}

	started, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	if g, w := heldBack(t, started), "CI-A "+item+", CI-USD "+whole; g != w {
		t.Errorf("blocked items: got %q, want %q", g, w)
	}
	if g, w := fields(t, started, "notice_count", "item_count", "overdue_amount", "blocked_item_count", "blocked_amount"),
		"notice_count 1, item_count 1, overdue_amount 50.00 EUR, blocked_item_count 2, blocked_amount 260.00 EUR"; g != w {
		t.Errorf("the run: got %s, want %s", g, w)
	}
	usd := objects(t, started, "blocked_items")[1]
	if g, w := fields(t, usd, "customer_id", "customer_number", "days_overdue", "amount", "local_amount"),
		"customer_id "+e.south.String()+", customer_number BP70000001, days_overdue 61, amount 200.00 USD, local_amount 160.00 EUR"; g != w {
		t.Errorf("Southwind's blocked item: got %s, want %s", g, w)
	}
	if g := e.proposed(t, apitest.String(t, started, "id")); g != "CI-B 0->1" {
		t.Errorf("proposed: got %q, want CI-B alone", g)
	}
	t.Run("a run after the end date duns the items", func(t *testing.T) {
		later, _ := e.In(t).Must(201, "POST", "/v1/dunning-runs", run(e.DE, "2100-12-31"), "")
		if g := heldBack(t, later); g != "" {
			t.Errorf("blocked items: got %q, want none", g)
		}
	})

	released, releasedTag := e.Must(200, "POST", "/v1/dunning-blocks/"+item+":release", "", itemTag)
	if g, w := fields(t, released, "status", "released_by", "released_by_name", "version"),
		"status released, released_by "+e.Tenant.Human.ID.String()+", released_by_name human, version 2"; g != w {
		t.Errorf("released: got %s, want %s", g, w)
	}
	e.Problem(409, "collections.block_released", "POST", "/v1/dunning-blocks/"+item+":release", "", releasedTag)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/dunning-blocks/"+whole+":release", "", `"7"`)
	again, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	if g, w := heldBack(t, again), "CI-USD "+whole; g != w {
		t.Errorf("blocked items after the release: got %q, want %q", g, w)
	}

	t.Run("a proposal with an item blocked since is stale", func(t *testing.T) {
		id, tag := e.In(t).start(t, keyDate)
		e.In(t).place(t, blockOf(e.DE, e.north, nil, lastHeld))
		p := e.In(t).Problem(409, "collections.proposal_stale", "POST", "/v1/dunning-runs/"+id+":confirm", "", tag)
		if d, _ := p["detail"].(string); !strings.Contains(d, "of its 2 items, 0 cleared, 0 dunned by another run, 2 blocked from dunning and 0 promised") {
			t.Errorf("detail: got %q, want Northwind's 2 items blocked", d)
		}
	})
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action LIKE 'collections.dunning_block.%'`, []any{e.Tenant.ID}, &audits)
	if audits != 4 {
		t.Errorf("audit records: got %d, want 4: three placed, one released", audits)
	}
}

// TestBlockLists: the blocks are listed the latest first, page by page,
// filtered by company, customer and status.
func TestBlockLists(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	first, tag := e.place(t, blockOf(e.DE, e.north, &entries[0], lastHeld))
	second, _ := e.place(t, blockOf(e.DE, e.south, nil, lastHeld))
	e.Must(200, "POST", "/v1/dunning-blocks/"+first+":release", "", tag)
	page, _ := e.Must(200, "GET", "/v1/dunning-blocks?page_size=1", "", "")
	next := apitest.String(t, page, "next_page_token")
	rest, _ := e.Must(200, "GET", "/v1/dunning-blocks?page_size=1&page_token="+next, "", "")
	if a, b := apitest.String(t, objects(t, page, "items")[0], "id"), apitest.String(t, objects(t, rest, "items")[0], "id"); a != second || b != first {
		t.Errorf("pages of one: got %s then %s, want %s then %s", a, b, second, first)
	}
	for _, c := range []struct{ name, query, want string }{
		{"all", "", second + " " + first},
		{"the company's", "?legal_entity_id=" + e.DE.String(), second + " " + first},
		{"Northwind's", "?customer_id=" + e.north.String(), first},
		{"active", "?status=active", second},
		{"released", "?status=released", first},
		{"a status no block has", "?status=expired", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/dunning-blocks"+c.query, "", "")
			var ids []string
			for _, b := range objects(t, page, "items") {
				ids = append(ids, apitest.String(t, b, "id"))
			}
			if g := strings.Join(ids, " "); g != c.want {
				t.Errorf("got %s, want %s", g, c.want)
			}
		})
	}
}

// TestBlockRefusals: a reason of white space, an end date before today or
// after the latest key date, a customer that is no business partner, an
// item that is no open item the customer owes, a journal line named in
// part and a company the caller may not act in are refused, each naming
// its field; an unknown block is not found.
func TestBlockRefusals(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	body := blockOf(e.DE, e.north, &entries[0], lastHeld)
	for _, c := range []struct {
		name, body, code, field string
	}{
		{"a reason of white space", strings.Replace(body, "Payment plan agreed", "  ", 1), "request.validation_failed", "reason"},
		{"an end date before today", strings.Replace(body, lastHeld, "2000-01-01", 1), "collections.invalid_block_end", "valid_to"},
		{"an end date after the latest key date", strings.Replace(body, lastHeld, "2101-01-01", 1), "collections.invalid_block_end", "valid_to"},
		{"a customer that is no business partner", strings.Replace(body, e.north.String(), uuid.NewString(), 1), "request.reference_not_found",
			"customer_id"},
		{"a credit item", blockOf(e.DE, e.north, &entries[5], lastHeld), "collections.item_not_open", "journal_entry_id"},
		{"another customer's item", blockOf(e.DE, e.north, &entries[7], lastHeld), "collections.item_not_open", "journal_entry_id"},
		{"a journal line in part", strings.Replace(body, `, "line_no": 1`, "", 1), "request.validation_failed", "journal_entry_id"},
		{"a company the caller may not act in", strings.Replace(body, e.DE.String(), uuid.NewString(), 1), "request.reference_not_found",
			"legal_entity_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(422, c.code, "POST", "/v1/dunning-blocks", c.body, "")
			if !strings.Contains(mustJSON(t, p["errors"]), `"field":"`+c.field+`"`) {
				t.Errorf("errors: got %v, want one naming %s", p["errors"], c.field)
			}
		})
	}
	e.Problem(404, "request.not_found", "GET", "/v1/dunning-blocks/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "POST", "/v1/dunning-blocks/"+uuid.NewString()+":release", "", `"1"`)
}

// badZone is a configuration whose company 2000 has a time zone the
// binary cannot load.
type badZone struct{ collections.Settings }

func (b badZone) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, v, err := b.Settings.Document(ctx)
	if err == nil {
		doc.Company("2000").TimeZone = "Mars/Olympus_Mons"
	}
	return doc, v, err
}

// TestBlockEndAgainstToday: the end date is judged against the company's
// today by the endpoints' clock: the day before is refused naming today,
// today itself is accepted; a zone that does not load fails, as no
// refusal.
func TestBlockEndAgainstToday(t *testing.T) {
	e := setup(t)
	e.story(t)
	in := collections.BlockInput{LegalEntityID: e.DE, CustomerID: e.north, Reason: "Payment plan agreed"}
	place := func(h *collections.Endpoints, validTo string) error {
		return e.inRequest(t, func(ctx context.Context) error {
			in.ValidTo = date(t, validTo)
			_, err := h.PlaceBlock(ctx, in)
			return err
		})
	}
	h := endpoints(t)
	h.Now = func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }
	if p := problem.From(place(h, "2026-06-30")); p.Code != "collections.invalid_block_end" || !strings.Contains(p.Detail, "2026-07-01") {
		t.Errorf("the day before today: got %v, want collections.invalid_block_end naming 2026-07-01", p)
	}
	if err := place(h, "2026-07-01"); err != nil {
		t.Errorf("today: got %v, want the block placed", err)
	}
	h.Settings = badZone{h.Settings}
	err := place(h, "2026-07-01")
	var p *problem.Problem
	if err == nil || errors.As(err, &p) {
		t.Errorf("a zone that does not load: got %v, want an error that is no refusal", err)
	}
}
