package assets_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withImpairments is withLedgers with company 2000's impairment loss
// (695000, an expense) and impairment reversal (495000, a revenue)
// accounts for MACHINES; LAND names none.
func withImpairments(doc *config.Config) {
	withLedgers(doc)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "695000", Name: "Impairment of machinery", Type: config.Expense},
		config.Account{Number: "495000", Name: "Reversal of impairments", Type: config.Revenue})
	m := de.AssetAccountsOf("MACHINES")
	m.ImpairmentLoss, m.ImpairmentReversal = "695000", "495000"
}

// newImpairments is newRuns with withImpairments.
func newImpairments(t *testing.T) assetEnv {
	t.Helper()
	return newRunsWith(t, withImpairments)
}

// impair posts an impairment of the asset with its current ETag and
// returns it.
func (e assetEnv) impair(t *testing.T, asset any, body string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/assets/", asset), "", "")
	q, _ := e.clocked.Must(201, "POST", fmt.Sprint("/v1/assets/", asset, ":impair"), "{"+body+"}", tag)
	return q
}

// refuseImpairment asks for an impairment of the asset with its current
// ETag and wants the problem.
func (e assetEnv) refuseImpairment(t *testing.T, asset any, body string, status int, code string) {
	t.Helper()
	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/assets/", asset), "", "")
	e.clocked.Problem(status, code, "POST", fmt.Sprint("/v1/assets/", asset, ":impair"), "{"+body+"}", tag)
}

// impaired is an impairment as "kind ledger value posting year/period
// amount amount-gc carrying after unimpaired asset".
func impaired(q map[string]any) string {
	return fmt.Sprint(q["kind"], " ", q["ledger_code"], " ", q["value_date"], " ", q["posting_date"], " ", q["fiscal_year"], "/", q["period"], " ",
		moneyOf(q["amount"]), " ", moneyOf(q["amount_gc"]), " ", amount(q["carrying_amount"]), " ", amount(q["carrying_amount_after"]), " ",
		amount(q["unimpaired_amount"]), " ", q["company_code"], " ", q["asset_number"] != nil, " ", q["asset_description"])
}

// sheetLine is the MACHINES row of company 2000's 2026 sheet in the
// ledger: "depreciation losses reversals closing-accumulated closing-net".
func (e assetEnv) sheetLine(t *testing.T, ledger uuid.UUID) string {
	t.Helper()
	s, _ := e.Must(200, "GET", fmt.Sprintf("/v1/asset-history-sheet?legal_entity_id=%s&ledger_id=%s&fiscal_year=2026&class=MACHINES", e.DE, ledger), "", "")
	r := s["items"].([]any)[0].(map[string]any)
	return fmt.Sprint(amount(r["opening_accumulated"]), " ", amount(r["depreciation"]), " ", amount(r["impairment_losses"]), " ",
		amount(r["impairment_reversals"]), " ", amount(r["closing_accumulated"]), " ", amount(r["closing_net_book_value"]), " | ",
		amount(s["totals"].(map[string]any)["impairment_losses"]), " ", amount(s["totals"].(map[string]any)["impairment_reversals"]))
}

