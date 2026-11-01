package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// runFixture posts, in company 2000, a USD supplier invoice of 100.00
// booked at 0.8 and a USD receipt of 500.00 on the flagged bank account,
// valued at a bank_selling rate of 0.9 on 2026-03-31: a loss of 10.00 on
// the invoice and a gain of 50.00 on the bank.
func runFixture(t *testing.T) env {
	t.Helper()
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	expense, ap, bank := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "100000")
	revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: expense, Amount: 10000},
		ledger.PostingLine{AccountID: ap, Amount: -10000, PartnerID: &e.supplier})
	revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: bank, Amount: 50000}, ledger.PostingLine{AccountID: expense, Amount: -50000})
	return e
}

// count scans one count as the administrator.
func count(t *testing.T, n *int, sql string, args ...any) {
	t.Helper()
	if err := pgtest.Admin(t).QueryRow(context.Background(), sql, args...).Scan(n); err != nil {
		t.Fatal(err)
	}
}

// entryLines reads an entry and renders its header and lines by account
// number.
func entryLines(t *testing.T, e env, id string) string {
	t.Helper()
	numbers := map[uuid.UUID]string{}
	for _, n := range []string{"100000", "211099", "400000", "610000"} {
		numbers[settleAccount(t, e, "2000", n)] = n
		numbers[settleAccount(t, e, "1000", n)] = n
	}
	var entry ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		entry, err = ledger.GetEntry(ctx, uuid.MustParse(id))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out := fmt.Sprintf("%s %s %s %s", entry.DocumentType, entry.PostingDate.Format("2006-01-02"), entry.CurrencyTC, entry.SourceContext)
	for _, l := range entry.Lines {
		out += fmt.Sprintf(" | %s %d %d %d", numbers[l.AccountID], l.TC, l.LC, l.GC)
	}
	return out
}

