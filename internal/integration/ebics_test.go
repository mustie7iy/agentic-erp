package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/go-faster/jx"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ebicsSettings are valid bank_ebics settings as the API decodes them.
func ebicsSettings() gen.ConnectorCreateSettings {
	return gen.ConnectorCreateSettings{"url": jx.Raw(`"https://ebics.bank.example/ebicsweb"`), "host_id": jx.Raw(`"BANKHOST"`),
		"partner_id": jx.Raw(`"PARTNER1"`), "user_id": jx.Raw(`"USER1"`)}
}

// TestEBICSRefusedInProduction: where EBICS is refused (production), a
// bank_ebics connector can be neither created nor enabled, though it can be
// disabled; elsewhere it can (ADR-0038, T44d).
func TestEBICSRefusedInProduction(t *testing.T) {
	f := fixture.New(t)
	development, production := &integration.Endpoints{}, &integration.Endpoints{RefuseEBICS: true}
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		create := func(h *integration.Endpoints, name string) (gen.CreateConnectorRes, error) {
			return h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.BankEBICS, Name: name, Settings: ebicsSettings(),
				SecretRef: gen.NewOptString("env:EBICS_KEYS")}, gen.CreateConnectorParams{})
		}
		if _, err := create(production, "EBICS refused"); problem.From(err).Code != "integration.ebics_not_certified" {
			t.Errorf("create in production: got %v, want integration.ebics_not_certified", err)
		}
		res, err := create(development, "EBICS")
		if err != nil {
			t.Fatalf("create in development: got %v, want created", err)
		}
		id := res.(*gen.ConnectorHeaders).Response.ID
		if _, err := production.DisableConnector(ctx, &gen.ReasonInput{Reason: "not certified"}, gen.DisableConnectorParams{ID: id, IfMatch: "*"}); err != nil {
			t.Fatalf("disable in production: got %v, want disabled", err)
		}
		if _, err := production.EnableConnector(ctx, gen.EnableConnectorParams{ID: id, IfMatch: "*"}); problem.From(err).Code != "integration.ebics_not_certified" {
			t.Errorf("enable in production: got %v, want integration.ebics_not_certified", err)
		}
		if _, err := development.EnableConnector(ctx, gen.EnableConnectorParams{ID: id, IfMatch: "*"}); err != nil {
			t.Errorf("enable in development: got %v, want enabled", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRefuseActiveEBICSFailure: the import's check reports a failing read.
func TestRefuseActiveEBICSFailure(t *testing.T) {
	f := fixture.New(t)
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		return integration.RefuseActiveEBICS(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}, f.Tenant.ID)
	})
	if !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("got %v, want the injected failure", err)
	}
}
