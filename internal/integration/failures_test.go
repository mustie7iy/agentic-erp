package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func endpoints(dir string) *integration.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return httpapi.Integration(httpapi.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), ExtractDir: dir})
}

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(e.dir)
	blocked := tiny()
	blocked.LFA1[0].SPERR = "X"
	write(t, e.dir, "tiny.json", blocked)
	usOnly := tiny()
	usOnly.T001 = []sap.T001{{BUKRS: "1000", WAERS: "USD"}}
	usOnly.SKB1 = []sap.SKB1{{BUKRS: "1000", SAKNR: "0000211000", MITKZ: "K"}}
	usOnly.LFB1[0].BUKRS = "1000"
	usOnly.EKKO, usOnly.EKPO, usOnly.MSEG, usOnly.RBKP, usOnly.RSEG, usOnly.ACDOCA = nil, nil, nil, nil, nil, nil
	write(t, e.dir, "us.json", usOnly)
	bad := tiny()
	bad.T001[0].WAERS = "USD"
	write(t, e.dir, "bad.json", bad)

	c := e.connector(t, "tiny.json")
	conn := uuid.MustParse(c)
	loaded := e.run(t, c, "documents", false, e.DE.String())
	e.act(t, loaded, "start")
	us := uuid.MustParse(e.run(t, e.connector(t, "us.json"), "documents", false, e.US.String())["id"].(string))
	fresh := uuid.MustParse(e.run(t, c, "documents", true, e.DE.String())["id"].(string))
	failing := uuid.MustParse(e.run(t, e.connector(t, "bad.json"), "documents", false, e.DE.String())["id"].(string))
	loadedID := uuid.MustParse(loaded["id"].(string))
	// A queued run of a fresh tenant loads everything again.
	other := setup(t)
	write(t, other.dir, "tiny.json", blocked)
	oc := other.connector(t, "tiny.json")
	first := uuid.MustParse(other.run(t, oc, "documents", false, other.DE.String())["id"].(string))
	balances := uuid.MustParse(other.run(t, oc, "balances", false, other.DE.String())["id"].(string))
	refusing := tiny()
	refusing.LFA1 = []sap.LFA1{{LIFNR: "0000000002", NAME1: "Other GmbH", LAND1: "ZZ"}}
	refusing.LFB1 = []sap.LFB1{{LIFNR: "0000000002", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"}}
	refusing.LFBK, refusing.EKKO, refusing.EKPO, refusing.MSEG, refusing.RBKP, refusing.RSEG, refusing.ACDOCA = nil, nil, nil, nil, nil, nil, nil
	write(t, e.dir, "refusing.json", refusing)
	refused := uuid.MustParse(e.run(t, e.connector(t, "refusing.json"), "documents", false, e.DE.String())["id"].(string))
	ho := endpoints(other.dir)
	yes := func(int32) error { return nil }

	calls := map[string]func(ctx context.Context) error{
		"create connector": func(ctx context.Context) error {
			_, err := h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: "email", Name: "Mail", Settings: gen.ConnectorCreateSettings{},
				SecretRef: gen.NewOptString("env:X")}, gen.CreateConnectorParams{})
			return err
		},
		"get connector": func(ctx context.Context) error {
			_, err := h.GetConnector(ctx, gen.GetConnectorParams{ID: conn})
			return err
		},
		"list connectors": func(ctx context.Context) error {
			_, err := h.ListConnectors(ctx, gen.ListConnectorsParams{})
			return err
		},
		"update connector": func(ctx context.Context) error {
			_, err := h.UpdateConnector(ctx, &gen.ConnectorUpdate{Name: gen.NewOptString("New")}, gen.UpdateConnectorParams{ID: conn, IfMatch: "*"})
			return err
		},
		"disable and enable": func(ctx context.Context) error {
			if _, err := h.DisableConnector(ctx, &gen.ReasonInput{Reason: "r"}, gen.DisableConnectorParams{ID: conn, IfMatch: "*"}); err != nil {
				return err
			}
			_, err := h.EnableConnector(ctx, gen.EnableConnectorParams{ID: conn, IfMatch: "*"})
			return err
		},
		"create run": func(ctx context.Context) error {
			_, err := h.CreateMigrationRun(ctx, &gen.MigrationRunCreate{ConnectorID: conn, Scope: gen.MigrationScope{LegalEntityIds: []uuid.UUID{e.DE},
				FiscalYear: 2026}, Mode: "documents"}, gen.CreateMigrationRunParams{})
			return err
		},
		"get run": func(ctx context.Context) error {
			_, err := h.GetMigrationRun(ctx, gen.GetMigrationRunParams{ID: loadedID})
			return err
		},
		"list runs": func(ctx context.Context) error {
			_, err := h.ListMigrationRuns(ctx, gen.ListMigrationRunsParams{})
			return err
		},
		"rehearse": func(ctx context.Context) error {
			_, err := h.StartMigrationRun(ctx, gen.StartMigrationRunParams{ID: fresh, IfMatch: "*"})
			return err
		},
		"extend suppliers": func(ctx context.Context) error {
			_, err := h.StartRun(ctx, us, yes)
			return err
		},
		"quality gate": func(ctx context.Context) error {
			_, err := h.StartRun(ctx, failing, yes)
			return err
		},
		"refused load": func(ctx context.Context) error {
			_, err := h.StartRun(ctx, refused, yes)
			return err
		},
		"reconcile": func(ctx context.Context) error {
			_, err := h.ReconcileMigrationRun(ctx, gen.ReconcileMigrationRunParams{ID: loadedID, IfMatch: "*"})
			return err
		},
		"order values": func(ctx context.Context) error {
			_, _, err := procurement.Orders{}.Value(ctx, nil)
			return err
		},
		"invoice counts": func(ctx context.Context) error {
			_, err := payables.Invoices{}.Count(ctx, nil)
			return err
		},
	}
	for name, call := range calls {
		if err := call(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
	}
	apitest.EveryFailure(t, other.Tenant, other.Tenant.Human, "load", func(ctx context.Context) error {
		_, err := ho.StartRun(ctx, first, yes)
		return err
	})
	apitest.EveryFailure(t, other.Tenant, other.Tenant.Human, "opening entries", func(ctx context.Context) error {
		_, err := ho.StartRun(ctx, balances, yes)
		return err
	})

	for name, call := range map[string]func(ctx context.Context) error{
		"list connectors order": func(ctx context.Context) error {
			_, err := h.ListConnectors(ctx, gen.ListConnectorsParams{OrderBy: gen.NewOptString("kind")})
			return err
		},
		"list runs order": func(ctx context.Context) error {
			_, err := h.ListMigrationRuns(ctx, gen.ListMigrationRunsParams{OrderBy: gen.NewOptString("status")})
			return err
		},
		"update unknown": func(ctx context.Context) error {
			_, err := integration.UpdateConnector(ctx, uuid.New(), yes, integration.ConnectorChange{})
			return err
		},
		"update mismatch": func(ctx context.Context) error {
			_, err := integration.UpdateConnector(ctx, conn, func(int32) error { return errMismatch }, integration.ConnectorChange{})
			return err
		},
		"status unknown": func(ctx context.Context) error {
			_, err := integration.SetConnectorStatus(ctx, uuid.New(), yes, "active", "")
			return err
		},
		"status mismatch": func(ctx context.Context) error {
			_, err := integration.SetConnectorStatus(ctx, conn, func(int32) error { return errMismatch }, "active", "")
			return err
		},
		"start unknown": func(ctx context.Context) error {
			_, err := h.StartRun(ctx, uuid.New(), yes)
			return err
		},
		"start mismatch": func(ctx context.Context) error {
			_, err := h.StartRun(ctx, fresh, func(int32) error { return errMismatch })
			return err
		},
		"reconcile unknown": func(ctx context.Context) error {
			_, err := h.ReconcileRun(ctx, uuid.New(), yes)
			return err
		},
		"reconcile mismatch": func(ctx context.Context) error {
			_, err := h.ReconcileRun(ctx, loadedID, func(int32) error { return errMismatch })
			return err
		},
	} {
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