// TestRevaluationRun posts a run (ADR-0042): a valuation entry on the key
// date with each total's difference on the flagged account or the
// adjustment account against the unrealised gain or loss, its reset on the
// next day, the run with its items, RevaluationRunPosted and both entries'
// JournalEntryPosted; a replayed request answers the same run; a second
// run of the key date is refused naming the first; agents are refused.
func TestRevaluationRun(t *testing.T) {
	e := runFixture(t)
	body := preview(e.de, "2026-03-31")
	key := apitest.NewKey()
	call := func() (int, map[string]any, string) {
		rec := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/revaluation-runs", Body: body, Tenant: e.ten.ID, As: e.ten.Human, Key: key})
		return rec.Code, apitest.JSON(t, rec), rec.Header().Get("Idempotent-Replayed")
	}
	code, run, _ := call()
	if code != 201 {
		t.Fatalf("run: got %d %v, want 201", code, run)
	}
	id := apitest.String(t, run, "id")
	t.Run("entries", func(t *testing.T) {
		if g, w := entryLines(t, e, apitest.String(t, run, "valuation_entry_id")),
			"SA 2026-03-31 EUR ledger | 100000 5000 5000 5000 | 400000 -5000 -5000 -5000 | 211099 -1000 -1000 -1000 | 610000 1000 1000 1000"; g != w {
			t.Errorf("valuation entry:\ngot  %s\nwant %s", g, w)
		}
		if g, w := entryLines(t, e, apitest.String(t, run, "reversal_entry_id")),
			"SA 2026-04-01 EUR ledger | 100000 -5000 -5000 -5000 | 400000 5000 5000 5000 | 211099 1000 1000 1000 | 610000 -1000 -1000 -1000"; g != w {
			t.Errorf("reset entry:\ngot  %s\nwant %s", g, w)
		}
	})
	t.Run("run", func(t *testing.T) {
		if g := fmt.Sprint(run["status"], run["item_count"], len(apitest.List(t, run, "items")), len(apitest.List(t, run, "totals"))); g != "posted2 2 2" {
			t.Errorf("run: got %s, want posted2 2 2", g)
		}
		got, _ := e.in(t).Must(200, "GET", "/v1/revaluation-runs/"+id, "", "")
		if fmt.Sprint(got["items"]) != fmt.Sprint(run["items"]) || fmt.Sprint(got["totals"]) != fmt.Sprint(run["totals"]) {
			t.Errorf("read back:\ngot  %v %v\nwant %v %v", got["items"], got["totals"], run["items"], run["totals"])
		}
		// The open item's partner and entry named, as the preview names them
		// (T73i2), on the post's answer and read back.
		var invoice string
		pgtest.Scan(t, `SELECT document_number FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`,
			[]any{e.ten.ID, apitest.List(t, got, "items")[1].(map[string]any)["journal_entry_id"]}, &invoice)
		for name, rep := range map[string]map[string]any{"posted": run, "read back": got} {
			it := apitest.List(t, rep, "items")[1].(map[string]any)
			if g, w := fmt.Sprint(it["partner_number"], " ", it["partner_name"], " ", it["document_number"]), "BP00000001 BP00000001 "+invoice; g != w {
				t.Errorf("%s: the open item named %s, want %s", name, g, w)
			}
		}
		page, _ := e.in(t).Must(200, "GET", "/v1/revaluation-runs?legal_entity_id="+e.de.String(), "", "")
		listed := apitest.List(t, page, "items")
		if len(listed) != 1 || listed[0].(map[string]any)["id"] != id || listed[0].(map[string]any)["items"] != nil {
			t.Errorf("list: got %v, want the run without its items", listed)
		}
	})
	t.Run("events", func(t *testing.T) {
		posted := apitest.Published(t, e.ten, outbox.Event{Context: "ledger", Name: "revaluation_run_posted"})
		if len(posted) != 1 || posted[0]["run_id"] != id || posted[0]["valuation_entry_id"] != run["valuation_entry_id"] {
			t.Errorf("RevaluationRunPosted: got %v, want one of run %s", posted, id)
		}
		seen := map[any]bool{}
		for _, p := range apitest.Published(t, e.ten, outbox.Event{Context: "ledger", Name: "journal_entry_posted"}) {
			seen[p["journal_entry_id"]] = true
		}
		if !seen[run["valuation_entry_id"]] || !seen[run["reversal_entry_id"]] {
			t.Error("JournalEntryPosted: want one for each of the run's entries")
		}
		var audits int
		count(t, &audits, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.revaluation_run.post' AND object_id = $2`, e.ten.ID, id)
		if audits != 1 {
			t.Errorf("audit records: got %d, want 1", audits)
		}
	})
	t.Run("replayed", func(t *testing.T) {
		code, again, replayed := call()
		if code != 201 || again["id"] != id || replayed != "true" {
			t.Errorf("replay: got %d %v replayed %q, want 201 of run %s replayed", code, again["id"], replayed, id)
		}
	})
	t.Run("a second run of the key date", func(t *testing.T) {
		rec := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/revaluation-runs", Body: body, Tenant: e.ten.ID, As: e.ten.Human, Key: apitest.NewKey()})
		p := apitest.JSON(t, rec)
		if rec.Code != 409 || p["code"] != "ledger.revaluation_exists" || !strings.Contains(fmt.Sprint(p["detail"]), id) {
			t.Errorf("second run: got %d %v, want 409 ledger.revaluation_exists naming %s", rec.Code, p, id)
		}
		if loc := rec.Header().Get("Location"); loc != "/v1/revaluation-runs/"+id {
			t.Errorf("Location: got %q, want /v1/revaluation-runs/%s", loc, id)
		}
	})
	t.Run("agents", func(t *testing.T) {
		agent := e.in(t)
		agent.As = e.ten.Agent
		agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/revaluation-runs", preview(e.de, "2026-02-28"), "")
	})
	t.Run("pages", func(t *testing.T) {
		// A run of February finds nothing to value and posts no entries.
		e.in(t).Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-02-28"), "")
		first, _ := e.in(t).Must(200, "GET", "/v1/revaluation-runs?page_size=1", "", "")
		token := apitest.String(t, first, "next_page_token")
		second, _ := e.in(t).Must(200, "GET", "/v1/revaluation-runs?page_size=1&page_token="+token, "", "")
		if g := fmt.Sprintf("%v %v %v", apitest.List(t, first, "items")[0].(map[string]any)["key_date"], apitest.List(t, second, "items")[0].(map[string]any)["key_date"],
			second["next_page_token"]); g != "2026-02-28 2026-03-31 <nil>" {
			t.Errorf("pages: got %s, want 2026-02-28 2026-03-31 <nil>", g)
		}
		e.in(t).Problem(400, "request.invalid_page_token", "GET", "/v1/revaluation-runs?page_token=garbage", "", "")
	})
	t.Run("its entries are not reversed alone", func(t *testing.T) {
		for _, k := range []string{"valuation_entry_id", "reversal_entry_id"} {
			p := e.in(t).Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+apitest.String(t, run, k)+":reverse", "", "*")
			if !strings.Contains(fmt.Sprint(p["detail"]), id) {
				t.Errorf("%s: detail %v, want it to name run %s", k, p["detail"], id)
			}
		}
	})
	t.Run("an unknown run", func(t *testing.T) {
		e.in(t).Problem(404, "request.not_found", "GET", "/v1/revaluation-runs/"+uuid.NewString(), "", "")
	})
}

