package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// ledgerRevaluation is setupParallel with revaluationSetup's valuation in
// both ledgers, 2L valued at bank_buying rates of its own (T52d,
// ADR-0045). Company 2000 has a USD supplier invoice of 100.00 booked at
// 0.8 in both ledgers, a second one of 30.00 cleared on 2026-03-20 (in
// every ledger, T52c), and in 2L alone a USD receipt of 50.00 on the
// flagged bank account; on 2026-03-31 USD is worth 0.9 at bank_selling, the
// legal entity's, and 0.85 at bank_buying, 2L's.
func ledgerRevaluation(t *testing.T) (parallelEnv, ledger.Entry) {
	t.Helper()
	p := setupParallel(t)
	doc := revaluationDoc(parallel(t), true)
	doc.Company("2000").FXValuation.Ledgers = []config.FXValuationLedger{{Code: "2L", RateType: "bank_buying"}}
	applyRevaluation(t, p.env, doc)
	settleRate(t, p.env, "USD", "2026-01-01", "0.8")
	revaluationRate(t, p.env, "USD", "EUR", "2026-03-15", "0.9")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'bank_buying', 'USD', 'EUR', '2026-03-15', 0.85, 'ECB', $2)`, p.ten.ID, p.ten.Human.ID)
	expense, ap := settleAccount(t, p.env, "2000", "520000"), settleAccount(t, p.env, "2000", "211000")
	invoice := revaluationPost(t, p.env, p.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: expense, Amount: 10000},
		ledger.PostingLine{AccountID: ap, Amount: -10000, PartnerID: &p.supplier})
	paid := revaluationPost(t, p.env, p.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: expense, Amount: 3000},
		ledger.PostingLine{AccountID: ap, Amount: -3000, PartnerID: &p.supplier})
	payment := revaluationPost(t, p.env, p.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: ap, Amount: 3000, PartnerID: &p.supplier},
		ledger.PostingLine{AccountID: expense, Amount: -3000})
	p.Must(201, "POST", "/v1/clearings", clearing(p.de, "2026-03-20", ref{map[string]any{"fiscal_year": 2026, "id": paid.ID.String()}, 2},
		ref{map[string]any{"fiscal_year": 2026, "id": payment.ID.String()}, 1}), "")
	p.Must(201, "POST", "/v1/journal-entries", with(entry(p.de, "2026-03-02", "USD", line{p.account(t, "2000", "100000"), "50.00", ""},
		line{expense.String(), "-50.00", ""}), `"ledger_id": "`+p.local+`"`), "")
	return p, invoice
}

// ledgerPreview is a preview or run request for a ledger.
func ledgerPreview(le uuid.UUID, ledgerID, keyDate string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "key_date": %q}`, le, ledgerID, keyDate)
}

// valuedItems renders a preview's or run's rate type and items, each as
// account number, the entry of an open item and the local difference.
func valuedItems(t *testing.T, p parallelEnv, got map[string]any) string {
	t.Helper()
	numbers := map[string]string{}
	for _, n := range []string{"100000", "211000"} {
		numbers[p.account(t, "2000", n)] = n
	}
	out := fmt.Sprint(got["rate_type"])
	for _, x := range apitest.List(t, got, "items") {
		it := x.(map[string]any)
		out += fmt.Sprintf(" | %s %v %v", numbers[it["account_id"].(string)], it["journal_entry_id"], it["difference_lc"].(map[string]any)["amount"])
	}
	return out
}

