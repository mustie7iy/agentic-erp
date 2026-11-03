package cash_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// setupSnapshot is the drill-down's Maple Inc (setupDrill) with two more
// supplier items paid after the document level's last day, 17 May: CAD
// -200.00 due Tuesday 19 May (in the week of 15 to 21 May, which the
// document level ends in) and CAD -600.00 due Tuesday 9 June (the week of
// 5 to 11 June), and a CAD 100.00 credit memo netted into the payment of
// 30 April; and a run of 22 April whose CAD 300.00 payment settles two
// items, kept as one line of the run; its people may take snapshots.
func setupSnapshot(t *testing.T) drillEnv {
	t.Helper()
	e := setupDrill(t)
	e.Tenant.Grant(t, "cash.snapshot.take", e.Tenant.Human)
	e.docs["may"] = e.item(t, e.Supplier, "2026-04-19", "CAD", -20000, &terms{baseline: "2026-04-19", net: 30})
	e.docs["june"] = e.item(t, e.Supplier, "2026-05-10", "CAD", -60000, &terms{baseline: "2026-05-10", net: 30})
	e.docs["credit"] = e.item(t, e.Supplier, "2026-04-15", "CAD", 10000, nil)
	e.docs["run2"] = e.run(t, e.main, "2026-04-22", "CAD", "proposed")
	pay := e.payment(t, e.docs["run2"], 30000, "CAD", nil)
	for _, amount := range []int64{20000, 10000} {
		held := e.item(t, e.Supplier, "2026-04-02", "CAD", -amount, nil)
		e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
			VALUES ($1, $2, 2026, $3, 1, $4, '2026-05-02')`, e.Tenant.ID, pay, held, amount)
	}
	return e
}

// take takes a snapshot on demand as the human.
func (e drillEnv) take(t *testing.T, name string) map[string]any {
	t.Helper()
	s, _ := e.Must(201, "POST", "/v1/cash-snapshots", fmt.Sprintf(`{"legal_entity_id": %q, "name": %q}`, e.le, name), "")
	return s
}

// linesOf renders a page of a snapshot's lines as "date[..end] source/
// certainty item amount xcount: supplier; document (has)" lines, and checks
// that their numbers ascend. (Flows of one day are ordered by a digest
// that holds ids, so the numbers themselves differ between runs.)
func (e drillEnv) linesOf(t *testing.T, p map[string]any) []string {
	t.Helper()
	last := 0.0
	for _, it := range p["items"].([]any) {
		n := it.(map[string]any)["line_no"].(float64)
		if n <= last {
			t.Errorf("line %v after line %v, want ascending", n, last)
		}
		last = n
	}
	names := map[string]string{}
	for name, id := range e.docs {
		names[id.String()] = name
	}
	var out []string
	for _, it := range p["items"].([]any) {
		l := it.(map[string]any)
		day := fmt.Sprint(l["value_date"])
		if end, ok := l["bucket_end"]; ok {
			day += ".." + fmt.Sprint(end)
		}
		supplier, doc := "-", "-"
		if s, ok := l["supplier"].(map[string]any); ok {
			supplier = fmt.Sprint(s["number"])
		}
		if d, ok := l["document"].(map[string]any); ok {
			doc = fmt.Sprint(d["kind"], " ", names[fmt.Sprint(d["id"])])
		}
		_, payment := l["payment_id"]
		out = append(out, fmt.Sprintf("%s %s/%s late=%v %v %s x%v bank=%v: %s; %s has=%v payment=%v", day, l["source"],
			l["certainty"], l["late"], l["liquidity_item"], amount(l["amount"]), l["flow_count"], l["house_bank_code"], supplier, doc, l["has_document"], payment))
	}
	return out
}

// TestCashSnapshotOnDemand: a person takes a named snapshot of Maple
// Inc's forecast (ADR-0051 §6, T53f): its flows up to the last bucket per
// document and item within 31 days, summed per bucket beyond, its totals
// per currency the forecast's (blocked suppliers' apart), audited and
// published; it is listed and read again with its lines.
func TestCashSnapshotOnDemand(t *testing.T) {
	e := setupSnapshot(t)
	f := e.forecastOf(t, "")
	s := e.take(t, "Board meeting April")
	want(t, "header", fmt.Sprint(s["kind"], " ", s["name"], " ", s["snapshot_date"], " ", s["time_zone"], " ", s["horizon_end"], " ", s["document_level_until"]),
		"on_demand Board meeting April 2026-04-17 UTC 2027-04-16 2026-05-17")
	var totals []string
	for _, c := range s["currencies"].([]any) {
		cm := c.(map[string]any)
		totals = append(totals, fmt.Sprint(cm["currency"], " ", amount(cm["total"]), " blocked ", amount(cm["blocked"]), " lines ", cm["line_count"]))
	}
	var forecast []string
	for _, c := range f["currencies"].([]any) {
		cm := c.(map[string]any)
		forecast = append(forecast, fmt.Sprint(cm["currency"], " ", amount(cm["total"])))
	}
	want(t, "the forecast's totals", forecast, []string{"CAD -1150.00 CAD", "USD -300.04 USD"})
	want(t, "the snapshot's totals", totals, []string{"CAD -1150.00 CAD blocked 0.00 CAD lines 13", "USD -300.04 USD blocked 0.00 USD lines 3"})
	id := s["id"].(string)
	p, _ := e.Must(200, "GET", "/v1/cash-snapshots/"+id+"/flows?currency=CAD", "", "")
	want(t, "lines", e.linesOf(t, p), []string{
		"2026-04-17 payment_run/confirmed late=false OP_SUPPLIERS -800.00 CAD x1 bank=MAIN: BP90000001; payment_run run has=true payment=true",
		"2026-04-22 payment_run/confirmed late=false OP_SUPPLIERS -300.00 CAD x1 bank=MAIN: BP90000001; payment_run run2 has=true payment=true",
		"2026-04-23 memo/expected late=false REC_CUSTOMERS 2000.00 CAD x1 bank=MAIN: -; memo_record receipt has=true payment=false",
		"2026-04-24 memo/expected late=false REC_CUSTOMERS 100.00 CAD x1 bank=<nil>: -; memo_record series has=true payment=false",
		"2026-04-28 down_payment/expected late=false OP_SUPPLIERS -250.00 CAD x1 bank=<nil>: BP90000001; down_payment_request request has=true payment=false",
		"2026-04-30 payable/expected late=false OP_SUPPLIERS 100.00 CAD x1 bank=MAIN: BP90000001; journal_entry credit has=true payment=false",
		"2026-04-30 payable/expected late=false OP_SUPPLIERS -1000.00 CAD x1 bank=MAIN: BP90000001; journal_entry item has=true payment=false",
		"2026-05-12 memo/planned late=false OP_SUPPLIERS -500.00 CAD x1 bank=<nil>: -; memo_record  has=true payment=false",
		"2026-05-18..2026-05-21 payable/expected late=false OP_SUPPLIERS -200.00 CAD x1 bank=MAIN: -; - has=false payment=false",
		"2026-06-05..2026-06-11 payable/expected late=false OP_SUPPLIERS -600.00 CAD x1 bank=MAIN: -; - has=false payment=false",
		"2026-07-17..2026-08-16 memo/expected late=false REC_CUSTOMERS 100.00 CAD x1 bank=<nil>: -; - has=false payment=false",
		"2026-10-17..2026-11-16 memo/expected late=false REC_CUSTOMERS 100.00 CAD x1 bank=<nil>: -; - has=false payment=false",
		"2027-01-17..2027-02-16 memo/expected late=false REC_CUSTOMERS 100.00 CAD x1 bank=<nil>: -; - has=false payment=false",
	})
	want(t, "page", fmt.Sprint(amount(p["total"]), " ", p["count"]), "-1150.00 CAD 13")
	items := p["items"].([]any)
	want(t, "the credit memo's and the invoice's lines one flow", items[5].(map[string]any)["flow_no"], items[6].(map[string]any)["flow_no"])
	got, _ := e.Must(200, "GET", "/v1/cash-snapshots/"+id, "", "")
	want(t, "read again", fmt.Sprint(got["id"], got["currencies"]), fmt.Sprint(s["id"], s["currencies"]))
	l, _ := e.Must(200, "GET", "/v1/cash-snapshots?legal_entity_id="+e.le.String(), "", "")
	want(t, "listed", len(l["items"].([]any)), 1)
	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "cash_snapshot_taken"})
	want(t, "published", fmt.Sprint(len(events), " ", events[0]["kind"], " ", events[0]["line_count"]), "1 on_demand 16")
	var actions int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'cash.snapshot.take' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &actions)
	want(t, "audited", actions, 1)
}

// TestCashSnapshotDaily: the daily snapshot of a date is taken once; taken
// again it finds the first and writes nothing; one whose date the company
// is no longer on is skipped, as is one of no legal entity; it is due once
// the company's local time reaches its snapshot time and until it is
// taken. (The workflow's run-twice test commits each run:
// workflows.TestTakeSnapshotTwice.)
func TestCashSnapshotDaily(t *testing.T) {
	e := setupSnapshot(t)
	c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	c.Consistent = inRequest{}
	day := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	err := apitest.InRequest(t, e.Tenant, pgtest.Actor{ID: cash.SnapshotterActor}, func(ctx context.Context) error {
		due, err := c.DueSnapshots(ctx, readAt)
		want(t, "due at 15:00, after 07:00", due[e.le], day)
		early, _ := c.DueSnapshots(ctx, time.Date(2026, 4, 17, 6, 59, 0, 0, time.UTC))
		_, found := early[e.le]
		want(t, "not due before 07:00", found, false)
		for i, wanted := range []string{cash.SnapshotTaken, cash.SnapshotExists} {
			got, _ := c.TakeDailySnapshot(ctx, e.le, day)
			want(t, fmt.Sprintf("run %d", i+1), got, wanted)
		}
		got, _ := c.TakeDailySnapshot(ctx, e.le, day.AddDate(0, 0, -1))
		want(t, "a date past", got, cash.SnapshotSkipped)
		got, _ = c.TakeDailySnapshot(ctx, uuid.New(), day)
		want(t, "no such legal entity", got, cash.SnapshotSkipped)
		due, _ = c.DueSnapshots(ctx, readAt)
		_, found = due[e.le]
		want(t, "taken: not due", found, false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// inRequest is a consistent read in the request's own transaction, which
// the read does not end: what the tests inject failures into.
type inRequest struct{}

func (inRequest) Read(ctx context.Context) (context.Context, pgx.Tx, error) {
	tx, _, err := reqctx.Request(ctx)
	return ctx, kept{tx}, err
}

// kept is a transaction its reader's rollback leaves open.
type kept struct{ pgx.Tx }

func (kept) Rollback(context.Context) error { return nil }

// raced is inRequest after another replica committed the company's daily
// snapshot of the date.
type raced struct {
	t      *testing.T
	tenant uuid.UUID
	le     uuid.UUID
}

func (r raced) Read(ctx context.Context) (context.Context, pgx.Tx, error) {
	pgtest.Exec(r.t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, 'daily', '2026-04-17', 'UTC', '2027-04-16', 1, $4)`, r.tenant, uuid.Must(uuid.NewV7()), r.le, cash.SnapshotterActor)
	return inRequest{}.Read(ctx)
}