// TestRevaluationRunGroupCurrency: in company 1000 (USD, group EUR) a GBP
// item whose local value falls by 5.00 while its group value rises by
// 5.00 posts both to the loss account, the group amount with the other
// sign (ADR-0042).
func TestRevaluationRunGroupCurrency(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'GBP', 'USD', '2026-01-01', 1.25, 'ECB', $2)`, e.ten.ID, e.ten.Human.ID)
	revaluationPost(t, e, e.us, "2026-03-02", "GBP", ledger.PostingLine{AccountID: settleAccount(t, e, "1000", "520000"), Amount: 10000},
		ledger.PostingLine{AccountID: settleAccount(t, e, "1000", "211000"), Amount: -10000, PartnerID: &e.supplier})
	revaluationRate(t, e, "GBP", "USD", "2026-03-15", "1.3")
	revaluationRate(t, e, "GBP", "EUR", "2026-03-20", "0.95")
	run, _ := e.Must(201, "POST", "/v1/revaluation-runs", preview(e.us, "2026-03-31"), "")
	if g, w := entryLines(t, e, apitest.String(t, run, "valuation_entry_id")),
		"SA 2026-03-31 USD ledger | 211099 -500 -500 500 | 610000 500 500 -500"; g != w {
		t.Errorf("valuation entry:\ngot  %s\nwant %s", g, w)
	}
}

// TestRevaluationRunWithoutDifferences: a run whose items are worth what
// they were booked at is recorded, and published, without entries; a run
// needs the next period open, and posts nothing when it is not.
func TestRevaluationRunWithoutDifferences(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.8")
	revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 10000},
		ledger.PostingLine{AccountID: settleAccount(t, e, "2000", "211000"), Amount: -10000, PartnerID: &e.supplier})
	t.Run("the next period closed", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = 4`, e.de)
		e.in(t).Problem(409, "ledger.period_not_open", "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = 4`, e.de)
		var runs int
		count(t, &runs, `SELECT count(*) FROM ledger.revaluation_run WHERE tenant_id = $1`, e.ten.ID)
		if runs != 0 {
			t.Errorf("runs: got %d, want none", runs)
		}
	})
	run, _ := e.Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	if g := fmt.Sprint(run["item_count"], run["valuation_entry_id"], run["reversal_entry_id"], len(apitest.List(t, run, "totals"))); g != "1 <nil> <nil> 0" {
		t.Errorf("run: got %s, want 1 item, no entries, no totals", g)
	}
	posted := apitest.Published(t, e.ten, outbox.Event{Context: "ledger", Name: "revaluation_run_posted"})
	if len(posted) != 1 || posted[0]["valuation_entry_id"] != nil {
		t.Errorf("RevaluationRunPosted: got %v, want one without entries", posted)
	}
}

