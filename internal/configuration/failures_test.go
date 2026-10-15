package configuration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func endpoints() *configuration.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &configuration.Endpoints{Pages: pages, Dependencies: deps()}
}

func TestDatabaseFailures(t *testing.T) {
	e := setup(t, false)
	h := endpoints()
	added := document(t)
	added.NumberRanges = append(added.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "payment", FiscalYear: 2026, First: 1, Last: 9})
	draft := e.propose(t, 1, added)
	approved := e.propose(t, 1, added)
	e.transition(t, approved, "submit")
	tax := document(t)
	tax.Companies[0].TaxCodes[0].RatePercent = "9"
	evidence := e.propose(t, 1, tax)
	e.transition(t, evidence, "submit")
	versions, _ := e.Must(200, "GET", "/v1/config-versions", "", "")
	version := uuid.MustParse(versions["items"].([]any)[0].(map[string]any)["id"].(string))
	ranges, _ := e.Must(200, "GET", "/v1/number-ranges", "", "")
	rangeID := uuid.MustParse(ranges["items"].([]any)[0].(map[string]any)["id"].(string))
	id := func(m map[string]any) uuid.UUID { return uuid.MustParse(m["id"].(string)) }
	proposed := gen.ChangeRequestCreateProposed{}
	if err := json.Unmarshal([]byte(jsonOf(t, added)), &proposed); err != nil {
		t.Fatal(err)
	}

	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateChangeRequest(ctx, &gen.ChangeRequestCreate{Title: "T", BaseVersion: 1, Proposed: proposed}, gen.CreateChangeRequestParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id(draft)})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListChangeRequests(ctx, gen.ListChangeRequestsParams{})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateChangeRequest(ctx, &gen.ChangeRequestUpdate{Title: gen.NewOptString("U"), Rationale: gen.NewOptString("R")},
				gen.UpdateChangeRequestParams{ID: id(draft), IfMatch: "*"})
			return err
		},
		"submit": func(ctx context.Context) error {
			_, err := h.SubmitChangeRequest(ctx, gen.SubmitChangeRequestParams{ID: id(draft), IfMatch: "*"})
			return err
		},
		"withdraw": func(ctx context.Context) error {
			_, err := h.WithdrawChangeRequest(ctx, &gen.ReasonInput{Reason: "r"}, gen.WithdrawChangeRequestParams{ID: id(draft), IfMatch: "*"})
			return err
		},
		"activate": func(ctx context.Context) error {
			_, err := h.ActivateChangeRequest(ctx, gen.ActivateChangeRequestParams{ID: id(approved), IfMatch: "*"})
			return err
		},
		"simulate": func(ctx context.Context) error {
			_, err := h.SimulateChangeRequest(ctx, gen.SimulateChangeRequestParams{ID: id(evidence), IfMatch: "*"})
			return err
		},
		"get version": func(ctx context.Context) error {
			_, err := h.GetConfigVersion(ctx, gen.GetConfigVersionParams{ID: version})
			return err
		},
		"list versions": func(ctx context.Context) error {
			_, err := h.ListConfigVersions(ctx, gen.ListConfigVersionsParams{})
			return err
		},
		"get range": func(ctx context.Context) error {
			_, err := h.GetNumberRange(ctx, gen.GetNumberRangeParams{ID: rangeID})
			return err
		},
		"list ranges": func(ctx context.Context) error {
			_, err := h.ListNumberRanges(ctx, gen.ListNumberRangesParams{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}

	bare := setup(t, true)
	bootstrap := func(ctx context.Context) error {
		_, err := configuration.Bootstrap(ctx, deps(), json.RawMessage(jsonOf(t, document(t))))
		return err
	}
	if err := bootstrap(context.Background()); err == nil {
		t.Error("bootstrap outside a unit of work")
	}
	apitest.EveryFailure(t, bare.ten, bare.ten.Human, "bootstrap", bootstrap)
	for i, fn := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := configuration.CreateChangeRequest(ctx, configuration.NewChangeRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := configuration.UpdateChangeRequest(ctx, uuid.Nil, nil, configuration.ChangeRequestChange{})
			return err
		},
		func(ctx context.Context) error {
			_, err := configuration.SubmitChangeRequest(ctx, configuration.Dependencies{}, uuid.Nil, nil)
			return err
		},
		func(ctx context.Context) error {
			_, err := configuration.WithdrawChangeRequest(ctx, configuration.Dependencies{}, uuid.Nil, nil, "")
			return err
		},
		func(ctx context.Context) error {
			_, err := configuration.ActivateChangeRequest(ctx, configuration.Dependencies{}, uuid.Nil, nil)
			return err
		},
	} {
		t.Run(fmt.Sprintf("context function %d", i), func(t *testing.T) {
			if fn(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}

	// Page sizes are checked by the handlers too, for callers other than the generated router.
	zero := gen.NewOptInt32(0)
	for name, call := range map[string]func(ctx context.Context) error{
		"change requests": func(ctx context.Context) error {
			_, err := h.ListChangeRequests(ctx, gen.ListChangeRequestsParams{PageSize: zero})
			return err
		},
		"versions": func(ctx context.Context) error {
			_, err := h.ListConfigVersions(ctx, gen.ListConfigVersionsParams{PageSize: zero})
			return err
		},
		"ranges": func(ctx context.Context) error {
			_, err := h.ListNumberRanges(ctx, gen.ListNumberRangesParams{PageSize: zero})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.ten, e.ten.Human, call); err == nil {
				t.Error("page_size 0: got no error, want it refused")
			}
		})
	}
}

// TestActivationWithoutLegalEntity covers a legal entity that disappeared
// between submission and activation (they are never deleted through the
// API): activation fails instead of creating a range for nobody.
func TestActivationWithoutLegalEntity(t *testing.T) {
	e := setup(t, false)
	cr := e.propose(t, 1, func() *config.Config { d := document(t); d.NumberRanges[0].Last = 123456; return d }())
	e.transition(t, cr, "submit")
	pgtest.Exec(t, `DELETE FROM config.number_range WHERE tenant_id = $1`, e.ten.ID)
	pgtest.Exec(t, `DELETE FROM tenancy.legal_entity WHERE tenant_id = $1`, e.ten.ID)
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := endpoints().ActivateChangeRequest(ctx, gen.ActivateChangeRequestParams{ID: uuid.MustParse(cr["id"].(string)), IfMatch: "*"})
		return err
	})
	pgtest.WantErr(t, err, "no legal entity 1000")
}

