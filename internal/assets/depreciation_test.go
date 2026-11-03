package assets_test

import (
	"context"
	"errors"
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
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// depreciable is an asset of company 2000 on the administration cost
// center whose 1,200.00 EUR line on the clearing account, posted on 10
// June 2026, was capitalised: depreciation from June, 10.00 a month in 0L
// (straight-line over 120 months) and from 20.00 in 2L (declining balance
// at twice the straight-line rate). It returns the asset's id and
// the acquisition's.
func (e assetEnv) depreciable(t *testing.T, description string) (string, string) {
	t.Helper()
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "1200.00"}, line{"100000", "-1200.00"})
	a, _ := e.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": %q, "cost_center_id": %q}`,
		e.DE, description, e.c.Admin))
	q := e.acquire(t, a["id"], entry, 1, "")
	return a["id"].(string), q["id"].(string)
}

// depreciate writes a depreciation movement of the asset in the ledger,
// posted on the last day of 2026's period with the asset's next seq, as a
// depreciation run (T57f) writes it.
func (e assetEnv) depreciate(t *testing.T, asset string, ledger uuid.UUID, period int, cents int64) {
	t.Helper()
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, asset)
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, accumulated, accumulated_gc, document_kind, document_id, created_by)
		SELECT tenant_id, gen_random_uuid(), id, last_seq, legal_entity_id, $2, class_code, 'depreciation', d, d, 2026, $3, -$4::bigint, -$4::bigint,
			'depreciation_run', gen_random_uuid(), $5
		FROM assets.asset, (SELECT (make_date(2026, $3, 1) + interval '1 month' - interval '1 day')::date AS d) x WHERE id = $1`,
		asset, ledger, period, cents, e.Tenant.Human.ID)
}

// schedule is a schedule's periods from the first, one line each:
// "period planned booked net-book-value", then the year's totals.
func schedule(s map[string]any, from int) string {
	var out []string
	for _, p := range s["periods"].([]any)[from-1:] {
		p := p.(map[string]any)
		out = append(out, fmt.Sprint(p["period"], " ", amount(p["planned"]), " ", amount(p["booked"]), " ", amount(p["net_book_value"])))
	}
	totals := s["totals"].(map[string]any)
	return strings.Join(out, "; ") + " | " + amount(totals["planned"]) + " " + amount(totals["booked"])
}

func amount(v any) string { return v.(map[string]any)["amount"].(string) }

// TestAssetSchedule: an asset's schedule of 2026 per ledger: nothing
// before its depreciation start in June, then straight-line in 0L and
// declining balance in 2L, the booked depreciation in the period it was
// posted, the net book value at each period's end, the year's totals; the
// leading ledger by default; refusals.
func TestAssetSchedule(t *testing.T) {
	e := newAcquisitions(t)
	asset, _ := e.depreciable(t, "CNC milling machine")
	e.depreciate(t, asset, e.l0, 6, 1000)
	s, _ := e.Must(200, "GET", "/v1/assets/"+asset+"/schedule?fiscal_year=2026", "", "")
	want(t, "the schedule's ledger", fmt.Sprint(s["ledger_code"], " ", s["method"], " ", s["useful_life_months"], " ", s["currency"], " ", s["depreciation_start"],
		" ", amount(s["residual_value"])), "0L straight_line 120 EUR 2026-06-10 0.00")
	want(t, "the asset", fmt.Sprint(s["asset_number"], " ", s["description"], " ", s["company_code"], " ", s["fiscal_year"]), "1 CNC milling machine 2000 2026")
	want(t, "0L", schedule(s, 5), "5 0.00 0.00 0.00; 6 10.00 10.00 1190.00; 7 10.00 0.00 1180.00; 8 10.00 0.00 1170.00; 9 10.00 0.00 1160.00; "+
		"10 10.00 0.00 1150.00; 11 10.00 0.00 1140.00; 12 10.00 0.00 1130.00 | 70.00 10.00")
	first := s["periods"].([]any)[0].(map[string]any)
	want(t, "the first period", fmt.Sprint(first["fiscal_year"], " ", first["start_date"], " ", first["end_date"]), "2026 2026-01-01 2026-01-31")
	s, _ = e.Must(200, "GET", fmt.Sprintf("/v1/assets/%s/schedule?fiscal_year=2026&ledger_id=%s", asset, e.l2), "", "")
	want(t, "2L", schedule(s, 6), "6 20.00 0.00 1180.00; 7 19.67 0.00 1160.33; 8 19.34 0.00 1140.99; 9 19.02 0.00 1121.97; "+
		"10 18.70 0.00 1103.27; 11 18.39 0.00 1084.88; 12 18.08 0.00 1066.80 | 133.20 0.00")
	want(t, "2L's factor", s["declining_factor"], "2")
	later, _ := e.depreciable(t, "CNC lathe")
	_, tag := e.Must(200, "GET", "/v1/assets/"+later, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+later, fmt.Sprintf(`{"depreciation_start": "2026-08-15", "terms": [{"ledger_id": %q,
		"residual_value": {"amount": "120.00", "currency": "EUR"}}]}`, e.l0), tag)
	s, _ = e.Must(200, "GET", "/v1/assets/"+later+"/schedule?fiscal_year=2026", "", "")
	want(t, "a depreciation start in August and a residual value", fmt.Sprint(s["depreciation_start"], " ", amount(s["residual_value"]), " ", schedule(s, 6)),
		"2026-08-15 120.00 6 0.00 0.00 1200.00; 7 0.00 0.00 1200.00; 8 9.00 0.00 1191.00; 9 9.00 0.00 1182.00; 10 9.00 0.00 1173.00; "+
			"11 9.00 0.00 1164.00; 12 9.00 0.00 1155.00 | 45.00 0.00")
	for name, c := range map[string]struct{ query, code string }{
		"a ledger the company does not keep": {"fiscal_year=2026&ledger_id=" + e.l3.String(), "assets.ledger_terms_missing"},
		"a year not set up":                  {"fiscal_year=2031", "ledger.no_fiscal_period"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(422, c.code, "GET", "/v1/assets/"+asset+"/schedule?"+c.query, "", "")
		})
	}
	e.Problem(404, "request.not_found", "GET", "/v1/assets/"+uuid.Must(uuid.NewV7()).String()+"/schedule?fiscal_year=2026", "", "")
}

// preview previews a run of company 2000 in 2026 and returns it.
func (e assetEnv) preview(t *testing.T, c apitest.Client, ledger uuid.UUID, period int, query string) map[string]any {
	t.Helper()
	p, _ := c.Must(200, "POST", "/v1/depreciation-runs/preview"+query, fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": 2026,
		"period": %d}`, e.DE, ledger, period), "")
	return p
}