// TestRevaluationRunRefused: a run of a legal entity that is not active,
// or one whose valuation fails, posts nothing.
func TestRevaluationRunRefused(t *testing.T) {
	e := runFixture(t)
	blocked := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Blocked', 'DE', 'EUR', gen_random_uuid(), 'blocked', $3, $3)`, e.ten.ID, blocked, e.ten.Human.ID)
	for _, c := range []struct{ name, body, code string }{
		{"a blocked legal entity", preview(blocked, "2026-03-31"), "ledger.legal_entity_not_active"},
		{"an unknown legal entity", preview(uuid.New(), "2026-03-31"), "request.reference_not_found"},
		{"a key date that is not a period end", preview(e.de, "2026-03-30"), "ledger.key_date_not_period_end"},
	} {
		t.Run(c.name, func(t *testing.T) { e.in(t).Problem(422, c.code, "POST", "/v1/revaluation-runs", c.body, "") })
	}
	t.Run("a loss account blocked for posting", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE legal_entity_id = $1 AND account_id = $2`,
			e.de, settleAccount(t, e, "2000", "610000"))
		e.in(t).Problem(422, "ledger.account_not_postable", "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	})
}

// TestRevaluationRunFailures fails each statement of a run that posts
// both entries, and of reading and listing runs, and calls each outside a
// unit of work.
func TestRevaluationRunFailures(t *testing.T) {
	e := runFixture(t)
	h := endpoints()
	post := func(ctx context.Context) error {
		_, err := h.PostRevaluationRun(ctx, &gen.RevaluationPreviewRequest{LegalEntityID: e.de, KeyDate: date("2026-03-31")}, gen.PostRevaluationRunParams{})
		return err
	}
	if err := post(context.Background()); err == nil {
		t.Error("post outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post revaluation run", post)
	run, _ := e.Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	id := uuid.MustParse(apitest.String(t, run, "id"))
	reads := map[string]func(ctx context.Context) error{
		"get revaluation run": func(ctx context.Context) error {
			_, err := h.GetRevaluationRun(ctx, gen.GetRevaluationRunParams{ID: id})
			return err
		},
		"list revaluation runs": func(ctx context.Context) error {
			_, err := h.ListRevaluationRuns(ctx, gen.ListRevaluationRunsParams{})
			return err
		},
	}
	for name, call := range reads {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Errorf("%s outside a unit of work: got no error, want one", name)
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
}

// TestRevaluationRunResetRate: the reset entry, dated the next day,
// records that day's local-to-group rate, so a rate that cannot be
// recorded then refuses the run (company 1000, local USD, group EUR).
func TestRevaluationRunResetRate(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationPost(t, e, e.us, "2026-03-02", "EUR", ledger.PostingLine{AccountID: settleAccount(t, e, "1000", "520000"), Amount: 10000},
		ledger.PostingLine{AccountID: settleAccount(t, e, "1000", "211000"), Amount: -10000, PartnerID: &e.supplier})
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'EUR', 'USD', '2026-04-01', 3000000000000, 'ECB', $2)`, e.ten.ID, e.ten.Human.ID)
	e.Problem(422, "ledger.rate_not_recordable", "POST", "/v1/revaluation-runs", preview(e.us, "2026-03-31"), "")
}

// TestRevaluationRunTooLarge: a run whose totals are out of range is
// refused, and a recorded run whose items no longer sum in range (written
// past the application) is refused on reading.
func TestRevaluationRunTooLarge(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	settleRate(t, e, "USD", "2026-01-01", "0.8")
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	expense, ap := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000")
	var entries []uuid.UUID
	for _, day := range []string{"2026-02-02", "2026-03-02"} {
		entries = append(entries, revaluationPost(t, e, e.de, day, "USD", ledger.PostingLine{AccountID: expense, Amount: 5e18},
			ledger.PostingLine{AccountID: ap, Amount: -5e18, PartnerID: &e.supplier}).ID)
	}
	e.Problem(422, "ledger.amount_too_large", "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	run := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, created_by, updated_by)
		SELECT $1, $2, $3, id, '2026-01-31', 'average', $4, $4 FROM ledger.ledger WHERE tenant_id = $1 AND is_leading`, e.ten.ID, run, e.de, e.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.revaluation_item (tenant_id, run_id, item_no, account_id, partner_id, fiscal_year, journal_entry_id, line_no,
			currency_tc, amount_tc, amount_lc, amount_gc, rate, rate_valid_from, valued_lc, rate_gc, rate_gc_valid_from, valued_gc)
		SELECT $1, $2, n, $3, $4, 2026, ($5::uuid[])[n], 2, 'USD', -1, 0, 0, 1, '2026-01-01', 9e18, 1, '2026-01-01', 0 FROM generate_series(1, 2) n`,
		e.ten.ID, run, ap, e.supplier, entries)
	e.Problem(422, "ledger.amount_too_large", "GET", "/v1/revaluation-runs/"+run.String(), "", "")
}

