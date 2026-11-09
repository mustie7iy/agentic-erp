package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// estimates are an asset's changes in estimate in its first ledger, one
// each: "year/period from: method life factor residual (before: method
// life factor residual)".
func estimates(a map[string]any) string {
	var out []string
	for _, c := range a["terms"].([]any)[0].(map[string]any)["estimate_changes"].([]any) {
		c := c.(map[string]any)
		b := c["before"].(map[string]any)
		out = append(out, fmt.Sprintf("%v/%v %v: %v %v %v %s (before: %v %v %v %s)", c["fiscal_year"], c["period"], c["effective_from"], c["method"],
			c["useful_life_months"], c["declining_factor"], amount(c["residual_value"]), b["method"], b["useful_life_months"], b["declining_factor"],
			amount(b["residual_value"])))
	}
	return strings.Join(out, "; ")
}

// estimate is a PATCH body changing the terms in the ledger in estimate
// from 2026's period, with the fields given (JSON members after a comma).
func estimate(ledger uuid.UUID, period int, fields string) string {
	return fmt.Sprintf(`{"terms": [{"ledger_id": %q, "effective_from": {"fiscal_year": 2026, "period": %d}%s}]}`, ledger, period, fields)
}

// TestChangeInEstimate (T57j): after June's and July's runs, the mill's
// useful life and residual value in 0L change from September; August
// keeps the old terms and September on depreciates the book value then
// over the life left, with nothing booked changing; the runs post so; a
// later change of method from November, and changes refused: not
// prospective, before the latest one, without effective_from, in a
// ledger nothing was posted in, with a new depreciation start, a special
// period, a year not set up.
func TestChangeInEstimate(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	path := "/v1/assets/" + mill
	for _, period := range []int{6, 7} {
		_, run := e.start(t, e.l0, period)
		e.carry(t, run)
	}
	_, tag := e.Must(200, "GET", path, "", "")
	for name, c := range map[string]struct {
		status     int
		code, body string
	}{
		"a booked period":                   {422, "assets.change_not_prospective", estimate(e.l0, 7, `, "useful_life_months": 60`)},
		"without effective_from":            {409, "assets.terms_in_use", fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 60}]}`, e.l0)},
		"a ledger nothing was posted in":    {422, "request.validation_failed", estimate(e.l2, 9, `, "useful_life_months": 60`)},
		"a new depreciation start":          {422, "request.validation_failed", estimate(e.l0, 9, `, "depreciation_start": "2026-07-01"`)},
		"a special period":                  {422, "assets.special_period", estimate(e.l0, 13, `, "useful_life_months": 60`)},
		"a year not set up":                 {422, "ledger.no_fiscal_period", strings.Replace(estimate(e.l0, 1, `, "useful_life_months": 60`), "2026", "2031", 1)},
		"a residual value in another money": {422, "request.validation_failed", estimate(e.l0, 9, `, "residual_value": {"amount": "1.00", "currency": "USD"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "PATCH", path, c.body, tag)
		})
	}
	same, sameTag := e.Must(200, "PATCH", path, estimate(e.l0, 9, `, "useful_life_months": 120`), tag)
	want(t, "a change in estimate changing nothing", fmt.Sprint(sameTag, " ", estimates(same)), tag+" ")

	a, tag := e.Must(200, "PATCH", path, estimate(e.l0, 9, `, "useful_life_months": 60, "residual_value": {"amount": "180.00", "currency": "EUR"}`), tag)
	want(t, "the terms from September", terms(a), "0L Leading ledger straight_line 60 <nil> 180.00 EUR <nil>; 2L HGB declining_balance 120 2 0.00 EUR <nil>")
	want(t, "the change", estimates(a), "2026/9 2026-09-01: straight_line 60 <nil> 180.00 (before: straight_line 120 <nil> 0.00)")
	change := a["terms"].([]any)[0].(map[string]any)["estimate_changes"].([]any)[0].(map[string]any)
	want(t, "who recorded it", change["created_by"], e.Tenant.Human.ID)
	var audited string
	pgtest.Scan(t, `SELECT (detail->'terms'->0->>'estimate_changes') || ' ' || (detail->'terms'->0->>'effective_from') FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'assets.asset.change' AND object_id = $2 ORDER BY seq DESC LIMIT 1`, []any{e.Tenant.ID, mill}, &audited)
	want(t, "the audit record", audited, "1 2026-09-01")
	s, _ := e.Must(200, "GET", path+"/schedule?fiscal_year=2026", "", "")
	want(t, "the schedule's terms, December's, and its changes", fmt.Sprint(s["useful_life_months"], " ", amount(s["residual_value"]), " ",
		len(s["estimate_changes"].([]any))), "60 180.00 1")
	want(t, "the schedule: August on the old terms, September on over the 57 periods left", schedule(s, 6),
		"6 10.00 10.00 1190.00; 7 10.00 10.00 1180.00; 8 10.00 0.00 1170.00; 9 17.37 0.00 1152.63; 10 17.37 0.00 1135.26; "+
			"11 17.37 0.00 1117.89; 12 17.37 0.00 1100.52 | 99.48 20.00")
	for _, c := range []struct {
		period int
		posted string
	}{{8, "10.00"}, {9, "17.37"}} {
		_, run := e.start(t, e.l0, c.period)
		want(t, fmt.Sprint("period ", c.period, "'s run"), runOf(e.carry(t, run)), fmt.Sprintf("posted 1 1 0 %s; 1 posted 1 1 0 %s", c.posted, c.posted))
	}

	_, tag = e.Must(200, "GET", path, "", "")
	e.Problem(422, "assets.change_not_prospective", "PATCH", path, estimate(e.l0, 9, `, "useful_life_months": 50`), tag)
	a, tag = e.Must(200, "PATCH", path, estimate(e.l0, 11, `, "method": "declining_balance", "declining_factor": "2"`), tag)
	want(t, "two changes", estimates(a), "2026/9 2026-09-01: straight_line 60 <nil> 180.00 (before: straight_line 120 <nil> 0.00); "+
		"2026/11 2026-11-01: declining_balance 60 2 180.00 (before: straight_line 60 <nil> 180.00)")
	e.Problem(422, "assets.change_not_prospective", "PATCH", path, estimate(e.l0, 10, `, "useful_life_months": 50`), tag)
	s, _ = e.Must(200, "GET", path+"/schedule?fiscal_year=2026", "", "")
	// November: the greater of 1135.26 x 2 / 60 = 37.84 and (1135.26 -
	// 180.00) / 55 = 17.37.
	want(t, "declining balance from November", schedule(s, 9), "9 17.37 17.37 1152.63; 10 17.37 0.00 1135.26; 11 37.84 0.00 1097.42; "+
		"12 36.58 0.00 1060.84 | 139.16 47.37")
}