// TestRevaluationPerLedger: a revaluation values one ledger's lines at
// its rate type, the ledger's own or the legal entity's, and posts and
// reverses in that ledger alone; one run is posted per key date and
// ledger, a request replayed answers the same run, and only a ledger the
// company keeps is valued (T52d, ADR-0045).
func TestRevaluationPerLedger(t *testing.T) {
	p, invoice := ledgerRevaluation(t)
	path := "/v1/revaluation-runs"
	t.Run("previews", func(t *testing.T) {
		got, _ := p.in(t).Must(200, "POST", path+"/preview", preview(p.de, "2026-03-31"), "")
		if g, w := valuedItems(t, p, got), "bank_selling | 211000 "+invoice.ID.String()+" -10.00"; g != w {
			t.Errorf("leading ledger:\ngot  %s\nwant %s", g, w)
		}
		got, _ = p.in(t).Must(200, "POST", path+"/preview", ledgerPreview(p.de, p.local, "2026-03-31"), "")
		if g, w := valuedItems(t, p, got), "bank_buying | 100000 <nil> 2.50 | 211000 "+p.copyOf(t, invoice.ID)+" -5.00"; g != w {
			t.Errorf("2L:\ngot  %s\nwant %s", g, w)
		}
		if got["ledger_id"] != p.local {
			t.Errorf("ledger: got %v, want 2L %s", got["ledger_id"], p.local)
		}
	})
	key := apitest.NewKey()
	run2L := func(key string) (int, map[string]any) {
		rec := p.API.Do(apitest.Req{Method: "POST", Path: path, Body: ledgerPreview(p.de, p.local, "2026-03-31"), Tenant: p.ten.ID, As: p.ten.Human, Key: key})
		return rec.Code, apitest.JSON(t, rec)
	}
	code, local := run2L(key)
	if code != 201 {
		t.Fatalf("2L run: got %d %v, want 201", code, local)
	}
	leading, _ := p.Must(201, "POST", path, preview(p.de, "2026-03-31"), "")
	t.Run("entries in the run's ledger alone", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			run    map[string]any
			ledger string
			lines  string
		}{
			{"2L", local, "2L", " | 100000 250 250 250 | 400000 -250 -250 -250 | 211099 -500 -500 -500 | 610000 500 500 500"},
			{"0L", leading, "0L", " | 211099 -1000 -1000 -1000 | 610000 1000 1000 1000"},
		} {
			t.Run(c.name, func(t *testing.T) {
				valuation := apitest.String(t, c.run, "valuation_entry_id")
				if g, w := entryLines(t, p.env, valuation), "SA 2026-03-31 EUR ledger"+c.lines; g != w {
					t.Errorf("valuation entry:\ngot  %s\nwant %s", g, w)
				}
				for _, id := range []string{valuation, apitest.String(t, c.run, "reversal_entry_id")} {
					if ledgers, _ := p.group(t, id); ledgers != c.ledger {
						t.Errorf("entry %s posted to: got %s, want %s alone", id, ledgers, c.ledger)
					}
				}
			})
		}
		if g := fmt.Sprint(local["ledger_id"] == p.local, " ", local["rate_type"], " ", leading["rate_type"]); g != "true bank_buying bank_selling" {
			t.Errorf("runs: got %s, want 2L's at bank_buying and 0L's at bank_selling", g)
		}
	})
	t.Run("one run per key date and ledger", func(t *testing.T) {
		code, again := run2L(key)
		if code != 201 || again["id"] != local["id"] {
			t.Errorf("replayed: got %d %v, want 201 of run %v", code, again["id"], local["id"])
		}
		code, second := run2L(apitest.NewKey())
		if code != 409 || second["code"] != "ledger.revaluation_exists" || !strings.Contains(fmt.Sprint(second["detail"]), local["id"].(string)) {
			t.Errorf("second 2L run: got %d %v, want 409 ledger.revaluation_exists naming %v", code, second, local["id"])
		}
		p.in(t).Problem(409, "ledger.revaluation_exists", "POST", path, preview(p.de, "2026-03-31"), "")
		var ledgers, want []string
		for _, ev := range apitest.Published(t, p.ten, outbox.Event{Context: "ledger", Name: "revaluation_run_posted"}) {
			ledgers = append(ledgers, fmt.Sprint(ev["ledger_id"]))
		}
		want = []string{p.leading, p.local}
		slices.Sort(ledgers)
		slices.Sort(want)
		if !slices.Equal(ledgers, want) {
			t.Errorf("RevaluationRunPosted: got ledgers %v, want one each of %v", ledgers, want)
		}
	})
	t.Run("cancelled in its ledger alone", func(t *testing.T) {
		p.in(t).Must(200, "POST", path+"/"+apitest.String(t, local, "id")+":cancel", `{"reason": "2L rate type corrected."}`, "*")
		var reversals []string
		pgtest.Scan(t, `SELECT array_agg(DISTINCT l.code) FROM ledger.reversal r JOIN ledger.journal_entry e ON e.tenant_id = r.tenant_id
			AND e.fiscal_year = r.reversal_year AND e.id = r.reversal_entry_id JOIN ledger.ledger l ON l.id = e.ledger_id
			WHERE r.tenant_id = $1 AND r.original_entry_id = ANY($2::uuid[])`,
			[]any{p.ten.ID, []string{apitest.String(t, local, "valuation_entry_id"), apitest.String(t, local, "reversal_entry_id")}}, &reversals)
		if !slices.Equal(reversals, []string{"2L"}) {
			t.Errorf("reversals: got ledgers %v, want 2L alone", reversals)
		}
		got, _ := p.in(t).Must(200, "GET", path+"/"+apitest.String(t, leading, "id"), "", "")
		if got["status"] != "posted" {
			t.Errorf("0L run: got %v, want posted", got["status"])
		}
		if code, rerun := run2L(apitest.NewKey()); code != 201 || rerun["id"] == local["id"] {
			t.Errorf("2L run after the cancel: got %d %v, want 201 of a new run", code, rerun["id"])
		}
	})
	t.Run("a ledger the company does not keep", func(t *testing.T) {
		for _, op := range []string{path + "/preview", path} {
			p.in(t).Problem(422, "ledger.ledger_not_kept", "POST", op, ledgerPreview(p.us, p.local, "2026-03-31"), "")
		}
	})
}