// TestImpairment: an asset depreciated in June is impaired by 300.00 in
// 0L at June's end, measured against its carrying amount then (1,190.00),
// with one entry in 0L alone (Dr impairment loss on its cost center / Cr
// accumulated depreciation); from July it depreciates the 890.00 left
// over the 119 periods of its life left (7.48, IAS 36.63), which July's
// run posts. In July a reversal above what the impairment took, net of
// the depreciation it saved (1,180.00 - 882.52 = 297.48, IAS 36.117), is
// refused, and one of 297.48 restores the asset to the carrying amount it
// would have had, so August depreciates 10.00 again. The history sheet
// shows both lines and reconciles; each is published and audited, reads
// back, and a stale If-Match is refused.
func TestImpairment(t *testing.T) {
	e := newImpairments(t)
	mill, _ := e.depreciable(t, "Mill")
	_, june := e.start(t, e.l0, 6)
	e.carry(t, june)
	_, stale := e.Must(200, "GET", "/v1/assets/"+mill, "", "")

	loss := e.impair(t, mill, `"kind": "loss", "amount": {"amount": "300.00", "currency": "EUR"}`)
	want(t, "the loss", impaired(loss), "loss 0L 2026-06-15 2026-06-15 2026/6 300.00 EUR 326.09 USD 1190.00 890.00 1190.00 2000 true Mill")
	want(t, "its entry, in 0L alone", entryLines(t, entryOf(loss, "0L")), "0L 695000 1000 PC1 30000 32609; 0L 160900 - PC1 -30000 -32609")
	want(t, "its entries", len(loss["entries"].([]any)), 1)
	want(t, "the mill after the loss", e.asset(t, mill), "active 2026-06-10; 0L 1200.00 EUR 1304.35 USD -310.00 EUR -336.96 USD 890.00 EUR 967.39 USD; "+
		"2L 1200.00 EUR 1304.35 USD 0.00 EUR 0.00 USD 1200.00 EUR 1304.35 USD")
	e.clocked.Problem(412, "request.version_mismatch", "POST", "/v1/assets/"+mill+":impair", `{"kind": "loss", "amount": {"amount": "1.00", "currency": "EUR"}}`,
		stale)
	s, _ := e.Must(200, "GET", fmt.Sprintf("/v1/assets/%s/schedule?fiscal_year=2026&ledger_id=%s", mill, e.l0), "", "")
	want(t, "the schedule from June", schedule(s, 6), "6 10.00 10.00 890.00; 7 7.48 0.00 882.52; 8 7.48 0.00 875.04; 9 7.48 0.00 867.56; "+
		"10 7.48 0.00 860.08; 11 7.48 0.00 852.60; 12 7.48 0.00 845.12 | 54.88 10.00")
	_, july := e.start(t, e.l0, 7)
	want(t, "July's run, on the impaired amount", runOf(e.carry(t, july)), "posted 1 1 0 7.48; 1 posted 1 1 0 7.48")

	inJuly := `"posting_date": "2026-07-20", "value_date": "2026-07-20"`
	e.refuseImpairment(t, mill, `"kind": "reversal", "amount": {"amount": "297.49", "currency": "EUR"}, `+inJuly, 422, "assets.reversal_exceeds_impairment")
	back := e.impair(t, mill, `"kind": "reversal", "amount": {"amount": "297.48", "currency": "EUR"}, "text": "Market recovered", `+inJuly)
	want(t, "the reversal", impaired(back), "reversal 0L 2026-07-20 2026-07-20 2026/7 297.48 EUR 323.35 USD 882.52 1180.00 1180.00 2000 true Mill")
	want(t, "its text", back["text"], "Market recovered")
	want(t, "its entry", entryLines(t, entryOf(back, "0L")), "0L 160900 - PC1 29748 32335; 0L 495000 - PC1 -29748 -32335")
	s, _ = e.Must(200, "GET", fmt.Sprintf("/v1/assets/%s/schedule?fiscal_year=2026&ledger_id=%s", mill, e.l0), "", "")
	want(t, "the schedule from July", schedule(s, 7), "7 7.48 7.48 1180.00; 8 10.00 0.00 1170.00; 9 10.00 0.00 1160.00; 10 10.00 0.00 1150.00; "+
		"11 10.00 0.00 1140.00; 12 10.00 0.00 1130.00 | 67.48 17.48")

	want(t, "the sheet in 0L", e.sheetLine(t, e.l0), "0.00 -17.48 -300.00 297.48 -20.00 1180.00 | -300.00 297.48")
	want(t, "the sheet in 2L", e.sheetLine(t, e.l2), "0.00 0.00 0.00 0.00 0.00 1200.00 | 0.00 0.00")
	want(t, "the accumulated depreciation account in 0L, the sheet's closing", balances(t, e.Tenant.ID, e.Account(t, "2000", "160900")), "0L: -2000 -2000 -2174")

	posted := slices.DeleteFunc(apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_posted"}),
		func(ev map[string]any) bool { return ev["document_kind"] != "impairment" })
	want(t, "AssetMovementPosted", fmt.Sprint(len(posted), " ", posted[0]["impairment_kind"], " ", posted[1]["impairment_kind"], " ",
		posted[0]["ledgers"].([]any)[0].(map[string]any)["accumulated"], " ", posted[1]["ledgers"].([]any)[0].(map[string]any)["accumulated_gc"]),
		"2 loss reversal map[amount:-300.00 currency:EUR] map[amount:323.35 currency:USD]")
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.impairment.post' AND NOT detail ? 'text'`,
		[]any{e.Tenant.ID}, &audited)
	want(t, "audited, the text not recorded", audited, 2)
	got, _ := e.Must(200, "GET", fmt.Sprint("/v1/asset-impairments/", loss["id"]), "", "")
	want(t, "read back", impaired(got), impaired(loss))
	m, _ := e.Must(200, "GET", fmt.Sprintf("/v1/assets/%s/movements?kind=impairment_reversal", mill), "", "")
	mv := m["items"].([]any)[0].(map[string]any)
	want(t, "the reversal's movement", fmt.Sprint(mv["document_kind"], " ", amount(mv["accumulated"]), " ", mv["document_number"] != nil),
		"impairment 297.48 true")
}

