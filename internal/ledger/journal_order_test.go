package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	orders "github.com/mustie7iy/agentic-erp/internal/controlling"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Costs posted to internal orders (T60b, ADR-0064 §2): a primary cost line
// names a cost center or an order; the order is the controlling
// context's, asked through ledger.CostObjects; the line takes its profit
// center and segment from the order's responsible cost center.

// internalOrder writes an order of the company under the responsible cost
// center, as the controlling context keeps it, released or not, and
// returns its id.
func (a assigned) internalOrder(t *testing.T, le uuid.UUID, code string, costCenter uuid.UUID, released bool) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO controlling.internal_order (tenant_id, id, legal_entity_id, code, name, responsible_cost_center_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'Order', $5, $6, $6)`, a.ten.ID, id, le, code, costCenter, a.ten.Human.ID)
	if released {
		pgtest.Exec(t, `UPDATE controlling.internal_order SET status = 'released', version = version + 1 WHERE id = $1`, id)
	}
	return id.String()
}

// orderOf is a line's internal order as "id:code", "-" when it has none.
func orderOf(v any) string {
	l, _ := v.(map[string]any)
	if id, ok := l["internal_order_id"]; ok {
		return fmt.Sprintf("%v:%v", id, l["internal_order_code"])
	}
	return "-"
}

// wantOrderLines checks an entry's lines: each one's centers and order.
func wantOrderLines(t *testing.T, entry map[string]any, want ...string) {
	t.Helper()
	lines := apitest.List(t, entry, "lines")
	if len(lines) != len(want) {
		t.Fatalf("lines: got %d, want %d", len(lines), len(want))
	}
	for i, l := range lines {
		if got := centersOf(l) + " " + orderOf(l); got != want[i] {
			t.Errorf("line %d: got %s, want %s", i+1, got, want[i])
		}
	}
}

func TestJournalInternalOrder(t *testing.T) {
	a := setupAssigned(t)
	pc1, seg1, dummy := a.pc1.String(), a.seg1.String(), a.dummy.String()
	fair := a.internalOrder(t, a.de, "FAIR", a.cc1000, true)
	sales := a.internalOrder(t, a.de, "SALES", a.cc2000, true)
	ap := func(amount string) centeredLine {
		return centeredLine{account: a.ap.String(), amount: amount, partner: a.supplier.String()}
	}
	post := func(date string, l centeredLine) string { return centeredEntry(a.de, date, l, ap("-"+l.amount)) }
	onFair := centersAre("", "", pc1, "PC1", seg1, "SEG1") + " " + fair + ":FAIR"
	posted, tag := a.Must(201, "POST", "/v1/journal-entries", post("2026-05-10", centeredLine{account: a.office.String(), amount: "7.00", order: fair}), "")

	t.Run("a line posted to an order", func(t *testing.T) {
		// 520000 has a default cost center, which the order's line does not
		// take: its profit center and segment are the responsible center's.
		wantOrderLines(t, posted, onFair, noCenters()+" -")
		got, _ := a.in(t).Must(200, "GET", "/v1/journal-entries/"+posted["id"].(string), "", "")
		wantOrderLines(t, got, onFair, noCenters()+" -")
		page, _ := a.in(t).Must(200, "GET", "/v1/journal-entries?legal_entity_id="+a.de.String()+"&fiscal_year=2026", "", "")
		wantOrderLines(t, apitest.List(t, page, "items")[0].(map[string]any), onFair, noCenters()+" -")
	})
	t.Run("an order whose cost center has no profit center takes the default", func(t *testing.T) {
		// Cost center 2000 is locked for costs in February: the lock is the
		// center's own postings', not its orders'.
		got, _ := a.in(t).Must(201, "POST", "/v1/journal-entries", post("2026-02-10", centeredLine{account: a.it.String(), amount: "1.00", order: sales}), "")
		wantOrderLines(t, got, centersAre("", "", dummy, "DUMMY", "", "")+" "+sales+":SALES", noCenters()+" -")
	})
	t.Run("every ledger's copy carries the order", func(t *testing.T) {
		var lines int
		pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_line l JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year
				AND e.id = l.journal_entry_id
			WHERE l.tenant_id = $1 AND e.posting_group_id = $2 AND l.internal_order_id = $3 AND l.cost_center_id IS NULL`,
			[]any{a.ten.ID, posted["posting_group_id"], fair}, &lines)
		if lines != 2 {
			t.Errorf("lines of ledgers 0L and 2L on the order: got %d, want 2", lines)
		}
	})
	t.Run("the event carries the order", func(t *testing.T) {
		var got []any
		for _, ev := range events(t, a.ten, "journal_entry_posted") {
			if ev["journal_entry_id"] == posted["id"] {
				for _, l := range ev["lines"].([]any) {
					got = append(got, l.(map[string]any)["internal_order_id"])
				}
			}
		}
		if fmt.Sprint(got) != fmt.Sprint([]any{fair, nil}) {
			t.Errorf("event lines' orders: got %v, want [%s <nil>]", got, fair)
		}
	})
	t.Run("a reversal copies the order", func(t *testing.T) {
		// Released orders stay so, and a reversal is not checked again.
		rev, _ := a.in(t).Must(200, "POST", "/v1/journal-entries/"+posted["id"].(string)+":reverse", "", tag)
		wantOrderLines(t, rev, onFair, noCenters()+" -")
	})
	t.Run("refusals", func(t *testing.T) { a.in(t).orderRefusals(t, fair) })
}

