package assets_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestUpdateAsset: a merge patch changes the description, the
// depreciation start in every ledger or one, and the terms per ledger,
// gives terms in a ledger the company came to keep later, and changes
// nothing when it changes nothing.
func TestUpdateAsset(t *testing.T) {
	e := newAssets(t)
	a, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	path := "/v1/assets/" + a["id"].(string)
	a, tag = e.Must(200, "PATCH", path, `{"description": "CNC milling machine, hall 2", "depreciation_start": "2026-07-01"}`, tag)
	want(t, "description", a["description"], "CNC milling machine, hall 2")
	want(t, "terms with a start", terms(a), "0L Leading ledger straight_line 120 <nil> 0.00 EUR 2026-07-01; 2L HGB declining_balance 120 2 0.00 EUR 2026-07-01")
	want(t, "ETag", tag, `"2"`)
	a, tag = e.Must(200, "PATCH", path, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "method": "declining_balance", "declining_factor": "2.50",
		"residual_value": {"amount": "1000.00", "currency": "EUR"}, "depreciation_start": null}, {"ledger_id": %q, "method": "straight_line",
		"useful_life_months": 96}]}`, e.l0, e.l2), tag)
	want(t, "changed terms", terms(a),
		"0L Leading ledger declining_balance 120 2.5 1000.00 EUR <nil>; 2L HGB straight_line 96 <nil> 0.00 EUR 2026-07-01")
	var audited string
	pgtest.Scan(t, `SELECT detail->'terms'->0->>'residual_value' || ' ' || (detail->'terms'->0->>'declining_factor') FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'assets.asset.change' AND object_id = $2 ORDER BY seq DESC LIMIT 1`, []any{e.Tenant.ID, a["id"]}, &audited)
	want(t, "the change's audit record, with the terms", audited, "100000 2.5")
	e.Problem(422, "request.validation_failed", "PATCH", path, `{"description": "   "}`, tag)
	same, sameTag := e.Must(200, "PATCH", path, fmt.Sprintf(`{"description": "CNC milling machine, hall 2", "terms": [{"ledger_id": %q, "declining_factor": "2.5"}]}`, e.l0), tag)
	want(t, "a patch changing nothing keeps the version", sameTag, tag)
	want(t, "and the asset", fmt.Sprint(same), fmt.Sprint(a))

	pgtest.Exec(t, `INSERT INTO ledger.company_ledger (tenant_id, legal_entity_id, ledger_id, is_leading, created_by) VALUES ($1, $2, $3, false, $4)`,
		e.Tenant.ID, e.DE, e.l3, e.Tenant.Human.ID)
	e.Problem(422, "request.validation_failed", "PATCH", path, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 60}]}`, e.l3), tag)
	a, tag = e.Must(200, "PATCH", path, fmt.Sprintf(`{"depreciation_start": "2026-08-01", "terms": [{"ledger_id": %q, "method": "declining_balance",
		"declining_factor": "3.0000", "useful_life_months": 60}]}`, e.l3), tag)
	later, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	want(t, "an asset created then has no terms in a ledger its class has none in", terms(later),
		"0L Leading ledger straight_line 120 <nil> 0.00 EUR <nil>; 2L HGB declining_balance 120 2 0.00 EUR <nil>")
	want(t, "terms in a ledger kept later", terms(a), "0L Leading ledger declining_balance 120 2.5 1000.00 EUR 2026-08-01; "+
		"2L HGB straight_line 96 <nil> 0.00 EUR 2026-08-01; 3L Tax declining_balance 60 3 0.00 EUR 2026-08-01")

	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_changed"})
	want(t, "events", len(evs), 5)
	want(t, "the change's event", fmt.Sprint(evs[1]["change"], " ", evs[1]["version"]), "changed 2")
	e.Problem(412, "request.version_mismatch", "PATCH", path, `{"description": "x"}`, `"1"`)

	pgtest.Exec(t, `UPDATE assets.asset_terms SET booked_through_year = 2026, booked_through_period = 6, version = version + 1
		WHERE asset_id = $1 AND ledger_id = $2`, a["id"], e.l2)
	read, _ := e.Must(200, "GET", path, "", "")
	booked := read["terms"].([]any)[1].(map[string]any)
	want(t, "booked through", fmt.Sprint(booked["booked_through_year"], "/", booked["booked_through_period"]), "2026/6")
	e.Problem(409, "assets.terms_in_use", "PATCH", path, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 72}]}`, e.l2), tag)
	e.Problem(409, "assets.terms_in_use", "PATCH", path, `{"depreciation_start": "2026-09-01"}`, tag)
	_, tag = e.Must(200, "PATCH", path, `{"description": "Still changes"}`, tag)
	_, tag = e.Must(200, "POST", path+":cancel", "", tag)
	e.Problem(422, "assets.asset_cancelled", "PATCH", path, `{"description": "After"}`, tag)
}

// TestUpdateAssetRefused: terms a ledger cannot take.
func TestUpdateAssetRefused(t *testing.T) {
	e := newAssets(t)
	a, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	path := "/v1/assets/" + a["id"].(string)
	entry := func(ledger uuid.UUID, fields string) string {
		return fmt.Sprintf(`{"terms": [{"ledger_id": %q%s}]}`, ledger, fields)
	}
	for name, c := range map[string]struct {
		body, code, field string
		status            int
	}{
		"a ledger twice":                     {fmt.Sprintf(`{"terms": [{"ledger_id": %q}, {"ledger_id": %q}]}`, e.l0, e.l0), "request.validation_failed", "terms[1].ledger_id", 422},
		"a ledger the company does not keep": {entry(e.l3, `, "method": "straight_line", "useful_life_months": 60`), "ledger.ledger_not_kept", "terms[0].ledger_id", 422},
		"an unknown method":                  {entry(e.l0, `, "method": "units_of_production"`), "request.validation_failed", "terms[0].method", 422},
		"a factor with straight-line":        {entry(e.l0, `, "declining_factor": "2"`), "request.validation_failed", "terms[0].declining_factor", 422},
		"declining balance without a factor": {entry(e.l0, `, "method": "declining_balance"`), "request.validation_failed", "terms[0].declining_factor", 422},
		"a factor of 1":                      {entry(e.l2, `, "declining_factor": "1"`), "request.validation_failed", "terms[0].declining_factor", 422},
		"a factor above 10":                  {entry(e.l2, `, "declining_factor": "10.5"`), "request.validation_failed", "terms[0].declining_factor", 422},
		"a residual value in USD": {entry(e.l0, `, "residual_value": {"amount": "1.00", "currency": "USD"}`), "request.validation_failed",
			"terms[0].residual_value.currency", 422},
		"a negative residual value": {entry(e.l0, `, "residual_value": {"amount": "-1.00", "currency": "EUR"}`), "request.validation_failed",
			"terms[0].residual_value.amount", 422},
		"a residual value with three decimals": {entry(e.l0, `, "residual_value": {"amount": "1.000", "currency": "EUR"}`), "request.invalid_amount",
			"terms[0].residual_value.amount", 400},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.Problem(c.status, c.code, "PATCH", path, c.body, tag)
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("errors: got %v, want one on %s", p["errors"], c.field)
			}
			want(t, "field", errs[0].(map[string]any)["field"], c.field)
		})
	}
}

// TestCancelAsset: a planned asset is cancelled; a cancelled or active
// one is not.
func TestCancelAsset(t *testing.T) {
	e := newAssets(t)
	a, tag := e.create(t, e.body(e.US, "MACHINES", ""))
	path := "/v1/assets/" + a["id"].(string)
	a, tag = e.Must(200, "POST", path+":cancel", "", tag)
	want(t, "status", a["status"], "cancelled")
	e.Problem(409, "assets.asset_not_planned", "POST", path+":cancel", "", tag)
	e.Problem(412, "request.version_mismatch", "POST", path+":cancel", "", `"1"`)
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_changed"})
	want(t, "the cancellation's event", fmt.Sprint(evs[len(evs)-1]["change"], " ", evs[len(evs)-1]["status"]), "cancelled cancelled")

	b, _ := e.create(t, e.body(e.US, "MACHINES", ""))
	pgtest.Exec(t, `UPDATE assets.asset SET status = 'active', capitalised_on = '2026-06-01', version = version + 1 WHERE id = $1`, b["id"])
	b, tag = e.Must(200, "GET", "/v1/assets/"+b["id"].(string), "", "")
	want(t, "capitalised on", b["capitalised_on"], "2026-06-01")
	e.Problem(409, "assets.asset_not_planned", "POST", "/v1/assets/"+b["id"].(string)+":cancel", "", tag)
	e.Problem(404, "request.not_found", "POST", "/v1/assets/"+uuid.NewString()+":cancel", "", tag)
}

// TestListAssets: the assets of the caller's companies, filtered and
// paged; the centers named only for a caller who may read them.
func TestListAssets(t *testing.T) {
	e := newAssets(t)
	first, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Sales)))
	e.create(t, e.body(e.US, "VEHICLES", ""))
	cancelled, tag := e.create(t, e.body(e.US, "MACHINES", ""))
	e.Must(200, "POST", "/v1/assets/"+cancelled["id"].(string)+":cancel", "", tag)
	list := func(c apitest.Client, query string) string {
		page, _ := c.Must(200, "GET", "/v1/assets?"+query, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			a := it.(map[string]any)
			out = append(out, fmt.Sprintf("%s/%s %s %s %v", a["company_code"], a["asset_number"], a["class_code"], a["status"], a["cost_center_code"]))
		}
		return strings.Join(out, ", ")
	}
	for name, c := range map[string]struct{ query, want string }{
		"all":           {"", "2000/1 MACHINES planned 1000, 1000/1 VEHICLES planned <nil>, 2000/2 MACHINES planned 2000, 1000/2 MACHINES cancelled <nil>"},
		"by creation":   {"order_by=created_at", "2000/1 MACHINES planned 1000, 2000/2 MACHINES planned 2000, 1000/1 VEHICLES planned <nil>, 1000/2 MACHINES cancelled <nil>"},
		"a company":     {"legal_entity_id=" + e.US.String(), "1000/1 VEHICLES planned <nil>, 1000/2 MACHINES cancelled <nil>"},
		"a class":       {"class_code=MACHINES&legal_entity_id=" + e.DE.String(), "2000/1 MACHINES planned 1000, 2000/2 MACHINES planned 2000"},
		"a status":      {"status=cancelled", "1000/2 MACHINES cancelled <nil>"},
		"a cost center": {"cost_center_id=" + e.c.Sales.String(), "2000/2 MACHINES planned 2000"},
		"a number":      {"asset_number=2&legal_entity_id=" + e.DE.String(), "2000/2 MACHINES planned 2000"},
	} {
		t.Run(name, func(t *testing.T) { want(t, "assets", list(e.Client, c.query), c.want) })
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/assets?order_by=description", "", "")

	var paged []string
	path := "/v1/assets?order_by=created_at&page_size=3"
	for range 3 {
		page, _ := e.Must(200, "GET", path, "", "")
		for _, it := range page["items"].([]any) {
			paged = append(paged, it.(map[string]any)["id"].(string))
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/assets?order_by=created_at&page_size=3&page_token=" + next
	}
	want(t, "assets one page after another", len(paged), 4)
	want(t, "the first", paged[0], first["id"])
	page, _ := e.Must(200, "GET", "/v1/assets?page_size=1", "", "")
	page, _ = e.Must(200, "GET", "/v1/assets?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	want(t, "the second by number", page["items"].([]any)[0].(map[string]any)["company_code"], "1000")

	reader := e.Client
	reader.As = e.Reader(t, "reader", e.US)
	want(t, "a reader of company 1000", list(reader, ""), "1000/1 VEHICLES planned <nil>, 1000/2 MACHINES cancelled <nil>")
	other := e.assetsOnly(t)
	got, _ := other.Must(200, "GET", "/v1/assets/"+first["id"].(string), "", "")
	want(t, "centers without ledger.cost_center.read", fmt.Sprint(got["cost_center_id"], " ", got["cost_center_code"], " ", got["profit_center_name"]),
		e.c.Admin.String()+" <nil> <nil>")
}

// assetsOnly is a client acting as a person who may read assets and
// nothing else: not cost centers, so centers are not named to them.
func (e assetEnv) assetsOnly(t *testing.T) apitest.Client {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), 'assets_only', 'a', 'active', $2, $2)`,
		e.Tenant.ID, e.Tenant.Human.ID)
	plain := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	plain.Subject = "human|" + plain.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'plain', true, 'active', $2, $2)`, e.Tenant.ID, plain.ID, pgtest.Issuer(t), plain.Subject)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'assets_only'`, e.Tenant.ID, plain.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT $1, gen_random_uuid(), id, 'assets.asset.read', current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'assets_only'`,
		e.Tenant.ID, plain.ID)
	other := e.Client
	other.As = plain
	return other
}

// TestAssetDatabaseFailures calls each asset handler with each of its database
// statements failing in turn, and the run port's adapter likewise.
func TestAssetDatabaseFailures(t *testing.T) {
	e := newAssets(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages})
	cc := e.c.Admin
	a, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, cc)))
	id := uuid.MustParse(a["id"].(string))
	e.Must(200, "PATCH", "/v1/assets/"+a["id"].(string), `{"depreciation_start": "2026-07-01"}`, tag)
	for name, fn := range map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateAsset(ctx, &gen.AssetCreate{LegalEntityID: e.DE, ClassCode: "MACHINES", Description: "d", CostCenterID: gen.NewOptUUID(cc)},
				gen.CreateAssetParams{})
			return err
		},
		"create without a cost center": func(ctx context.Context) error {
			_, err := h.CreateAsset(ctx, &gen.AssetCreate{LegalEntityID: e.US, ClassCode: "MACHINES", Description: "d"}, gen.CreateAssetParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetAsset(ctx, gen.GetAssetParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListAssets(ctx, gen.ListAssetsParams{})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateAsset(ctx, &gen.AssetUpdate{Description: gen.NewOptString("x"), Terms: []gen.AssetTermsUpdate{{LedgerID: e.l0,
				ResidualValue: gen.NewOptMoney(gen.Money{Amount: "1.00", Currency: "EUR"})}}}, gen.UpdateAssetParams{ID: id, IfMatch: `"2"`})
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := h.CancelAsset(ctx, gen.CancelAssetParams{ID: id, IfMatch: `"2"`})
			return err
		},
		"a run's operation": func(ctx context.Context) error {
			o, err := h.Runs.Start(ctx, "/v1/depreciation-runs/"+id.String(), map[string]any{"run_id": id})
			if err != nil {
				return err
			}
			if err := h.Runs.Succeed(ctx, o.ID, "/v1/depreciation-runs/"+id.String(), map[string]any{"assets_posted": 1}); err != nil {
				return err
			}
			o, err = h.Runs.Start(ctx, "/v1/depreciation-runs/"+id.String(), map[string]any{"run_id": id})
			if err != nil {
				return err
			}
			return h.Runs.Fail(ctx, o.ID, problem.Unprocessable("ledger.period_not_open", "The period is closed."))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
				t.Errorf("outside a unit of work: got %v, want it refused", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"the ledgers": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.Ledgers(ctx, nil)
			return err
		},
		"an assignment": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.Assignment(ctx, e.DE, assetDay, 2026, "620000", nil, nil)
			return err
		},
		"center names": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.CenterNames(ctx, nil)
			return err
		},
	} {
		if err := fn(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
			t.Errorf("%s outside a unit of work: got %v, want reqctx.ErrOutsideRequest", name, err)
		}
	}
}

// zoneLost is the active document with company 2000's zone one the
// binary cannot load.
type zoneLost struct{ assets.Settings }

func (z zoneLost) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := z.Settings.Document(ctx)
	if err == nil {
		doc.Company("2000").TimeZone = "Europe/Nowhere"
	}
	return doc, n, err
}

// TestCreateAssetZoneLost: a company whose stored zone does not load
// fails a creation rather than dating the asset in UTC.
func TestCreateAssetZoneLost(t *testing.T) {
	e := newAssets(t)
	h := app.Assets(app.Config{})
	h.Settings = zoneLost{h.Settings}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.CreateAsset(ctx, &gen.AssetCreate{LegalEntityID: e.DE, ClassCode: "MACHINES", Description: "d", CostCenterID: gen.NewOptUUID(e.c.Admin)},
			gen.CreateAssetParams{})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "Europe/Nowhere") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
}

// TestDepreciationRunOperations: the run port writes a queued operation of
// kind assets.depreciate for the run, and finishes it.
func TestDepreciationRunOperations(t *testing.T) {
	e := newAssets(t)
	e.Tenant.Grant(t, "platform.operation.read", e.Tenant.Human)
	h := app.Assets(app.Config{})
	run := uuid.NewString()
	var done, failed uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		o, err := h.Runs.Start(ctx, "/v1/depreciation-runs/"+run, map[string]any{"run_id": run})
		if err != nil {
			return err
		}
		want(t, "the operation", fmt.Sprint(o.Kind, " ", o.Status), assets.KindDepreciate+" queued")
		done = o.ID
		if err := h.Runs.Succeed(ctx, o.ID, "/v1/depreciation-runs/"+run, map[string]any{"assets_posted": 1}); err != nil {
			return err
		}
		if o, err = h.Runs.Start(ctx, "/v1/depreciation-runs/"+run, nil); err != nil {
			return err
		}
		failed = o.ID
		return h.Runs.Fail(ctx, o.ID, problem.Unprocessable("ledger.period_not_open", "The period is closed."))
	}); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[uuid.UUID]string{done: "succeeded", failed: "failed"} {
		o, _ := e.Must(200, "GET", "/v1/operations/"+id.String(), "", "")
		want(t, "the operation's status", o["status"], status)
	}
}

// ledgersReversed is the ledger port listing the tenant's ledgers in the
// reverse of their order.
type ledgersReversed struct{ assets.Ledger }

func (l ledgersReversed) Ledgers(ctx context.Context, le *uuid.UUID) ([]assets.LedgerRef, error) {
	out, err := l.Ledger.Ledgers(ctx, le)
	slices.Reverse(out)
	return out, err
}

// recordedLedgers calls fn as the tenant's person and returns the ledger
// ids at path in the audit record of the action it writes, in their order
// there: its buffered records are written in its transaction and read
// back before the transaction is rolled back.
func recordedLedgers(t *testing.T, e assetEnv, action, path string, fn func(ctx context.Context) error) string {
	t.Helper()
	var got string
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		ctx, buf := audit.WithBuffer(ctx)
		if err := fn(ctx); err != nil {
			return err
		}
		tx, p, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		if err := buf.Flush(ctx, tx, p, "", assetDay); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT jsonb_path_query_array(detail, $2::jsonpath)::text FROM audit.audit_event WHERE action = $1
			ORDER BY seq DESC LIMIT 1`, action, path).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// ledgerList is the ledger ids as recordedLedgers shows them.