// TestCashSnapshotLines: a snapshot's lines by liquidity item, what no rule
// assigned, source and value dates, in pages; refusals; and a caller who
// may read the forecast alone sees the lines without their suppliers,
// payments and documents, still told a line names one.
func TestCashSnapshotLines(t *testing.T) {
	e := setupSnapshot(t)
	id := e.take(t, "Board")["id"].(string)
	lines := func(query string) map[string]any {
		t.Helper()
		p, _ := e.Must(200, "GET", "/v1/cash-snapshots/"+id+"/flows?currency=CAD"+query, "", "")
		return p
	}
	for _, c := range []struct{ query, want string }{
		{"&liquidity_item=REC_CUSTOMERS", "2400.00 CAD 5"},
		{"&unclassified=true", "0.00 CAD 0"},
		{"&source=payable", "-1700.00 CAD 4"},
		{"&from=2026-04-24&to=2026-04-30", "-1050.00 CAD 4"},
		{"&liquidity_item=NOPE", "0.00 CAD 0"},
	} {
		t.Run(c.query, func(t *testing.T) {
			p := lines(c.query)
			want(t, "total and count", fmt.Sprint(amount(p["total"]), " ", p["count"]), c.want)
		})
	}
	t.Run("pages", func(t *testing.T) {
		var got []string
		query := "&page_size=5"
		for page := 1; ; page++ {
			p := lines(query)
			got = append(got, e.linesOf(t, p)...)
			next, ok := p["next_page_token"].(string)
			if !ok {
				break
			}
			query = "&page_size=5&page_token=" + next
		}
		want(t, "every line once", fmt.Sprint(len(got), " ", got), fmt.Sprint(13, " ", e.linesOf(t, lines("&page_size=50"))))
		e.Problem(400, "request.invalid_page_token", "GET", "/v1/cash-snapshots/"+id+"/flows?currency=USD&page_token="+
			lines("&page_size=1")["next_page_token"].(string), "", "")
	})
	t.Run("a currency it has no line in", func(t *testing.T) {
		for cur, want0 := range map[string]string{"EUR": "0.00 EUR 0", "XYZ": "0.00 XYZ 0"} {
			p, _ := e.Must(200, "GET", "/v1/cash-snapshots/"+id+"/flows?currency="+cur, "", "")
			want(t, cur, fmt.Sprint(amount(p["total"]), " ", p["count"]), want0)
		}
	})
	e.Problem(422, "request.validation_failed", "GET", "/v1/cash-snapshots/"+id+"/flows?currency=CAD&liquidity_item=OPERATING&unclassified=true", "", "")
	e.Problem(422, "request.validation_failed", "GET", "/v1/cash-snapshots/"+id+"/flows?currency=CAD&from=2026-05-01&to=2026-04-30", "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/cash-snapshots/"+uuid.NewString()+"/flows?currency=CAD", "", "")
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", "/v1/cash-snapshots/"+id, "", "")
	l, _ := e.As(other).Must(200, "GET", "/v1/cash-snapshots", "", "")
	want(t, "listed to a reader of another company", len(l["items"].([]any)), 0)
	t.Run("the forecast alone", func(t *testing.T) {
		for _, p := range []string{"partner.supplier.read", "ledger.entry.read", "payables.invoice.read", "payments.run.read", "cash.memo.read"} {
			e.Tenant.Revoke(t, p, e.Tenant.Human)
		}
		got := e.linesOf(t, lines("&to=2026-04-17"))
		want(t, "lines", got, []string{"2026-04-17 payment_run/confirmed late=false OP_SUPPLIERS -800.00 CAD x1 bank=MAIN: -; - has=true payment=false"})
	})
}

