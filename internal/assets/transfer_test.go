package assets_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// land is a planned asset of class LAND (company 2000; its depreciation
// account takes no cost object) on profit center PC1.
func (e assetEnv) land(t *testing.T) string {
	t.Helper()
	a, _ := e.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "LAND", "description": "Plot", "profit_center_id": %q}`, e.DE, e.c.Profit))
	return a["id"].(string)
}

// TestAssetTransfer: the mill (residual value 120.00 in 0L), its June run
// posted, moves half to a planned plot of class LAND (a reclassification):
// June's depreciation is posted back first, the share's cost, accumulated
// depreciation and residual value go to the plot at their booked group
// amounts, each side on its own profit center, and the plot becomes active
// from the transfer's value date. The rest moves in full, which retires
// the mill; reversing that makes it active again, and the first is then no
// longer the mill's latest posting. A planned shed receiving a share
// becomes active from it, and planned again by its reversal.
func TestAssetTransfer(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	_, tag := e.Must(200, "GET", "/v1/assets/"+mill, "", "")
	e.Must(200, "PATCH", "/v1/assets/"+mill, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "residual_value": {"amount": "120.00", "currency": "EUR"}}]}`, e.l0), tag)
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	plot := e.land(t)
	half := e.transfer(t, mill, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "50"}, "value_date": "2026-06-12"`, plot))
	want(t, "the transfer", fmt.Sprint(half["kind"], " ", half["to_asset_description"], " ", half["to_class_code"], " ", half["full"], " ", half["value_date"]),
		"asset Plot LAND false 2026-06-12")
	want(t, "what it took", moved(half), "0L -9.00 2026-06-15 600.00 0.00 60.00 | -9.78 652.18 0.00; 2L 0.00 2026-06-15 600.00 0.00 0.00 | 0.00 652.18 0.00")
	main, _ := partOf(half, "0L", "main")
	want(t, "the 0L entry", entryLines(t, main), "0L 620000 1000 PC1 -900 -978; 0L 160900 - PC1 900 978; 0L 160000 - PC1 -60000 -65218; 0L 160000 - PC1 60000 65218")
	want(t, "the plot", e.asset(t, plot), "active 2026-06-12; 0L 600.00 EUR 652.18 USD 0.00 EUR 0.00 USD 600.00 EUR 652.18 USD; "+
		"2L 600.00 EUR 652.18 USD 0.00 EUR 0.00 USD 600.00 EUR 652.18 USD")
	var residual string
	pgtest.Scan(t, `SELECT string_agg(kind || ' ' || residual, ', ' ORDER BY asset_id = $2, kind) FROM assets.movement m JOIN ledger.ledger g ON g.id = m.ledger_id
		WHERE document_id = $1 AND g.code = '0L'`, []any{half["id"], plot}, &residual)
	want(t, "the residual value carried", residual, "depreciation 0, transfer_out -6000, transfer_in 6000")

	rest := e.transfer(t, mill, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q`, plot))
	want(t, "the full transfer", fmt.Sprint(rest["full"], " ", e.asset(t, mill)[:7]), "true retired")
	changed := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_changed"})
	want(t, "AssetChanged", fmt.Sprint(changed[len(changed)-1]["change"]), "retired")
	posted := slices.DeleteFunc(apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_posted"}),
		func(ev map[string]any) bool { return ev["document_kind"] != "transfer" })
	want(t, "AssetMovementPosted", fmt.Sprint(len(posted), " ", posted[1]["transfer_kind"], " ", posted[1]["to_asset_id"], " ", posted[1]["asset_status"]),
		fmt.Sprint("2 asset ", plot, " retired"))

	e.untransfer(t, rest, `{"reason": "r"}`)
	want(t, "the mill, active again", e.asset(t, mill)[:6], "active")
	changed = apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_changed"})
	want(t, "AssetChanged", fmt.Sprint(changed[len(changed)-1]["change"]), "reactivated")
	e.refuseTransferReversal(t, half, `{"reason": "r"}`, 409, "assets.later_movements")

	press, _ := e.depreciable(t, "Press")
	shed := e.land(t)
	q := e.transfer(t, press, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "10"}`, shed))
	want(t, "the shed", e.asset(t, shed)[:17], "active 2026-06-15")
	e.untransfer(t, q, `{"reason": "r"}`)
	want(t, "the shed, planned again", e.asset(t, shed)[:14], "planned <nil>;")
	want(t, "the press, whole again", e.asset(t, press)[:45], "active 2026-06-10; 0L 1200.00 EUR 1304.35 USD")
	reversed := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_movement_reversed"})
	want(t, "AssetMovementReversed", fmt.Sprint(reversed[len(reversed)-1]["document_kind"], " ", reversed[len(reversed)-1]["to_asset_status"]),
		"transfer planned")
}

// TestAssetTransferReversalRefused: an asset transfer's reversal dated
// before it is refused, and once anything was posted on the receiver
// after it, so is its reversal; an acquisition's reversal is refused while
// a transfer posted after it stands.
func TestAssetTransferReversalRefused(t *testing.T) {
	e := newRuns(t)
	mill, acquired := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"cost": {"amount": "300.00", "currency": "EUR"}}`, press))
	want(t, "the share", q["share_cost"].(map[string]any)["amount"], "300.00")
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+acquired, "", "")
	e.clocked.Problem(409, "assets.later_movements", "POST", "/v1/asset-acquisitions/"+acquired+":reverse", `{"reason": "r"}`, tag)
	e.refuseTransferReversal(t, q, `{"reason": "r", "posting_date": "2026-06-14"}`, 422, "assets.reversal_before_original")
	entry := e.post(t, "2000", "2026-06-15", "EUR", nil, line{"169000", "10.00"}, line{"100000", "-10.00"})
	e.acquire(t, press, entry, 1, "")
	e.refuseTransferReversal(t, q, `{"reason": "r"}`, 409, "assets.later_movements")
}