// previewItems are a preview's assets, one line each: description,
// planned, booked, difference, class, centers and accounts.
func previewItems(p map[string]any) string {
	var out []string
	for _, it := range p["items"].([]any) {
		it := it.(map[string]any)
		out = append(out, fmt.Sprint(it["description"], ": ", amount(it["planned"]), " ", amount(it["booked"]), " ", amount(it["difference"]), " ",
			it["class_name"], " ", it["cost_center_code"], " ", it["profit_center_code"], " ", it["depreciation_account"].(map[string]any)["number"], " ",
			it["accumulated_account"].(map[string]any)["number"]))
	}
	return strings.Join(out, "; ")
}

// previewTotals are a preview's totals: the count and amounts, then each
// line as "account cost-center profit-center amount".
func previewTotals(p map[string]any) string {
	tot := p["totals"].(map[string]any)
	out := []string{fmt.Sprint(tot["assets"], " ", amount(tot["planned"]), " ", amount(tot["booked"]), " ", amount(tot["difference"]))}
	for _, l := range tot["lines"].([]any) {
		l := l.(map[string]any)
		out = append(out, fmt.Sprint(l["account_number"], " ", l["account_name"], " ", l["cost_center_code"], " ", l["profit_center_code"], " ", amount(l["amount"])))
	}
	return strings.Join(out, "; ")
}