// TestCashSnapshotList: the snapshots newest first, by legal entity, kind
// and dates, in pages.
func TestCashSnapshotList(t *testing.T) {
	e := setupSnapshot(t)
	first, second := e.take(t, "First")["id"], e.take(t, "Second")["id"]
	pgtest.Exec(t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, 'daily', '2026-04-16', 'UTC', '2027-04-15', 1, $4)`, e.Tenant.ID, uuid.Must(uuid.NewV7()), e.le, cash.SnapshotterActor)
	names := func(query string) []string {
		t.Helper()
		p, _ := e.Must(200, "GET", "/v1/cash-snapshots?"+query, "", "")
		var out []string
		for _, it := range p["items"].([]any) {
			s := it.(map[string]any)
			out = append(out, fmt.Sprint(s["snapshot_date"], " ", s["kind"], " ", s["name"]))
		}
		return out
	}
	want(t, "newest first", names("legal_entity_id="+e.le.String()), []string{"2026-04-17 on_demand Second", "2026-04-17 on_demand First", "2026-04-16 daily <nil>"})
	want(t, "daily", names("kind=daily"), []string{"2026-04-16 daily <nil>"})
	want(t, "dates", names("from=2026-04-17&to=2026-04-17"), []string{"2026-04-17 on_demand Second", "2026-04-17 on_demand First"})
	p, _ := e.Must(200, "GET", "/v1/cash-snapshots?page_size=1", "", "")
	want(t, "first page", p["items"].([]any)[0].(map[string]any)["id"], second)
	p, _ = e.Must(200, "GET", "/v1/cash-snapshots?page_size=1&page_token="+p["next_page_token"].(string), "", "")
	want(t, "second page", p["items"].([]any)[0].(map[string]any)["id"], first)
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/cash-snapshots?kind=daily&page_token="+p["next_page_token"].(string), "", "")
}

// TestCashSnapshotDailyRace: two replicas take a company's daily snapshot
// of a date at once; the one whose write comes second finds the first's
// and writes nothing (the database keeps one per company and date).
func TestCashSnapshotDailyRace(t *testing.T) {
	e := setupSnapshot(t)
	c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	c.Consistent = raced{t: t, tenant: e.Tenant.ID, le: e.le}
	err := apitest.InRequest(t, e.Tenant, pgtest.Actor{ID: cash.SnapshotterActor}, func(ctx context.Context) error {
		got, err := c.TakeDailySnapshot(ctx, e.le, time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC))
		want(t, "the second", got, cash.SnapshotExists)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCashSnapshotRefusals: a snapshot of a legal entity the caller cannot
// act in, or that does not exist, is not taken; one whose sums are out of
// the range of an amount is refused, as the forecast is.
func TestCashSnapshotRefusals(t *testing.T) {
	e := setupSnapshot(t)
	other := e.Reader(t, "reader of 1000", e.US)
	e.Tenant.Grant(t, "cash.snapshot.take", other)
	body := func(le uuid.UUID) string { return fmt.Sprintf(`{"legal_entity_id": %q, "name": "Board"}`, le) }
	e.As(other).Problem(404, "request.not_found", "POST", "/v1/cash-snapshots", body(e.le), "")
	e.Problem(404, "request.not_found", "POST", "/v1/cash-snapshots", body(uuid.New()), "")
	e.item(t, e.Supplier, "2026-04-01", "CAD", -9_000_000_000_000_000_000, nil)
	e.item(t, e.Supplier, "2026-04-02", "CAD", -9_000_000_000_000_000_000, nil)
	e.Problem(422, "cash.amount_too_large", "POST", "/v1/cash-snapshots", body(e.le), "")
}

// TestCashSnapshotRetention: the snapshots of a legal entity dated more
// than 25 months before the workflow's date are deleted with their lines,
// each audited; run again, none is.
func TestCashSnapshotRetention(t *testing.T) {
	e := setupSnapshot(t)
	c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	old, kept := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for day, sid := range map[string]uuid.UUID{"2024-03-16": old, "2024-03-17": kept} {
		pgtest.Exec(t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, snapshot_date, time_zone, horizon_end, config_version, taken_by)
			VALUES ($1, $2, $3, 'daily', $4, 'UTC', '2025-03-16', 1, $5)`, e.Tenant.ID, sid, e.le, day, cash.SnapshotterActor)
		pgtest.Exec(t, `INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, currency, source, certainty, amount, flow_count)
			VALUES ($1, $2, 1, 1, $3, 'CAD', 'payable', 'expected', -100, 1)`, e.Tenant.ID, sid, day)
	}
	err := apitest.InRequest(t, e.Tenant, pgtest.Actor{ID: cash.SnapshotterActor}, func(ctx context.Context) error {
		for i, wanted := range []int{1, 0} {
			n, err := c.PruneSnapshots(ctx, e.le, time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC))
			if err != nil {
				return err
			}
			want(t, fmt.Sprintf("run %d", i+1), n, wanted)
		}
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		var left []string
		if err := tx.QueryRow(ctx, `SELECT array_agg(snapshot_date::text ORDER BY snapshot_date) FROM cash.snapshot`).Scan(&left); err != nil {
			return err
		}
		want(t, "kept", left, []string{"2024-03-17"})
		var lines int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM cash.snapshot_flow WHERE snapshot_id = $1`, old).Scan(&lines); err != nil {
			return err
		}
		want(t, "its lines deleted with it", lines, 0)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// failingEntities is the tenancy port, down.
type failingEntities struct{}

func (failingEntities) Company(context.Context, uuid.UUID) (cash.Company, bool, error) {
	return cash.Company{}, false, errors.New("directory down")
}

func (failingEntities) ByCode(context.Context, string) (uuid.UUID, bool, error) {
	return uuid.Nil, false, errors.New("directory down")
}

// failingBanks is the payments port, unable to name house banks.
type failingBanks struct{ cash.Payments }

func (failingBanks) HouseBankCodes(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return nil, errors.New("banks down")
}

// TestCashSnapshotFailures: taking, reading, listing, pruning snapshots
// and finding those due fail with the database and with their ports.
func TestCashSnapshotFailures(t *testing.T) {
	e := setupSnapshot(t)
	id := uuid.MustParse(e.take(t, "Board")["id"].(string))
	day := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	endpoints := func() *cash.Endpoints {
		c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
		c.Consistent = inRequest{}
		return c
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"take": func(ctx context.Context) error { _, err := endpoints().TakeSnapshot(ctx, e.le, "Again"); return err },
		"read": func(ctx context.Context) error { _, err := endpoints().Snapshot(ctx, id); return err },
		"list": func(ctx context.Context) error {
			_, _, err := endpoints().Snapshots(ctx, cash.SnapshotsQuery{})
			return err
		},
		"lines": func(ctx context.Context) error {
			_, err := endpoints().SnapshotLines(ctx, cash.SnapshotLinesQuery{Snapshot: id, Currency: "CAD", PageSize: 50})
			return err
		},
		"lines of another currency": func(ctx context.Context) error {
			_, err := endpoints().SnapshotLines(ctx, cash.SnapshotLinesQuery{Snapshot: id, Currency: "EUR", PageSize: 50})
			return err
		},
		"daily": func(ctx context.Context) error { _, err := endpoints().TakeDailySnapshot(ctx, e.le, day); return err },
		"due":   func(ctx context.Context) error { _, err := endpoints().DueSnapshots(ctx, readAt); return err },
		"prune": func(ctx context.Context) error {
			_, err := endpoints().PruneSnapshots(ctx, e.le, day.AddDate(3, 0, 0))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want it refused")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	inRequestAs := func(fn func(ctx context.Context) error) error {
		t.Helper()
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
	}
	for name, c := range map[string]struct {
		change func(*cash.Endpoints)
		fn     func(*cash.Endpoints, context.Context) error
		want   string
	}{
		"no consistent read": {func(c *cash.Endpoints) { c.Consistent = nil },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.TakeSnapshot(ctx, e.le, "Board")
				return err
			}, "consistent read"},
		"the directory down for the daily": {func(c *cash.Endpoints) { c.LegalEntities = failingEntities{} },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.TakeDailySnapshot(ctx, e.le, day)
				return err
			}, "directory down"},
		"the directory down for the due": {func(c *cash.Endpoints) { c.LegalEntities = failingEntities{} },
			func(c *cash.Endpoints, ctx context.Context) error { _, err := c.DueSnapshots(ctx, readAt); return err }, "directory down"},
		"a zone lost": {func(c *cash.Endpoints) { c.Settings = zoneLost{c.Settings} },
			func(c *cash.Endpoints, ctx context.Context) error { _, err := c.DueSnapshots(ctx, readAt); return err }, "America/Nowhere"},
		"suppliers not named": {func(c *cash.Endpoints) { c.Partners = failingPartners{} },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.SnapshotLines(ctx, cash.SnapshotLinesQuery{Snapshot: id, Currency: "CAD", PageSize: 50})
				return err
			}, "partner names unavailable"},
		"banks not named": {func(c *cash.Endpoints) { c.Payments = failingBanks{c.Payments} },
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.SnapshotLines(ctx, cash.SnapshotLinesQuery{Snapshot: id, Currency: "CAD", PageSize: 50})
				return err
			}, "banks down"},
		"a page of none": {func(*cash.Endpoints) {},
			func(c *cash.Endpoints, ctx context.Context) error {
				_, err := c.ListCashSnapshotFlows(ctx, gen.ListCashSnapshotFlowsParams{ID: id, Currency: "CAD", PageSize: gen.NewOptInt32(0)})
				return err
			}, "page_size"},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := endpoints()
			c.change(c2)
			if err := inRequestAs(func(ctx context.Context) error { return c.fn(c2, ctx) }); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
	t.Run("a tenant without a configuration has none due", func(t *testing.T) {
		bare := pgtest.NewTenant(t)
		err := apitest.InRequest(t, bare, bare.Human, func(ctx context.Context) error {
			due, err := endpoints().DueSnapshots(ctx, readAt)
			want(t, "due", len(due), 0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