// TestAssetTransferOfAReversedInvoice: cost from a supplier invoice
// transferred to another asset is out of reach of the invoice's reversal
// line there (assets.source_reversed).
func TestAssetTransferOfAReversedInvoice(t *testing.T) {
	e := newRuns(t)
	cc := fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)
	mill, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	press, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	invoice, entry := e.invoiceOf(t, "MILL-9", "2026-06-04")
	e.acquire(t, mill["id"], entry, 1, "")
	e.transfer(t, mill["id"], fmt.Sprintf(`"kind": "asset", "to_asset_id": "%v", "share": {"percent": "50"}`, press["id"]))
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+invoice.String(), "", "")
	inv, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+invoice.String()+":reverse", `{"reason": "Duplicate", "posting_date": "2026-06-15"}`, tag)
	rev := uuid.MustParse(inv["reversal_entry_id"].(string))
	e.clocked.Problem(422, "assets.source_reversed", "POST", "/v1/asset-acquisitions", acquisition(press["id"], rev, 1, ""), "")
}

// TestTransferRefused: the problems a transfer meets.
func TestTransferRefused(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	planned, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	cancelled, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	e.Must(200, "POST", fmt.Sprint("/v1/assets/", cancelled["id"], ":cancel"), "", tag)
	us, _ := e.create(t, e.body(e.US, "MACHINES", fmt.Sprintf(`, "profit_center_id": %q`, e.pc2)))
	late, _ := e.depreciable(t, "Late")
	entry := e.post(t, "2000", "2026-07-05", "EUR", nil, line{"169000", "100.00"}, line{"100000", "-100.00"})
	e.acquire(t, late, entry, 1, `, "posting_date": "2026-07-05"`)
	moved, _ := e.depreciable(t, "Moved")
	e.transfer(t, moved, fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-07-03"`, cc))
	bare, _ := e.depreciable(t, "Bare")
	pgtest.Exec(t, `DELETE FROM assets.asset_terms WHERE asset_id = $1 AND ledger_id = $2`, bare, e.l2)
	retired, _ := e.depreciable(t, "Retired")
	e.retire(t, retired, `"kind": "scrapping"`)
	plot := e.land(t)
	center := func(asset any, rest string) string {
		return fmt.Sprintf(`{"asset_id": "%v", "kind": "center"%s}`, asset, rest)
	}
	to := func(asset, receiver any, rest string) string {
		return fmt.Sprintf(`{"asset_id": "%v", "kind": "asset", "to_asset_id": "%v"%s}`, asset, receiver, rest)
	}
	cost := fmt.Sprintf(`, "cost_center_id": %q`, cc)
	for name, c := range map[string]struct {
		status     int
		code, body string
	}{
		"a center transfer mid-period":            {422, "assets.transfer_mid_period", center(mill, cost+`, "value_date": "2026-06-10"`)},
		"a center transfer in the period before":  {422, "assets.value_date_outside_period", center(mill, cost+`, "value_date": "2026-05-01"`)},
		"a center transfer before a later value":  {409, "assets.later_value_date", center(late, cost+`, "posting_date": "2026-06-20"`)},
		"the asset's own centers":                 {422, "request.validation_failed", center(mill, fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin))},
		"a profit center alone for a cost center": {422, "request.validation_failed", center(mill, fmt.Sprintf(`, "profit_center_id": %q`, e.c.Profit))},
		"no center": {422, "request.validation_failed", center(mill, "")},
		"no center for an asset without a cost center": {422, "request.validation_failed", center(plot, "")},
		"another profit center than the derived":       {422, "ledger.profit_center_mismatch", center(mill, cost+fmt.Sprintf(`, "profit_center_id": %q`, e.c.Profit))},
		"a locked cost center":                         {422, "ledger.cost_center_invalid", center(mill, fmt.Sprintf(`, "cost_center_id": %q`, e.c.Locked))},
		"a cancelled asset":                            {422, "assets.asset_cancelled", center(cancelled["id"], cost)},
		"a retired asset":                              {422, "assets.asset_retired", center(retired, cost)},
		"no terms in a ledger kept":                    {422, "assets.ledger_terms_missing", center(bare, cost)},
		"an unknown asset":                             {404, "request.not_found", center(uuid.Must(uuid.NewV7()), cost)},
		"another kind":                                 {422, "request.validation_failed", `{"asset_id": "` + mill + `", "kind": "sale"}`},
		"a center transfer to an asset":                {422, "request.validation_failed", center(mill, cost+`, "to_asset_id": "`+press+`"`)},
		"a center transfer with a share":               {422, "request.validation_failed", center(mill, cost+`, "share": {"percent": "5"}`)},
		"an asset transfer with centers":               {422, "request.validation_failed", to(mill, press, cost)},
		"an asset transfer without a receiver":         {422, "request.validation_failed", `{"asset_id": "` + mill + `", "kind": "asset"}`},
		"an asset transfer to itself":                  {422, "assets.transfer_same_asset", to(mill, mill, "")},
		"to another company":                           {422, "assets.transfer_other_company", to(mill, us["id"], "")},
		"of a planned asset":                           {422, "assets.asset_not_capitalised", to(planned["id"], mill, "")},
		"to a retired asset":                           {422, "assets.asset_retired", to(mill, retired, "")},
		"to an unknown asset":                          {404, "request.not_found", to(mill, uuid.Must(uuid.NewV7()), "")},
		"valued before capitalisation":                 {422, "assets.value_date_before_capitalisation", to(mill, press, `, "value_date": "2026-06-05"`)},
		"valued in the period before":                  {422, "assets.value_date_outside_period", to(mill, press, `, "value_date": "2026-05-31"`)},
		"to a receiver valued later":                   {409, "assets.later_value_date", to(mill, late, `, "posting_date": "2026-06-20"`)},
		"to a receiver on later centers":               {409, "assets.posting_before_center_transfer", to(mill, moved, `, "posting_date": "2026-06-20"`)},
		"to a receiver without terms":                  {422, "assets.ledger_terms_missing", to(mill, bare, "")},
		"a cost above the asset's":                     {422, "assets.share_exceeds", to(mill, press, `, "share": {"cost": {"amount": "1200.01", "currency": "EUR"}}`)},
		"a cost in another currency":                   {422, "request.validation_failed", to(mill, press, `, "share": {"cost": {"amount": "1.00", "currency": "USD"}}`)},
		"an empty share":                               {422, "request.validation_failed", to(mill, press, `, "share": {}`)},
	} {
		t.Run(name, func(t *testing.T) {
			e.clocked.In(t).Problem(c.status, c.code, "POST", "/v1/asset-transfers", c.body, "")
		})
	}
	e.Problem(404, "request.not_found", "GET", "/v1/asset-transfers/"+uuid.Must(uuid.NewV7()).String(), "", "")
}