// TestRevaluationPerLedgerPeriods: each ledger's run needs its own
// ledger's periods open, so 2L closed for April refuses 2L's run and
// leaves 0L's to post (T32, T52d).
func TestRevaluationPerLedgerPeriods(t *testing.T) {
	p, _ := ledgerRevaluation(t)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = 2026 AND period = 4`, p.de, p.local)
	p.Problem(409, "ledger.period_not_open", "POST", "/v1/revaluation-runs", ledgerPreview(p.de, p.local, "2026-03-31"), "")
	p.Must(201, "POST", "/v1/revaluation-runs", preview(p.de, "2026-03-31"), "")
	var runs int
	count(t, &runs, `SELECT count(*) FROM ledger.revaluation_run WHERE tenant_id = $1 AND ledger_id = $2`, p.ten.ID, p.local)
	if runs != 0 {
		t.Errorf("2L runs: got %d, want none", runs)
	}
}

// TestRevaluationPerLedgerFailures fails each statement of a parallel
// ledger's preview, run and cancel.
func TestRevaluationPerLedgerFailures(t *testing.T) {
	p, _ := ledgerRevaluation(t)
	h := endpoints()
	req := &gen.RevaluationPreviewRequest{LegalEntityID: p.de, LedgerID: gen.NewOptUUID(uuid.MustParse(p.local)), KeyDate: date("2026-03-31")}
	apitest.EveryFailure(t, p.ten, p.ten.Human, "preview 2L", func(ctx context.Context) error {
		_, err := h.PreviewRevaluation(ctx, req, gen.PreviewRevaluationParams{})
		return err
	})
	apitest.EveryFailure(t, p.ten, p.ten.Human, "post 2L run", func(ctx context.Context) error {
		_, err := h.PostRevaluationRun(ctx, req, gen.PostRevaluationRunParams{})
		return err
	})
	run, _ := p.Must(201, "POST", "/v1/revaluation-runs", ledgerPreview(p.de, p.local, "2026-03-31"), "")
	apitest.EveryFailure(t, p.ten, p.ten.Human, "cancel 2L run", func(ctx context.Context) error {
		_, err := h.CancelRevaluationRun(ctx, &gen.ReasonInput{Reason: "test"},
			gen.CancelRevaluationRunParams{ID: uuid.MustParse(apitest.String(t, run, "id")), IfMatch: "*"})
		return err
	})
}