// cancelRun cancels a run with a fresh Idempotency-Key.
func cancelRun(e env, id, ifMatch string) (int, map[string]any) {
	rec := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/revaluation-runs/" + id + ":cancel", Body: `{"reason": "The USD rate was keyed wrongly."}`,
		Tenant: e.ten.ID, As: e.ten.Human, Key: apitest.NewKey(), IfMatch: ifMatch})
	return rec.Code, apitest.JSON(e.T, rec)
}

// TestRevaluationRunCancel cancels a run (ADR-0042): both entries are
// reversed on their own dates, the run is cancelled with its items,
// audited and published, and its key date is free for another run; a
// replay answers the same, a second cancel is refused, If-Match is
// required and checked, agents are refused, and a run without entries is
// cancelled without reversals.
func TestRevaluationRunCancel(t *testing.T) {
	e := runFixture(t)
	run, tag := e.Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	id := apitest.String(t, run, "id")
	path := "/v1/revaluation-runs/" + id + ":cancel"
	body := `{"reason": "The USD rate was keyed wrongly."}`
	t.Run("preconditions", func(t *testing.T) {
		e.in(t).Problem(428, "request.precondition_required", "POST", path, body, "")
		e.in(t).Problem(412, "request.version_mismatch", "POST", path, body, `"99"`)
		agent := e.in(t)
		agent.As = e.ten.Agent
		agent.Problem(403, "identity.agent_not_allowed", "POST", path, body, tag)
		e.in(t).Problem(404, "request.not_found", "POST", "/v1/revaluation-runs/"+uuid.NewString()+":cancel", body, "*")
	})
	key := apitest.NewKey()
	call := func() (int, map[string]any, string, string) {
		rec := e.API.Do(apitest.Req{Method: "POST", Path: path, Body: body, Tenant: e.ten.ID, As: e.ten.Human, Key: key, IfMatch: tag})
		return rec.Code, apitest.JSON(t, rec), rec.Header().Get("ETag"), rec.Header().Get("Idempotent-Replayed")
	}
	code, cancelled, cancelledTag, _ := call()
	if code != 200 {
		t.Fatalf("cancel: got %d %v, want 200", code, cancelled)
	}
	if g := fmt.Sprint(cancelled["status"], len(apitest.List(t, cancelled, "items"))); g != "cancelled2" {
		t.Errorf("cancelled run: got %s, want cancelled with its 2 items", g)
	}
	if cancelledTag == tag {
		t.Errorf("ETag: got %s again, want a new one", tag)
	}
	t.Run("reversals", func(t *testing.T) {
		reversed := apitest.Published(t, e.ten, outbox.Event{Context: "ledger", Name: "journal_entry_reversed"})
		dates := map[any]any{}
		for _, r := range reversed {
			dates[r["journal_entry_id"]] = r["reversal_posting_date"]
		}
		if g := fmt.Sprintln(len(reversed), dates[run["valuation_entry_id"]], dates[run["reversal_entry_id"]]); g != "2 2026-03-31 2026-04-01\n" {
			t.Errorf("JournalEntryReversed: got %s, want 2 2026-03-31 2026-04-01", g)
		}
	})
	t.Run("event and audit", func(t *testing.T) {
		got := apitest.Published(t, e.ten, outbox.Event{Context: "ledger", Name: "revaluation_run_cancelled"})
		if len(got) != 1 {
			t.Fatalf("RevaluationRunCancelled: got %d, want 1", len(got))
		}
		if g, w := fmt.Sprint(got[0]["run_id"], " ", got[0]["reason"]), id+" The USD rate was keyed wrongly."; g != w {
			t.Errorf("RevaluationRunCancelled: got %s, want %s", g, w)
		}
		if got[0]["valuation_reversal_entry_id"] == nil || got[0]["reset_reversal_entry_id"] == nil {
			t.Errorf("RevaluationRunCancelled: got %v, want both reversals", got[0])
		}
		var audits int
		count(t, &audits, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.revaluation_run.cancel' AND object_id = $2`, e.ten.ID, id)
		if audits != 1 {
			t.Errorf("audit records: got %d, want 1", audits)
		}
	})
	t.Run("replayed", func(t *testing.T) {
		code, again, _, replayed := call()
		if g := fmt.Sprint(code, again["status"], replayed); g != "200cancelledtrue" {
			t.Errorf("replay: got %s, want 200 cancelled, replayed", g)
		}
	})
	t.Run("cancelled twice", func(t *testing.T) {
		e.in(t).Problem(409, "ledger.revaluation_not_posted", "POST", path, body, "*")
	})
	t.Run("its entries stay reversed", func(t *testing.T) {
		e.in(t).Problem(409, "ledger.already_reversed", "POST", "/v1/journal-entries/"+apitest.String(t, run, "valuation_entry_id")+":reverse", "", "*")
	})
	t.Run("rerun", func(t *testing.T) {
		again, _ := e.in(t).Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
		if again["id"] == id || again["valuation_entry_id"] == nil {
			t.Errorf("rerun: got %v, want a new run with its own entries", again)
		}
	})
	t.Run("a run without entries", func(t *testing.T) {
		empty, emptyTag := e.in(t).Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-02-28"), "")
		if code, got := cancelRun(e, apitest.String(t, empty, "id"), emptyTag); code != 200 || got["status"] != "cancelled" {
			t.Errorf("cancel: got %d %v, want 200 cancelled", code, got)
		}
		events := apitest.Published(t, e.ten, outbox.Event{Context: "ledger", Name: "revaluation_run_cancelled"})
		last := events[len(events)-1]
		if last["run_id"] != empty["id"] || last["valuation_reversal_entry_id"] != nil {
			t.Errorf("RevaluationRunCancelled: got %v, want run %v without reversals", last, empty["id"])
		}
	})
}

// TestRevaluationRunCancelPeriodLocked: a run whose valuation entry's or
// reset entry's period is locked (T32) is not cancelled, and nothing of it
// is reversed.
func TestRevaluationRunCancelPeriodLocked(t *testing.T) {
	for _, period := range []int{3, 4} {
		t.Run(fmt.Sprintf("period %d", period), func(t *testing.T) {
			e := runFixture(t)
			run, tag := e.in(t).Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
			for _, status := range []string{"closed", "locked"} {
				pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = $2`,
					e.de, period, status)
			}
			p := e.in(t).Problem(409, "ledger.period_not_open", "POST", "/v1/revaluation-runs/"+apitest.String(t, run, "id")+":cancel",
				`{"reason": "late"}`, tag)
			if !strings.Contains(fmt.Sprint(p["detail"]), "locked") {
				t.Errorf("detail: got %v, want it to say the period is locked", p["detail"])
			}
			var reversals, posted int
			count(t, &reversals, `SELECT count(*) FROM ledger.reversal WHERE tenant_id = $1`, e.ten.ID)
			count(t, &posted, `SELECT count(*) FROM ledger.revaluation_run WHERE tenant_id = $1 AND status = 'posted'`, e.ten.ID)
			if reversals != 0 || posted != 1 {
				t.Errorf("after the refusal: got %d reversals and %d posted runs, want 0 and 1", reversals, posted)
			}
		})
	}
}

// TestRevaluationRunCancelFailures fails each statement of cancelling a
// run that posted both entries, and calls it outside a unit of work.
func TestRevaluationRunCancelFailures(t *testing.T) {
	e := runFixture(t)
	run, _ := e.Must(201, "POST", "/v1/revaluation-runs", preview(e.de, "2026-03-31"), "")
	h := endpoints()
	cancel := func(ctx context.Context) error {
		_, err := h.CancelRevaluationRun(ctx, &gen.ReasonInput{Reason: "test"},
			gen.CancelRevaluationRunParams{ID: uuid.MustParse(apitest.String(t, run, "id")), IfMatch: "*"})
		return err
	}
	if err := cancel(context.Background()); err == nil {
		t.Error("cancel outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "cancel revaluation run", cancel)
}