// orderRefusals are the orders a posting refuses, each naming the line and
// why.
func (a assigned) orderRefusals(t *testing.T, fair string) {
	t.Helper()
	created := a.internalOrder(t, a.de, "CREATED", a.cc1000, false)
	other := a.internalOrder(t, a.us, "US", a.ccUS, true)
	later := a.internalOrder(t, a.de, "LATER", a.cc3000, true) // its cost center is valid from June
	lost := a.internalOrder(t, a.de, "LOST", uuid.New(), true) // its cost center is none of the ledger's
	closed := a.internalOrder(t, a.de, "CLOSED", a.cc1000, true)
	pgtest.Exec(t, `UPDATE controlling.internal_order SET status = 'closed', version = version + 1 WHERE id = $1`, closed)
	office, cc1000 := a.office.String(), a.cc1000.String()
	for name, c := range map[string]struct {
		line centeredLine
		code string
		want string
	}{
		"an order that is none":           {centeredLine{account: office, order: uuid.NewString()}, "ledger.internal_order_invalid", "unknown"},
		"an order of another company":     {centeredLine{account: office, order: other}, "ledger.internal_order_invalid", "not_in_company"},
		"an order not released":           {centeredLine{account: office, order: created}, "ledger.internal_order_invalid", "not_released"},
		"an order closed":                 {centeredLine{account: office, order: closed}, "ledger.internal_order_invalid", "closed"},
		"an order beside a cost center":   {centeredLine{account: office, order: fair, cc: cc1000}, "ledger.internal_order_invalid", "with_cost_center"},
		"an order on a revenue account":   {centeredLine{account: a.revenue.String(), order: fair}, "ledger.internal_order_invalid", "not_a_cost_account"},
		"an order on a balance sheet one": {centeredLine{account: a.bank.String(), order: fair}, "ledger.internal_order_invalid", "not_a_cost_account"},
		"a cost center not yet valid":     {centeredLine{account: office, order: later}, "ledger.internal_order_invalid", "cost_center_not_valid_on_date"},
		"a cost center the ledger lacks":  {centeredLine{account: office, order: lost}, "ledger.internal_order_invalid", "cost_center_not_valid_on_date"},
	} {
		t.Run(name, func(t *testing.T) {
			c.line.amount = "1.00"
			m := a.in(t).Problem(422, c.code, "POST", "/v1/journal-entries", centeredEntry(a.de, "2026-05-10", c.line,
				centeredLine{account: a.ap.String(), amount: "-1.00", partner: a.supplier.String()}), "")
			errs, _ := m["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("field errors: got %v, want one", m["errors"])
			}
			e, _ := errs[0].(map[string]any)
			if got := fmt.Sprint(e["field"], " ", e["code"]); got != "lines[0].internal_order_id "+c.want {
				t.Errorf("field error: got %s, want lines[0].internal_order_id %s", got, c.want)
			}
		})
	}
	body := centeredEntry(a.de, "2026-05-10", centeredLine{account: office, amount: "1.00", order: fair, pc: a.dummy.String()},
		centeredLine{account: a.ap.String(), amount: "-1.00", partner: a.supplier.String()})
	a.Problem(422, "ledger.profit_center_mismatch", "POST", "/v1/journal-entries", body, "")
}