// TestChangeInEstimateFailures calls a change in estimate, a
// classification as held for sale, the asset's
// read and the run's preview with each of their database statements
// failing in turn.
func TestChangeInEstimateFailures(t *testing.T) {
	e := newAcquisitions(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages, Now: func() time.Time { return assetDay }})
	asset, _ := e.depreciable(t, "Mill")
	e.depreciate(t, asset, e.l0, 6, 1000)
	pgtest.Exec(t, `UPDATE assets.asset_terms SET booked_through_year = 2026, booked_through_period = 6, version = version + 1
		WHERE asset_id = $1 AND ledger_id = $2`, asset, e.l0)
	_, tag := e.Must(200, "GET", "/v1/assets/"+asset, "", "")
	_, tag = e.Must(200, "PATCH", "/v1/assets/"+asset, estimate(e.l0, 8, `, "useful_life_months": 60`), tag)
	id := uuid.MustParse(asset)
	for name, fn := range map[string]func(ctx context.Context) error{
		"a change in estimate": func(ctx context.Context) error {
			_, err := h.UpdateAsset(ctx, &gen.AssetUpdate{Terms: []gen.AssetTermsUpdate{{LedgerID: e.l0, UsefulLifeMonths: gen.NewOptInt32(48),
				EffectiveFrom: gen.NewOptAssetEstimateFrom(gen.AssetEstimateFrom{FiscalYear: 2026, Period: 9})}}}, gen.UpdateAssetParams{ID: id, IfMatch: tag})
			return err
		},
		"a classification as held for sale": func(ctx context.Context) error {
			_, err := h.UpdateAsset(ctx, &gen.AssetUpdate{Terms: []gen.AssetTermsUpdate{{LedgerID: e.l0,
				HeldForSaleFrom: gen.NewOptAssetEstimateFrom(gen.AssetEstimateFrom{FiscalYear: 2026, Period: 9})}}}, gen.UpdateAssetParams{ID: id, IfMatch: tag})
			return err
		},
		"the asset": func(ctx context.Context) error {
			_, err := h.GetAsset(ctx, gen.GetAssetParams{ID: id})
			return err
		},
		"the preview": func(ctx context.Context) error {
			_, err := h.PreviewDepreciationRun(ctx, &gen.DepreciationRunPreviewRequest{LegalEntityID: e.DE, FiscalYear: 2026, Period: 8},
				gen.PreviewDepreciationRunParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
}

// TestRetirementBeforeAChange (T57j): a change in estimate's residual
// value is the asset's as it stood, so a retirement or transfer valued
// before the change's period is refused once the change is made; with
// the change from the retirement's own period, the share takes the
// change's residual value.
func TestRetirementBeforeAChange(t *testing.T) {
	e := newRuns(t)
	changed := func(period int, residual string) string {
		asset, _ := e.depreciable(t, "Mill")
		pgtest.Exec(t, `UPDATE assets.asset_terms SET booked_through_year = 2026, booked_through_period = 5, version = version + 1 WHERE asset_id = $1`, asset)
		_, tag := e.Must(200, "GET", "/v1/assets/"+asset, "", "")
		e.Must(200, "PATCH", "/v1/assets/"+asset, estimate(e.l0, period, `, "residual_value": {"amount": "`+residual+`", "currency": "EUR"}`), tag)
		return asset
	}
	scrap := func(asset string) string {
		return fmt.Sprintf(`{"asset_id": %q, "kind": "scrapping", "share": {"percent": "50"}}`, asset)
	}
	e.clocked.Problem(409, "assets.later_value_date", "POST", "/v1/asset-retirements", scrap(changed(7, "200.00")), "")
	june := changed(6, "100.00")
	e.clocked.Must(201, "POST", "/v1/asset-retirements", scrap(june), "")
	var residual string
	pgtest.Scan(t, `SELECT string_agg(g.code || ' ' || m.residual, ', ' ORDER BY g.code) FROM assets.movement m JOIN ledger.ledger g ON g.id = m.ledger_id
		WHERE m.asset_id = $1 AND m.kind = 'retirement'`, []any{june}, &residual)
	want(t, "the residual value the half took", residual, "0L -5000, 2L 0")
}