// TestImpairmentRefused: what an impairment refuses before it posts.
func TestImpairmentRefused(t *testing.T) {
	e := newImpairments(t)
	mill, _ := e.depreciable(t, "Mill")
	land, _ := e.create(t, e.body(e.DE, "LAND", ""))
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "500.00"}, line{"100000", "-500.00"})
	e.acquire(t, land["id"], entry, 1, "")
	planned, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	loss := func(amount string, extra string) string {
		return fmt.Sprintf(`"kind": "loss", "amount": {"amount": %q, "currency": "EUR"}%s`, amount, extra)
	}
	for name, c := range map[string]struct {
		asset  any
		body   string
		status int
		code   string
	}{
		"an unknown kind":           {mill, `"kind": "write_down", "amount": {"amount": "1.00", "currency": "EUR"}`, 422, "request.validation_failed"},
		"an amount of zero":         {mill, loss("0.00", ""), 422, "request.validation_failed"},
		"an amount not of EUR":      {mill, loss("1.001", ""), 400, "request.invalid_amount"},
		"another currency":          {mill, `"kind": "loss", "amount": {"amount": "1.00", "currency": "USD"}`, 422, "request.validation_failed"},
		"a ledger it does not keep": {mill, loss("1.00", fmt.Sprintf(`, "ledger_id": %q`, e.l3)), 422, "ledger.ledger_not_kept"},
		"above the carrying amount": {mill, loss("1200.01", ""), 422, "assets.impairment_exceeds_carrying_amount"},
		"a reversal of nothing":     {mill, `"kind": "reversal", "amount": {"amount": "0.01", "currency": "EUR"}`, 422, "assets.reversal_exceeds_impairment"},
		"valued in May":             {mill, loss("1.00", `, "value_date": "2026-05-31"`), 422, "assets.value_date_outside_period"},
		"valued before its capitalisation": {mill, loss("1.00", `, "value_date": "2026-06-09"`), 422,
			"assets.value_date_before_capitalisation"},
		"a class without the account": {land["id"], loss("1.00", ""), 422, "assets.impairment_account_missing"},
		"a class without the reversal's account": {land["id"], `"kind": "reversal", "amount": {"amount": "1.00", "currency": "EUR"}`, 422,
			"assets.impairment_account_missing"},
		"a planned asset": {planned["id"], loss("1.00", ""), 422, "assets.asset_not_capitalised"},
	} {
		t.Run(name, func(t *testing.T) { e.refuseImpairment(t, c.asset, c.body, c.status, c.code) })
	}
	e.clocked.Problem(404, "request.not_found", "POST", "/v1/assets/"+uuid.NewString()+":impair", "{"+loss("1.00", "")+"}", `"1"`)
	e.clocked.Problem(428, "request.precondition_required", "POST", "/v1/assets/"+mill+":impair", "{"+loss("1.00", "")+"}", "")

	_, tag := e.Must(200, "GET", fmt.Sprint("/v1/assets/", planned["id"]), "", "")
	e.Must(200, "POST", fmt.Sprint("/v1/assets/", planned["id"], ":cancel"), "", tag)
	e.refuseImpairment(t, planned["id"], loss("1.00", ""), 422, "assets.asset_cancelled")
	e.retire(t, land["id"], `"kind": "sale", "proceeds": {"amount": "500.00", "currency": "EUR"}`) // no gain or loss: no line wants a cost center
	e.refuseImpairment(t, land["id"], loss("1.00", ""), 422, "assets.asset_retired")

	_, july := e.start(t, e.l0, 7)
	e.carry(t, july)
	e.refuseImpairment(t, mill, loss("1.00", ""), 409, "assets.later_value_date")
}