func ledgerList(ids ...uuid.UUID) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// TestAssetRecordedInLedgerOrder: a change's audit record lists the
// asset's terms in the ledgers' order, not in the order the database
// returns their rows in (batch 50's ci-local saw 2L's first after a PATCH
// moved 0L's row). Two changes of the description leave the terms' rows
// as they are, so the database returns them alike both times, and each
// records them in its ledger port's order: as listed, then reversed.
func TestAssetRecordedInLedgerOrder(t *testing.T) {
	e := newAssets(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	id := uuid.MustParse(a["id"].(string))
	for _, c := range []struct {
		name     string
		reversed bool
		want     string
	}{
		{"in the ledgers' order", false, ledgerList(e.l0, e.l2)},
		{"in the port's order, reversed", true, ledgerList(e.l2, e.l0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := app.Assets(app.Config{})
			if c.reversed {
				h.Ledger = ledgersReversed{h.Ledger}
			}
			got := recordedLedgers(t, e, "assets.asset.change", "$.terms[*].ledger_id", func(ctx context.Context) error {
				_, err := h.UpdateAsset(ctx, &gen.AssetUpdate{Description: gen.NewOptString(c.name)}, gen.UpdateAssetParams{ID: id, IfMatch: "*"})
				return err
			})
			if got != c.want {
				t.Errorf("the change's terms: got %s, want %s", got, c.want)
			}
		})
	}
}