// TestInternalOrderNotParked: a parked entry and an entry template keep no
// order in the first version, so one naming an order is refused instead
// of losing it.
func TestInternalOrderNotParked(t *testing.T) {
	a := setupAssigned(t)
	fair := a.internalOrder(t, a.de, "FAIR", a.cc1000, true)
	body := centeredEntry(a.de, "2026-05-10", centeredLine{account: a.office.String(), amount: "1.00", order: fair},
		centeredLine{account: a.bank.String(), amount: "-1.00"})
	a.Problem(422, "ledger.internal_order_not_parkable", "POST", "/v1/parked-journal-entries", body, "")
	template := fmt.Sprintf(`{"legal_entity_id": %q, "name": "Fair", "currency": "EUR", "lines": [
		{"account_id": %q, "amount": {"amount": "1.00", "currency": "EUR"}, "internal_order_id": %q},
		{"account_id": %q, "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, a.de, a.office, fair, a.bank)
	a.Problem(422, "ledger.internal_order_not_parkable", "POST", "/v1/entry-templates", template, "")
}

// failingOrders is a controlling context that cannot answer.
type failingOrders struct{}

func (failingOrders) Orders(context.Context, []uuid.UUID, bool) (map[uuid.UUID]ledger.CostOrder, error) {
	return nil, errors.New("controlling is away")
}

// TestInternalOrderPort: a line naming an order is never posted unchecked:
// a handler wired without the port, and a port that fails, are
// errors; and the database's check is the application's problem.
func TestInternalOrderPort(t *testing.T) {
	a := setupAssigned(t)
	fair := uuid.MustParse(a.internalOrder(t, a.de, "FAIR", a.cc1000, true))
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	entry := &gen.JournalEntryCreate{LegalEntityID: a.de, DocumentDate: day, PostingDate: day, Currency: "EUR", Lines: []gen.JournalLineInput{
		{AccountID: a.office, Amount: gen.Money{Amount: "1.00", Currency: "EUR"}, InternalOrderID: gen.NewOptUUID(fair)},
		{AccountID: a.bank, Amount: gen.Money{Amount: "-1.00", Currency: "EUR"}}}}
	for name, c := range map[string]struct {
		port ledger.CostObjects
		want string
	}{
		"no port wired":   {nil, "cost objects port is not wired"},
		"a port in error": {failingOrders{}, "controlling is away"},
	} {
		t.Run(name, func(t *testing.T) {
			h := endpoints()
			h.CostObjects = c.port
			err := apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) error {
				_, err := h.PostJournalEntry(ctx, entry, gen.PostJournalEntryParams{})
				return err
			})
			if err == nil {
				t.Fatal("got no error, want an internal one")
			}
			if got := problem.From(err).Status; got != 500 {
				t.Errorf("status: got %d, want 500", got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error: got %v, want one saying %q", err, c.want)
			}
		})
	}
	t.Run("the database's check", func(t *testing.T) {
		p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "journal_line_one_cost_object"})
		if p.Status != 422 || p.Code != "ledger.internal_order_invalid" {
			t.Errorf("problem: got %d %s, want 422 ledger.internal_order_invalid", p.Status, p.Code)
		}
	})
}

// TestReversalOnAClosedOrder: a closed order takes no posting, a
// reversal's included (T60e): the entry that charged it is not reversed
// once the order is closed, and an entry of an open order still is.
func TestReversalOnAClosedOrder(t *testing.T) {
	a := setupAssigned(t)
	fair := a.internalOrder(t, a.de, "FAIR", a.cc1000, true)
	charge := func() (string, string) {
		posted, tag := a.Must(201, "POST", "/v1/journal-entries", centeredEntry(a.de, "2026-05-10",
			centeredLine{account: a.office.String(), amount: "1.00", order: fair}, centeredLine{account: a.bank.String(), amount: "-1.00"}), "")
		return "/v1/journal-entries/" + posted["id"].(string) + ":reverse", tag
	}
	first, firstTag := charge()
	second, secondTag := charge()
	a.Must(200, "POST", first, "", firstTag)
	pgtest.Exec(t, `UPDATE controlling.internal_order SET status = 'closed', version = version + 1 WHERE id = $1`, fair)
	t.Run("a port in error is the reversal's", func(t *testing.T) {
		h := endpoints()
		h.CostObjects = failingOrders{}
		err := apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.OptJournalEntryReverse{},
				gen.ReverseJournalEntryParams{ID: uuid.MustParse(second[len("/v1/journal-entries/") : len(second)-len(":reverse")]), IfMatch: secondTag})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "controlling is away") {
			t.Errorf("got %v, want the port's error", err)
		}
	})
	m := a.Problem(409, "ledger.internal_order_closed", "POST", second, "", secondTag)
	if want := "The entry posted to internal order FAIR, which is closed since and takes no posting; the entry cannot be reversed."; m["detail"] != want {
		t.Errorf("detail: got %v, want %s", m["detail"], want)
	}
}

// TestPostingToAnInternalOrder: another context's posting names an order
// on its line (T60e: the controlling context's settlement), checked as a
// manual entry's is through the port its Postings are wired with; without
// the port the line is an error, never posted unchecked.
func TestPostingToAnInternalOrder(t *testing.T) {
	a := setupAssigned(t)
	fair := uuid.MustParse(a.internalOrder(t, a.de, "FAIR", a.cc1000, true))
	created := uuid.MustParse(a.internalOrder(t, a.de, "CREATED", a.cc1000, false))
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	post := func(p ledger.Postings, order uuid.UUID) (entry ledger.Entry, err error) {
		return entry, apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
			entry, err = p.Post(ctx, ledger.Posting{LegalEntityID: a.de, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "EUR",
				SourceContext: "controlling", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					{AccountID: a.office, Amount: 100, InternalOrderID: &order}, {AccountID: a.bank, Amount: -100}}})
			return err
		})
	}
	wired := postings()
	wired.CostObjects = orders.CostObjects{}
	entry, err := post(wired, fair)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := a.Must(200, "GET", "/v1/journal-entries/"+entry.ID.String(), "", "")
	wantOrderLines(t, got, fmt.Sprintf("-/%s:PC1/%s:SEG1 %s:FAIR", a.pc1, a.seg1, fair), "-/-/- -")
	if got["source_context"] != "controlling" {
		t.Errorf("source context: got %v, want controlling", got["source_context"])
	}
	if _, err := post(wired, created); problem.From(err).Code != "ledger.internal_order_invalid" {
		t.Errorf("an order not released: got %v, want ledger.internal_order_invalid", err)
	}
	if _, err := post(postings(), fair); err == nil || !strings.Contains(err.Error(), "cost objects port is not wired") {
		t.Errorf("postings without the port: got %v, want an error saying the port is not wired", err)
	}
}

// TestInternalOrderNamedByPort: reading an entry whose line names an order
// asks the controlling context for its code; a reader wired without the
// port fails rather than show the line without it.
func TestInternalOrderNamedByPort(t *testing.T) {
	a := setupAssigned(t)
	fair := a.internalOrder(t, a.de, "FAIR", a.cc1000, true)
	posted, _ := a.Must(201, "POST", "/v1/journal-entries", centeredEntry(a.de, "2026-05-10",
		centeredLine{account: a.office.String(), amount: "1.00", order: fair}, centeredLine{account: a.bank.String(), amount: "-1.00"}), "")
	h := endpoints()
	h.CostObjects = nil
	err := apitest.InRequest(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		_, err := h.GetJournalEntry(ctx, gen.GetJournalEntryParams{ID: uuid.MustParse(posted["id"].(string))})
		return err
	})
	if err == nil || problem.From(err).Status != 500 {
		t.Errorf("a reader without the port: got %v, want an internal error", err)
	}
	// An order the controlling context no longer knows is shown by id alone.
	pgtest.Exec(t, `DELETE FROM controlling.internal_order WHERE id = $1`, fair)
	got, _ := a.Must(200, "GET", "/v1/journal-entries/"+posted["id"].(string), "", "")
	if o := orderOf(apitest.List(t, got, "lines")[0]); o != fair+":<nil>" {
		t.Errorf("the line's order: got %s, want %s without a code", o, fair)
	}
}