// TestTransfersAtOnce: full transfers of two assets to each other at once
// lock both assets in id order, so neither deadlocks: one posts, the
// other finds its asset or receiver retired.
func TestTransfersAtOnce(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, pair := range [][2]string{{mill, press}, {press, mill}} {
		wg.Go(func() {
			codes[i], _, _ = e.clocked.Do("POST", "/v1/asset-transfers", fmt.Sprintf(`{"asset_id": %q, "kind": "asset", "to_asset_id": %q}`, pair[0], pair[1]), "")
		})
	}
	wg.Wait()
	want(t, "the transfers", fmt.Sprint(min(codes[0], codes[1]), " ", max(codes[0], codes[1])), "201 422")
}

// TestListAssetTransfers: the transfers of the caller's companies by
// posting date, filtered by company, asset (sender or receiver), kind,
// posting dates and status, paged.
func TestListAssetTransfers(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill, _ := e.depreciable(t, "Mill")
	press, _ := e.depreciable(t, "Press")
	moved := e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q`, cc))
	share := e.transfer(t, press, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "10"}`, mill))
	reversed := e.transfer(t, press, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "10"}`, mill))
	e.untransfer(t, reversed, `{"reason": "r"}`)
	ids := func(query string) string {
		page, _ := e.Must(200, "GET", "/v1/asset-transfers"+query, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return strings.Join(out, " ")
	}
	for name, c := range map[string]struct{ query, want string }{
		"all":               {"", fmt.Sprint(moved["id"], " ", share["id"], " ", reversed["id"])},
		"by company":        {"?legal_entity_id=" + e.US.String(), ""},
		"by sender":         {"?asset_id=" + press, fmt.Sprint(share["id"], " ", reversed["id"])},
		"by receiver":       {"?asset_id=" + mill + "&kind=asset", fmt.Sprint(share["id"], " ", reversed["id"])},
		"by kind":           {"?kind=center", fmt.Sprint(moved["id"])},
		"by status":         {"?status=reversed", fmt.Sprint(reversed["id"])},
		"posted from":       {"?posting_date_from=2026-06-16", ""},
		"posted to, by age": {"?posting_date_to=2026-06-15&order_by=created_at", fmt.Sprint(moved["id"], " ", share["id"], " ", reversed["id"])},
	} {
		t.Run(name, func(t *testing.T) { want(t, "the transfers", ids(c.query), c.want) })
	}
	byCreation, _ := e.Must(200, "GET", "/v1/asset-transfers?page_size=1&order_by=created_at", "", "")
	want(t, "a page by creation", byCreation["next_page_token"] != nil, true)
	first, _ := e.Must(200, "GET", "/v1/asset-transfers?page_size=2", "", "")
	next, _ := e.Must(200, "GET", "/v1/asset-transfers?page_size=2&page_token="+first["next_page_token"].(string), "", "")
	want(t, "the second page", next["items"].([]any)[0].(map[string]any)["id"], reversed["id"])
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/asset-transfers?order_by=kind", "", "")
	got, _ := e.Must(200, "GET", fmt.Sprint("/v1/asset-transfers/", moved["id"]), "", "")
	want(t, "a center transfer read", fmt.Sprint(got["from"].(map[string]any)["cost_center_name"], " -> ", got["to"].(map[string]any)["cost_center_name"]),
		"CC 1000 -> CC 4000")
}

// TestTransferDatabaseFailures calls each transfer handler with each of
// its database statements failing in turn: a center transfer with a
// catch-up of its own, an asset transfer, the reversals of both kinds, a
// read and the list.
func TestTransferDatabaseFailures(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages, Now: func() time.Time { return assetDay }})
	mill := uuid.MustParse(e.january(t))
	press, _ := e.depreciable(t, "Press")
	reversible := e.transfer(t, e.january(t), fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10"`, cc))
	id := uuid.MustParse(reversible["id"].(string))
	lathe, _ := e.depreciable(t, "Lathe")
	share := e.transfer(t, lathe, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "10"}`, press))
	shareID := uuid.MustParse(share["id"].(string))
	april := gen.NewOptDate(time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC))
	for name, fn := range map[string]func(ctx context.Context) error{
		"a center transfer": func(ctx context.Context) error {
			_, err := h.CreateAssetTransfer(ctx, &gen.AssetTransferCreate{AssetID: mill, Kind: "center", CostCenterID: gen.NewOptUUID(cc), PostingDate: april},
				gen.CreateAssetTransferParams{})
			return err
		},
		"an asset transfer": func(ctx context.Context) error {
			_, err := h.CreateAssetTransfer(ctx, &gen.AssetTransferCreate{AssetID: uuid.MustParse(press), Kind: "asset", ToAssetID: gen.NewOptUUID(mill),
				Share: gen.NewOptAssetRetirementShare(gen.AssetRetirementShare{Percent: gen.NewOptString("25")})}, gen.CreateAssetTransferParams{})
			return err
		},
		"a reversal": func(ctx context.Context) error {
			_, err := h.ReverseAssetTransfer(ctx, &gen.AssetTransferReverse{Reason: "r"}, gen.ReverseAssetTransferParams{ID: id, IfMatch: "*"})
			return err
		},
		"an asset transfer's reversal": func(ctx context.Context) error {
			_, err := h.ReverseAssetTransfer(ctx, &gen.AssetTransferReverse{Reason: "r"}, gen.ReverseAssetTransferParams{ID: shareID, IfMatch: "*"})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetAssetTransfer(ctx, gen.GetAssetTransferParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListAssetTransfers(ctx, gen.ListAssetTransfersParams{})
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
}

// periodUnknown is a ledger whose period reads fail with an error of the
// machinery, or whose group amounts do.
type periodUnknown struct {
	assets.Ledger
	groupFails bool
}

func (l periodUnknown) PeriodOpen(context.Context, uuid.UUID, uuid.UUID, int16, int16) error {
	return errRuns
}

func (l periodUnknown) GroupAmounts(ctx context.Context, from, to string, date time.Time, amounts []money.Minor) ([]money.Minor, error) {
	if l.groupFails {
		return nil, errRuns
	}
	return l.Ledger.GroupAmounts(ctx, from, to, date, amounts)
}

// landGone is the active document without class LAND.
type landGone struct{ assets.Settings }

func (c landGone) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := c.Settings.Document(ctx)
	if err == nil {
		doc.AssetClasses = slices.DeleteFunc(doc.AssetClasses, func(a config.AssetClass) bool { return a.Code == "LAND" })
	}
	return doc, n, err
}

// TestTransferReadsFail: a transfer whose period status, group amounts,
// group currency or class cannot be read fails rather than posting, and
// so does a reversal whose catch-up's period status cannot be read.
func TestTransferReadsFail(t *testing.T) {
	e := newRuns(t)
	cc := e.otherCenter(t)
	mill := e.january(t)
	press, _ := e.depreciable(t, "Press")
	reversible := e.transfer(t, e.january(t), fmt.Sprintf(`"kind": "center", "cost_center_id": %q, "posting_date": "2026-04-10"`, cc))
	april := gen.NewOptDate(time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC))
	center := &gen.AssetTransferCreate{AssetID: uuid.MustParse(mill), Kind: "center", CostCenterID: gen.NewOptUUID(cc), PostingDate: april}
	share := &gen.AssetTransferCreate{AssetID: uuid.MustParse(press), Kind: "asset", ToAssetID: gen.NewOptUUID(uuid.MustParse(mill))}
	toLand := &gen.AssetTransferCreate{AssetID: uuid.MustParse(press), Kind: "asset", ToAssetID: gen.NewOptUUID(uuid.MustParse(e.land(t)))}
	for name, c := range map[string]struct {
		change func(h *assets.Endpoints)
		req    *gen.AssetTransferCreate
		want   string
	}{
		"the period's status":  {func(h *assets.Endpoints) { h.Ledger = periodUnknown{Ledger: h.Ledger} }, center, errRuns.Error()},
		"a catch-up's rate":    {func(h *assets.Endpoints) { h.Ledger = periodUnknown{Ledger: h.Ledger, groupFails: true} }, center, errRuns.Error()},
		"a share's rate":       {func(h *assets.Endpoints) { h.Ledger = periodUnknown{Ledger: h.Ledger, groupFails: true} }, share, errRuns.Error()},
		"the group currency":   {func(h *assets.Endpoints) { h.Companies = noGroupCurrency{h.Companies} }, center, errRuns.Error()},
		"the asset's class":    {func(h *assets.Endpoints) { h.Settings = classGone{h.Settings} }, center, "MACHINES"},
		"the receiver's class": {func(h *assets.Endpoints) { h.Settings = landGone{h.Settings} }, toLand, "LAND"},
		"the company's zone":   {func(h *assets.Endpoints) { h.Settings = zoneLost{h.Settings} }, share, "Europe/Nowhere"},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Assets(app.Config{})
			c.change(h)
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreateAssetTransfer(ctx, c.req, gen.CreateAssetTransferParams{})
				return err
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	h := app.Assets(app.Config{})
	h.Ledger = periodUnknown{Ledger: h.Ledger}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ReverseAssetTransfer(ctx, &gen.AssetTransferReverse{Reason: "r"},
			gen.ReverseAssetTransferParams{ID: uuid.MustParse(reversible["id"].(string)), IfMatch: "*"})
		return err
	})
	if p := problem.From(err); err == nil || p.Status != 500 {
		t.Errorf("a reversal whose catch-up's period cannot be read: got %v, want it failed", err)
	}
}

// TestTransferRecordedInLedgerOrder: a transfer's audit record and
// AssetMovementPosted (one list, both) give its ledgers in the ledgers'
// order, not in the order the database groups its movements in
// (transferDetails; batch 50's fix forward, which T57h's events predate):
// posted with the ledger port's order and with it reversed, each in a
// transaction rolled back, on the same assets; its reversal's record,
// after the reversal wrote its rows, gives them in the port's order too,
// either way.
func TestTransferRecordedInLedgerOrder(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	plot := e.land(t)
	asset, to := uuid.MustParse(mill), uuid.MustParse(plot)
	share := gen.NewOptAssetRetirementShare(gen.AssetRetirementShare{Percent: gen.NewOptString("10")})
	for _, c := range []struct {
		name     string
		reversed bool
		want     string
	}{
		{"in the ledgers' order", false, ledgerList(e.l0, e.l2)},
		{"in the port's order, reversed", true, ledgerList(e.l2, e.l0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := app.Assets(app.Config{Now: func() time.Time { return assetDay }})
			if c.reversed {
				h.Ledger = ledgersReversed{h.Ledger}
			}
			got := recordedLedgers(t, e, "assets.transfer.post", "$.ledgers[*].ledger_id", func(ctx context.Context) error {
				_, err := h.CreateAssetTransfer(ctx, &gen.AssetTransferCreate{AssetID: asset, Kind: "asset", ToAssetID: gen.NewOptUUID(to), Share: share},
					gen.CreateAssetTransferParams{})
				return err
			})
			want(t, "the transfer's ledgers", got, c.want)
		})
	}
	q := e.transfer(t, mill, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "10"}`, plot))
	for _, c := range []struct {
		name     string
		reversed bool
		want     string
	}{
		{"its reversal in the ledgers' order", false, ledgerList(e.l0, e.l2)},
		{"its reversal in the port's order, reversed", true, ledgerList(e.l2, e.l0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := app.Assets(app.Config{Now: func() time.Time { return assetDay }})
			if c.reversed {
				h.Ledger = ledgersReversed{h.Ledger}
			}
			got := recordedLedgers(t, e, "assets.transfer.reverse", "$.ledgers[*].ledger_id", func(ctx context.Context) error {
				_, err := h.ReverseAssetTransfer(ctx, &gen.AssetTransferReverse{Reason: "r"},
					gen.ReverseAssetTransferParams{ID: uuid.MustParse(q["id"].(string)), IfMatch: "*"})
				return err
			})
			want(t, "the reversal's ledgers", got, c.want)
		})
	}
}