// TestStateFromLaterSteps shows fields that later steps fill (workflow
// requests in T4, evidence from the simulation worker, rollback versions).
func TestStateFromLaterSteps(t *testing.T) {
	e := setup(t, false)
	tax := document(t)
	tax.Companies[0].TaxCodes[0].RatePercent = "9"
	cr := e.propose(t, 1, tax)
	e.transition(t, cr, "submit")
	crID, wf := uuid.MustParse(cr["id"].(string)), uuid.New()
	pgtest.Exec(t, `UPDATE config.change_request SET workflow_request_id = $2, version = version + 1 WHERE id = $1`, crID, wf)
	pgtest.Exec(t, `INSERT INTO config.evidence (tenant_id, change_request_id, kind, passed, summary, detail, recorded_by)
		VALUES ($1, $2, 'simulation', true, '120 invoices replayed', '{"replayed": 120}', $3)`, e.ten.ID, crID, e.ten.Human.ID)
	got, _ := e.Must(200, "GET", "/v1/change-requests/"+crID.String(), "", "")
	ev := got["evidence"].([]any)
	if got["workflow_request_id"] != wf.String() {
		t.Errorf("workflow_request_id: got %v, want %s", got["workflow_request_id"], wf)
	}
	if len(ev) != 1 {
		t.Fatalf("evidence: got %d %v, want 1", len(ev), ev)
	}
	if replayed := ev[0].(map[string]any)["detail"].(map[string]any)["replayed"]; replayed != float64(120) {
		t.Errorf("evidence replayed: got %v, want 120", replayed)
	}
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT tenant_id, gen_random_uuid(), 2, document, hash, 'rollback', 1, activated_by FROM config.config_version WHERE tenant_id = $1`, e.ten.ID)
	list, _ := e.Must(200, "GET", "/v1/config-versions?number=2", "", "")
	v := list["items"].([]any)[0].(map[string]any)
	if v["restored_from"] != float64(1) {
		t.Errorf("rollback version restored_from: got %v, want 1", v["restored_from"])
	}
	if v["is_active"] != false {
		t.Errorf("rollback version is_active: got %v, want false", v["is_active"])
	}
	// Simulation reports a missing audit buffer (a call outside the unit of work's setup).
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: e.ten.Human.ID, Kind: "human", TenantID: e.ten.ID})
	if _, err := endpoints().SimulateChangeRequest(ctx, gen.SimulateChangeRequestParams{ID: crID, IfMatch: "*"}); err == nil {
		t.Fatal("simulation without an audit buffer")
	}
}

// deps is what the API wires: the tenancy directory and the sections other
// contexts own.
func deps() configuration.Dependencies {
	return configuration.Dependencies{LegalEntities: tenancy.Directory{},
		Sections: []configuration.Section{ledger.Setup{LegalEntities: tenancy.Directory{}}, partner.PaymentTerms{}}}
}