// TestImpairmentChangeInEstimateLater: a change in estimate from a later
// period leaves an impairment of this one free (it keeps the residual
// value as it is), and the later period depreciates the impaired amount
// on the changed terms.
func TestImpairmentChangeInEstimateLater(t *testing.T) {
	e := newImpairments(t)
	mill, _ := e.depreciable(t, "Mill")
	_, june := e.start(t, e.l0, 6)
	e.carry(t, june)
	_, tag := e.Must(200, "GET", "/v1/assets/"+mill, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+mill, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 60,
		"effective_from": {"fiscal_year": 2026, "period": 7}}]}`, e.l0), tag)
	e.impair(t, mill, `"kind": "loss", "amount": {"amount": "290.00", "currency": "EUR"}`)
	s, _ := e.Must(200, "GET", fmt.Sprintf("/v1/assets/%s/schedule?fiscal_year=2026&ledger_id=%s", mill, e.l0), "", "")
	want(t, "July: 900.00 over the 59 periods left of 60", schedule(s, 7)[:22], "7 15.25 0.00 884.75; 8")
}

// TestImpairmentReversalAfterRetirement: a retirement or transfer is not
// valued in an impaired period (the impairment applies at its end, the
// share at its start); in the next period one takes half of the impaired
// asset, its catch-up on the impaired amount; once it has, what its
// accumulated depreciation held of the impairment is not known apart, so
// the impairment is not reversed; a loss is still posted, after which the
// retirement, no longer the asset's latest posting, is not reversed.
func TestImpairmentReversalAfterRetirement(t *testing.T) {
	e := newImpairments(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	e.impair(t, mill, `"kind": "loss", "amount": {"amount": "200.00", "currency": "EUR"}`)
	e.clocked.Problem(409, "assets.later_value_date", "POST", "/v1/asset-retirements", fmt.Sprintf(`{"asset_id": %q, "kind": "scrapping"}`, mill), "")
	e.clocked.Problem(409, "assets.later_value_date", "POST", "/v1/asset-transfers",
		fmt.Sprintf(`{"asset_id": %q, "kind": "asset", "to_asset_id": %q}`, mill, press), "")
	july := `"posting_date": "2026-07-15", "value_date": "2026-07-15"`
	q := e.retire(t, mill, `"kind": "scrapping", "share": {"percent": "50"}, `+july)
	// June depreciated 10.00 of 1,200.00, then 990.00 left after the loss
	// over 119 periods; half of cost 1,200.00 and of accumulated 210.00.
	want(t, "the half retired, its impairment with it", retired(q)[:41], "0L 0.5 10.00 600.00 -105.00 0.00 495.00 -")
	e.refuseImpairment(t, mill, `"kind": "reversal", "amount": {"amount": "1.00", "currency": "EUR"}, `+july, 409, "assets.impairment_reversal_after_retirement")
	e.impair(t, mill, `"kind": "loss", "amount": {"amount": "100.00", "currency": "EUR"}, `+july)
	e.refuseRetirementReversal(t, q, `{"reason": "r"}`, 409, "assets.later_movements")
}

// TestImpairmentInAParallelLedger: an impairment in 2L posts there alone,
// on the declining balance schedule's carrying amount, and an acquisition
// is no longer reversed once an asset was impaired.
func TestImpairmentInAParallelLedger(t *testing.T) {
	e := newImpairments(t)
	mill, acquired := e.depreciable(t, "Mill")
	q := e.impair(t, mill, fmt.Sprintf(`"kind": "loss", "ledger_id": %q, "amount": {"amount": "180.00", "currency": "EUR"}`, e.l2))
	want(t, "the loss in 2L", impaired(q), "loss 2L 2026-06-15 2026-06-15 2026/6 180.00 EUR 195.65 USD 1180.00 1000.00 1180.00 2000 true Mill")
	want(t, "its entry", entryLines(t, entryOf(q, "2L")), "2L 695000 1000 PC1 18000 19565; 2L 160900 - PC1 -18000 -19565")
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+acquired, "", "")
	e.clocked.Problem(409, "assets.later_movements", "POST", "/v1/asset-acquisitions/"+acquired+":reverse", `{"reason": "r"}`, tag)
}

// TestListAssetImpairments: the impairments of the caller's companies by
// posting date, filtered by company, asset, ledger, kind and posting
// dates, paged.
func TestListAssetImpairments(t *testing.T) {
	e := newImpairments(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	a := e.impair(t, mill, `"kind": "loss", "amount": {"amount": "100.00", "currency": "EUR"}`)
	b := e.impair(t, press, fmt.Sprintf(`"kind": "loss", "ledger_id": %q, "amount": {"amount": "50.00", "currency": "EUR"}`, e.l2))
	c := e.impair(t, mill, `"kind": "reversal", "amount": {"amount": "40.00", "currency": "EUR"}`)
	ids := func(query string) string {
		page, _ := e.Must(200, "GET", "/v1/asset-impairments"+query, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return strings.Join(out, " ")
	}
	for name, cs := range map[string]struct{ query, want string }{
		"all":             {"", fmt.Sprint(a["id"], " ", b["id"], " ", c["id"])},
		"of the mill":     {"?asset_id=" + mill, fmt.Sprint(a["id"], " ", c["id"])},
		"in 2L":           {"?ledger_id=" + e.l2.String(), fmt.Sprint(b["id"])},
		"reversals":       {"?kind=reversal", fmt.Sprint(c["id"])},
		"of company 2000": {"?legal_entity_id=" + e.DE.String() + "&posting_date_from=2026-06-15&posting_date_to=2026-06-15", fmt.Sprint(a["id"], " ", b["id"], " ", c["id"])},
		"of company 1000": {"?legal_entity_id=" + e.US.String(), ""},
		"before June 15":  {"?posting_date_to=2026-06-14", ""},
		"by creation":     {"?order_by=created_at", fmt.Sprint(a["id"], " ", b["id"], " ", c["id"])},
	} {
		t.Run(name, func(t *testing.T) { want(t, name, ids(cs.query), cs.want) })
	}
	first, _ := e.Must(200, "GET", "/v1/asset-impairments?page_size=2", "", "")
	next, _ := e.Must(200, "GET", "/v1/asset-impairments?page_size=2&page_token="+first["next_page_token"].(string), "", "")
	want(t, "the second page", next["items"].([]any)[0].(map[string]any)["id"], c["id"])
	created, _ := e.Must(200, "GET", "/v1/asset-impairments?page_size=1&order_by=created_at", "", "")
	e.Must(200, "GET", "/v1/asset-impairments?page_size=1&order_by=created_at&page_token="+created["next_page_token"].(string), "", "")
}

// allLedgers is a ledger whose companies keep every ledger of the
// tenant, 3L among them, in which the assets have no terms.
type allLedgers struct{ assets.Ledger }

func (l allLedgers) Ledgers(ctx context.Context, _ *uuid.UUID) ([]assets.LedgerRef, error) {
	return l.Ledger.Ledgers(ctx, nil)
}

// TestImpairmentFailures: an asset without terms in a ledger its company
// keeps is not impaired there (422 assets.ledger_terms_missing); a
// schedule that cannot be walked fails the impairment, never a panic;
// and each handler fails cleanly with each of its database statements
// failing in turn: a loss, a reversal, a read and the list.
func TestImpairmentFailures(t *testing.T) {
	e := newImpairments(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages, Now: func() time.Time { return assetDay }})
	mill, _ := e.depreciable(t, "Mill")
	asset := uuid.MustParse(mill)
	q := e.impair(t, mill, `"kind": "loss", "amount": {"amount": "100.00", "currency": "EUR"}`)
	id := uuid.MustParse(q["id"].(string))
	eur := func(a string) gen.Money { return gen.Money{Amount: a, Currency: "EUR"} }

	all := app.Assets(app.Config{Now: func() time.Time { return assetDay }})
	all.Ledger = allLedgers{all.Ledger}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := all.ImpairAsset(ctx, &gen.AssetImpairmentCreate{Kind: "loss", Amount: eur("1.00"), LedgerID: gen.NewOptUUID(e.l3)},
			gen.ImpairAssetParams{ID: asset, IfMatch: "*"})
		return err
	})
	pgtest.WantErr(t, err, "assets.ledger_terms_missing")

	for name, fn := range map[string]func(ctx context.Context) error{
		"a loss": func(ctx context.Context) error {
			_, err := h.ImpairAsset(ctx, &gen.AssetImpairmentCreate{Kind: "loss", Amount: eur("10.00")}, gen.ImpairAssetParams{ID: asset, IfMatch: "*"})
			return err
		},
		"a reversal": func(ctx context.Context) error {
			_, err := h.ImpairAsset(ctx, &gen.AssetImpairmentCreate{Kind: "reversal", Amount: eur("10.00")}, gen.ImpairAssetParams{ID: asset, IfMatch: "*"})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetAssetImpairment(ctx, gen.GetAssetImpairmentParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListAssetImpairments(ctx, gen.ListAssetImpairmentsParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
				t.Errorf("outside a unit of work: got %v, want it refused", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}

	_, tag := e.Must(200, "GET", "/v1/assets/"+mill, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+mill, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 2, "declining_factor": "10"}]}`, e.l2), tag)
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, mill)
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, cost, cost_gc, document_kind, document_id, created_by)
		SELECT tenant_id, gen_random_uuid(), id, last_seq, legal_entity_id, $2, class_code, 'acquisition', '2026-06-10', '2026-06-10', 2026, 6,
			2000000000000000000, 2000000000000000000, 'acquisition', gen_random_uuid(), $3
		FROM assets.asset WHERE id = $1`, mill, e.l2, e.Tenant.Human.ID)
	e.refuseImpairment(t, mill, fmt.Sprintf(`"kind": "loss", "ledger_id": %q, "amount": {"amount": "1.00", "currency": "EUR"}`, e.l2), 500,
		"platform.internal")
}