// TestPreviewDepreciationRun: a run's preview selects the active assets
// whose depreciation has started and a planned asset still holding
// depreciation, shows each one's planned, booked and difference amounts
// on its accounts and centers, pages by asset number with the totals of
// all of them on every page; it passes over an asset booked through a
// later period or with a later center assignment; refusals.
func TestPreviewDepreciationRun(t *testing.T) {
	e := newAcquisitions(t)
	mill, _ := e.depreciable(t, "Mill")
	lathe, _ := e.depreciable(t, "Lathe")
	e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin))) // planned, never acquired
	press, acquisition := e.depreciable(t, "Press")
	e.depreciate(t, press, e.l0, 6, 1000)
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+acquisition, "", "")
	e.clocked.Must(200, "POST", "/v1/asset-acquisitions/"+acquisition+":reverse", `{"reason": "Not ours"}`, tag)
	want(t, "nothing in 2L in January", previewTotals(e.preview(t, e.Client, e.l2, 1, "")), "0 0.00 0.00 0.00")

	p := e.preview(t, e.Client, e.l0, 6, "")
	want(t, "the preview", fmt.Sprint(p["company_code"], " ", p["ledger_code"], " ", p["fiscal_year"], "/", p["period"], " ", p["period_start"], "..",
		p["period_end"], " ", p["currency"]), "2000 0L 2026/6 2026-06-01..2026-06-30 EUR")
	want(t, "June in 0L", previewItems(p), "Mill: 10.00 0.00 10.00 Machinery and equipment 1000 PC1 620000 160900; "+
		"Lathe: 10.00 0.00 10.00 Machinery and equipment 1000 PC1 620000 160900; Press: 0.00 10.00 -10.00 Machinery and equipment 1000 PC1 620000 160900")
	want(t, "June's totals in 0L", previewTotals(p), "3 20.00 10.00 10.00; 620000 Depreciation 1000 PC1 10.00; 160900 Machinery, accumulated depreciation <nil> PC1 -10.00")
	p = e.preview(t, e.Client, e.l2, 6, "")
	want(t, "June in 2L", previewItems(p)+" | "+previewTotals(p), "Mill: 20.00 0.00 20.00 Machinery and equipment 1000 PC1 620000 160900; "+
		"Lathe: 20.00 0.00 20.00 Machinery and equipment 1000 PC1 620000 160900 | 2 40.00 0.00 40.00; 620000 Depreciation 1000 PC1 40.00; "+
		"160900 Machinery, accumulated depreciation <nil> PC1 -40.00")
	p = e.preview(t, e.Client, e.l0, 6, "?page_size=2")
	want(t, "the first page", previewItems(p), "Mill: 10.00 0.00 10.00 Machinery and equipment 1000 PC1 620000 160900; "+
		"Lathe: 10.00 0.00 10.00 Machinery and equipment 1000 PC1 620000 160900")
	p = e.preview(t, e.Client, e.l0, 6, "?page_size=2&page_token="+p["next_page_token"].(string))
	want(t, "the second page", previewItems(p)+" | "+previewTotals(p), "Press: 0.00 10.00 -10.00 Machinery and equipment 1000 PC1 620000 160900 | "+
		"3 20.00 10.00 10.00; 620000 Depreciation 1000 PC1 10.00; 160900 Machinery, accumulated depreciation <nil> PC1 -10.00")
	want(t, "the last page's token", p["next_page_token"], nil)
	want(t, "centers to a reader of assets alone", previewItems(e.preview(t, e.assetsOnly(t), e.l0, 6, "?page_size=1")),
		"Mill: 10.00 0.00 10.00 Machinery and equipment <nil> <nil> 620000 160900")
	want(t, "January", previewTotals(e.preview(t, e.Client, e.l0, 1, "")), "1 0.00 10.00 -10.00; 620000 Depreciation 1000 PC1 -10.00; "+
		"160900 Machinery, accumulated depreciation <nil> PC1 10.00")

	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "600.00"}, line{"100000", "-600.00"})
	sales, _ := e.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "Van", "cost_center_id": %q}`, e.DE, e.c.Sales))
	e.acquire(t, sales["id"], entry, 1, "")
	want(t, "June's totals in 2L with an asset on the sales cost center, without a profit center", previewTotals(e.preview(t, e.Client, e.l2, 6, "")),
		"3 50.00 0.00 50.00; 620000 Depreciation 1000 PC1 40.00; 620000 Depreciation 2000 <nil> 10.00; "+
			"160900 Machinery, accumulated depreciation <nil> <nil> -10.00; 160900 Machinery, accumulated depreciation <nil> PC1 -40.00")
	pgtest.Exec(t, `UPDATE assets.asset SET status = 'retired', version = version + 1 WHERE id = $1`, sales["id"])

	e.depreciate(t, mill, e.l0, 6, 1000)
	pgtest.Exec(t, `UPDATE assets.asset_terms SET booked_through_year = 2026, booked_through_period = 7, version = version + 1 WHERE asset_id = $1 AND ledger_id = $2`, mill, e.l0)
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, lathe)
	pgtest.Exec(t, `INSERT INTO assets.asset_assignment (tenant_id, asset_id, seq, valid_from, cost_center_id, profit_center_id, created_by)
		SELECT a.tenant_id, a.id, a.last_seq, '2026-08-01', r.cost_center_id, r.profit_center_id, $2 FROM assets.asset a
		JOIN assets.asset_assignment r ON r.asset_id = a.id AND r.seq = 1 WHERE a.id = $1`, lathe, e.Tenant.Human.ID)
	want(t, "June, the mill booked through July and the lathe on centers from August", previewItems(e.preview(t, e.Client, e.l0, 6, "")),
		"Press: 0.00 10.00 -10.00 Machinery and equipment 1000 PC1 620000 160900")
	want(t, "July", previewItems(e.preview(t, e.Client, e.l0, 7, "")), "Mill: 20.00 10.00 10.00 Machinery and equipment 1000 PC1 620000 160900; "+
		"Press: 0.00 10.00 -10.00 Machinery and equipment 1000 PC1 620000 160900")
	want(t, "August", previewItems(e.preview(t, e.Client, e.l0, 8, "")), "Mill: 30.00 10.00 20.00 Machinery and equipment 1000 PC1 620000 160900; "+
		"Lathe: 30.00 0.00 30.00 Machinery and equipment 1000 PC1 620000 160900; "+
		"Press: 0.00 10.00 -10.00 Machinery and equipment 1000 PC1 620000 160900")

	for name, c := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"a special period":        {fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 13}`, e.DE), 422, "assets.special_period"},
		"a ledger not kept":       {fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": 2026, "period": 6}`, e.DE, e.l3), 422, "ledger.ledger_not_kept"},
		"a year not set up":       {fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2031, "period": 6}`, e.DE), 422, "ledger.no_fiscal_period"},
		"an unknown legal entity": {fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 6}`, uuid.Must(uuid.NewV7())), 404, "request.not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "POST", "/v1/depreciation-runs/preview", c.body, "")
		})
	}
	e.Problem(400, "request.invalid_page_token", "POST", "/v1/depreciation-runs/preview?page_token=x",
		fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 6}`, e.DE), "")
}

// movements are a page of an asset's movements, one line each: kind,
// ledger, posting period, cost, accumulated depreciation (local and
// group), its entry's number and whether it reverses or is reversed.
func movements(page map[string]any) string {
	var out []string
	for _, m := range page["items"].([]any) {
		m := m.(map[string]any)
		out = append(out, fmt.Sprint(m["seq"], " ", m["kind"], " ", m["ledger_code"], " ", m["fiscal_year"], "/", m["fiscal_period"], " ",
			moneyOf(m["cost"]), " ", moneyOf(m["cost_gc"]), " ", moneyOf(m["accumulated"]), " ", moneyOf(m["accumulated_gc"]), " ",
			m["document_kind"], " ", m["document_number"] != nil, " ", m["reverses_id"] != nil, " ", m["reversed_by_id"] != nil))
	}
	return strings.Join(out, "; ")
}

// brief are a page's movements as "seq kind ledger".
func brief(page map[string]any) string {
	var out []string
	for _, m := range page["items"].([]any) {
		m := m.(map[string]any)
		out = append(out, fmt.Sprint(m["seq"], " ", m["kind"], " ", m["ledger_code"]))
	}
	return strings.Join(out, "; ")
}

// TestAssetMovements: an asset's movements in posting order, each with
// its ledger, amounts, document and entry, a reversal and what it
// reverses linked both ways; filtered by ledger, year and kind; paged.
func TestAssetMovements(t *testing.T) {
	e := newAcquisitions(t)
	press, acquisition := e.depreciable(t, "Press")
	e.depreciate(t, press, e.l0, 6, 1000)
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+acquisition, "", "")
	e.clocked.Must(200, "POST", "/v1/asset-acquisitions/"+acquisition+":reverse", `{"reason": "Not ours"}`, tag)
	page, _ := e.Must(200, "GET", "/v1/assets/"+press+"/movements", "", "")
	want(t, "the movements", movements(page), "2 acquisition 0L 2026/6 1200.00 EUR 1200.00 EUR 0.00 EUR 0.00 EUR acquisition true false true; "+
		"2 acquisition 2L 2026/6 1200.00 EUR 1200.00 EUR 0.00 EUR 0.00 EUR acquisition true false true; "+
		"3 depreciation 0L 2026/6 0.00 EUR 0.00 EUR -10.00 EUR -10.00 EUR depreciation_run false false false; "+
		"4 acquisition 0L 2026/6 -1200.00 EUR -1200.00 EUR 0.00 EUR 0.00 EUR acquisition true true false; "+
		"4 acquisition 2L 2026/6 -1200.00 EUR -1200.00 EUR 0.00 EUR 0.00 EUR acquisition true true false")
	first := page["items"].([]any)[0].(map[string]any)
	want(t, "the first movement's document", fmt.Sprint(first["document_id"], " ", first["value_date"], " ", first["journal_entry_fiscal_year"]),
		acquisition+" 2026-06-10 2026")
	for name, c := range map[string]struct{ query, want string }{
		"in 2L":          {"?ledger_id=" + e.l2.String(), "2 acquisition 2L; 4 acquisition 2L"},
		"depreciation":   {"?kind=depreciation", "3 depreciation 0L"},
		"posted in 2026": {"?fiscal_year=2026&kind=depreciation", "3 depreciation 0L"},
		"newest first":   {"?order_by=seq%20desc&page_size=1", "4 acquisition 2L"},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := e.In(t).Must(200, "GET", "/v1/assets/"+press+"/movements"+c.query, "", "")
			want(t, "the movements", brief(got), c.want)
		})
	}
	page, _ = e.Must(200, "GET", "/v1/assets/"+press+"/movements?page_size=2", "", "")
	page, _ = e.Must(200, "GET", "/v1/assets/"+press+"/movements?page_size=2&page_token="+page["next_page_token"].(string), "", "")
	want(t, "the second page", len(page["items"].([]any)), 2)
	e.Problem(404, "request.not_found", "GET", "/v1/assets/"+uuid.Must(uuid.NewV7()).String()+"/movements", "", "")
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/assets/"+press+"/movements?order_by=kind", "", "")
}

// TestDepreciationReadFailures calls the schedule, the movements and the
// preview with each of their database statements failing in turn.
func TestDepreciationReadFailures(t *testing.T) {
	e := newAcquisitions(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages, Now: func() time.Time { return assetDay }})
	asset, _ := e.depreciable(t, "Mill")
	e.depreciable(t, "Lathe")
	id := uuid.MustParse(asset)
	e.depreciate(t, asset, e.l0, 6, 1000)
	preview := &gen.DepreciationRunPreviewRequest{LegalEntityID: e.DE, FiscalYear: 2026, Period: 6}
	for name, fn := range map[string]func(ctx context.Context) error{
		"the schedule": func(ctx context.Context) error {
			_, err := h.GetAssetSchedule(ctx, gen.GetAssetScheduleParams{ID: id, FiscalYear: 2026})
			return err
		},
		"the movements": func(ctx context.Context) error {
			_, err := h.ListAssetMovements(ctx, gen.ListAssetMovementsParams{ID: id})
			return err
		},
		"the preview": func(ctx context.Context) error {
			_, err := h.PreviewDepreciationRun(ctx, preview, gen.PreviewDepreciationRunParams{PageSize: gen.NewOptInt32(1)})
			return err
		},
		"the preview's periods": func(ctx context.Context) error {
			_, err := ledger.Calendar{LegalEntities: tenancy.Directory{}}.Periods(ctx, e.DE, 2026)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	// The contract refuses these before the handler; called directly, the
	// handler refuses them itself.
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.PreviewDepreciationRun(ctx, preview, gen.PreviewDepreciationRunParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	pgtest.WantErr(t, err, "request.invalid_page_size")
}

// classGone is the active document without the asset class MACHINES.
type classGone struct{ assets.Settings }

func (c classGone) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := c.Settings.Document(ctx)
	if err == nil {
		doc.AssetClasses = slices.DeleteFunc(doc.AssetClasses, func(a config.AssetClass) bool { return a.Code == "MACHINES" })
	}
	return doc, n, err
}

// settingsLost is a configuration that does not load.
type settingsLost struct{ assets.Settings }

func (settingsLost) Document(context.Context) (*config.Config, int32, error) {
	return nil, 0, errors.New("configuration lost")
}

// TestPreviewSettingsLost: a preview selecting an asset whose class the
// active configuration lost fails with the class's problem, as the run
// would, and one whose configuration does not load fails.
func TestPreviewSettingsLost(t *testing.T) {
	e := newAcquisitions(t)
	e.depreciable(t, "Mill")
	preview := &gen.DepreciationRunPreviewRequest{LegalEntityID: e.DE, FiscalYear: 2026, Period: 6}
	for name, c := range map[string]struct {
		settings assets.Settings
		want     string
	}{
		"a class lost":     {classGone{configuration.Settings{}}, "assets.class_unknown"},
		"no configuration": {settingsLost{}, "configuration lost"},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Assets(app.Config{})
			h.Settings = c.settings
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.PreviewDepreciationRun(ctx, preview, gen.PreviewDepreciationRunParams{})
				return err
			})
			if err == nil || !strings.Contains(fmt.Sprint(err, problemCode(err)), c.want) {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
}

// TestDepreciationOutOfRange: terms and movements whose depreciation is
// out of range fail the schedule and the preview rather than wrap: an
// asset of 2·10^18 minor units on declining balance at a factor of ten
// over two periods.
func TestDepreciationOutOfRange(t *testing.T) {
	e := newAcquisitions(t)
	asset, _ := e.depreciable(t, "Mill")
	_, tag := e.Must(200, "GET", "/v1/assets/"+asset, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+asset, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 2, "declining_factor": "10"}]}`, e.l2), tag)
	pgtest.Exec(t, `UPDATE assets.asset SET last_seq = last_seq + 1, version = version + 1 WHERE id = $1`, asset)
	pgtest.Exec(t, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date, posting_date,
			fiscal_year, fiscal_period, cost, cost_gc, document_kind, document_id, created_by)
		SELECT tenant_id, gen_random_uuid(), id, last_seq, legal_entity_id, $2, class_code, 'acquisition', '2026-06-10', '2026-06-10', 2026, 6,
			2000000000000000000, 2000000000000000000, 'acquisition', gen_random_uuid(), $3
		FROM assets.asset WHERE id = $1`, asset, e.l2, e.Tenant.Human.ID)
	e.Problem(500, "platform.internal", "GET", fmt.Sprintf("/v1/assets/%s/schedule?fiscal_year=2026&ledger_id=%s", asset, e.l2), "", "")
	e.Problem(500, "platform.internal", "POST", "/v1/depreciation-runs/preview",
		fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": 2026, "period": 6}`, e.DE, e.l2), "")
}
