package integration_test

import (
	"context"
	"testing"

	"github.com/go-faster/jx"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
)

// einvoiceSettings are valid einvoice_provider settings, the stand-in's.
const einvoiceSettings = `{"adapter": "standin", "base_url": "https://127.0.0.1:18444/provider", "participants": {"AE01": "0235:100000000000003"}, "format": "pint_ae"}`

// TestEInvoiceSettings: an einvoice_provider connector names its adapter,
// the provider's https base URL, a participant identifier per company and
// the format; anything else is refused naming the setting (ADR-0061 §3).
func TestEInvoiceSettings(t *testing.T) {
	f := fixture.New(t)
	h := &integration.Endpoints{}
	for name, c := range map[string]struct {
		settings, field string // field: "" when valid
	}{
		"the stand-in":            {einvoiceSettings, ""},
		"another setting":         {`{"adapter": "standin", "base_url": "https://p.example", "participants": {"AE01": "P1"}, "format": "pint_ae", "token": "x"}`, "settings.token"},
		"another adapter":         {`{"adapter": "acme", "base_url": "https://p.example", "participants": {"AE01": "P1"}, "format": "pint_ae"}`, "settings.adapter"},
		"no adapter":              {`{"base_url": "https://p.example", "participants": {"AE01": "P1"}, "format": "pint_ae"}`, "settings.adapter"},
		"plain http":              {`{"adapter": "standin", "base_url": "http://p.example", "participants": {"AE01": "P1"}, "format": "pint_ae"}`, "settings.base_url"},
		"credentials in the URL":  {`{"adapter": "standin", "base_url": "https://u:p@p.example", "participants": {"AE01": "P1"}, "format": "pint_ae"}`, "settings.base_url"},
		"a query":                 {`{"adapter": "standin", "base_url": "https://p.example/?k=1", "participants": {"AE01": "P1"}, "format": "pint_ae"}`, "settings.base_url"},
		"another format":          {`{"adapter": "standin", "base_url": "https://p.example", "participants": {"AE01": "P1"}, "format": "xrechnung"}`, "settings.format"},
		"no participants":         {`{"adapter": "standin", "base_url": "https://p.example", "participants": {}, "format": "pint_ae"}`, "settings.participants"},
		"participants not a map":  {`{"adapter": "standin", "base_url": "https://p.example", "participants": ["P1"], "format": "pint_ae"}`, "settings.participants"},
		"a participant twice":     {`{"adapter": "standin", "base_url": "https://p.example", "participants": {"AE01": "P1", "AE02": "P1"}, "format": "pint_ae"}`, "settings.participants"},
		"a malformed participant": {`{"adapter": "standin", "base_url": "https://p.example", "participants": {"AE01": "P 1"}, "format": "pint_ae"}`, "settings.participants"},
		"a malformed company":     {`{"adapter": "standin", "base_url": "https://p.example", "participants": {"ae01": "P1"}, "format": "pint_ae"}`, "settings.participants"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.EInvoiceProvider, Name: name, Settings: decoded(t, c.settings),
					SecretRef: gen.NewOptString("env:PROVIDER_TOKEN")}, gen.CreateConnectorParams{})
				return err
			})
			if c.field == "" {
				if err != nil {
					t.Errorf("got %v, want created", err)
				}
				return
			}
			p := problem.From(err)
			if p.Code != "integration.invalid_settings" || len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("got %v, want integration.invalid_settings on %s", err, c.field)
			}
		})
	}
}

// decoded is settings as the API decodes them.
func decoded(t *testing.T, settings string) gen.ConnectorCreateSettings {
	t.Helper()
	out := gen.ConnectorCreateSettings{}
	if err := jx.DecodeStr(settings).ObjBytes(func(d *jx.Decoder, key []byte) error {
		raw, err := d.Raw()
		out[string(key)] = jx.Raw(raw.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStandInRefusedInProduction: where uncertified connectors are refused
// (production), an einvoice_provider of the stand-in's adapter can be
// neither created nor enabled, though it can be disabled; elsewhere it can
// (ADR-0061 §3).
func TestStandInRefusedInProduction(t *testing.T) {
	f := fixture.New(t)
	development, production := &integration.Endpoints{}, &integration.Endpoints{RefuseEBICS: true}
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		create := func(h *integration.Endpoints, name string) (gen.CreateConnectorRes, error) {
			return h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.EInvoiceProvider, Name: name, Settings: decoded(t, einvoiceSettings),
				SecretRef: gen.NewOptString("env:PROVIDER_TOKEN")}, gen.CreateConnectorParams{})
		}
		if _, err := create(production, "Stand-in refused"); problem.From(err).Code != "integration.adapter_not_certified" {
			t.Errorf("create in production: got %v, want integration.adapter_not_certified", err)
		}
		res, err := create(development, "Stand-in")
		if err != nil {
			t.Fatalf("create in development: got %v, want created", err)
		}
		id := res.(*gen.ConnectorHeaders).Response.ID
		if _, err := production.DisableConnector(ctx, &gen.ReasonInput{Reason: "not certified"}, gen.DisableConnectorParams{ID: id, IfMatch: "*"}); err != nil {
			t.Fatalf("disable in production: got %v, want disabled", err)
		}
		if _, err := production.EnableConnector(ctx, gen.EnableConnectorParams{ID: id, IfMatch: "*"}); problem.From(err).Code != "integration.adapter_not_certified" {
			t.Errorf("enable in production: got %v, want integration.adapter_not_certified", err)
		}
		if _, err := development.EnableConnector(ctx, gen.EnableConnectorParams{ID: id, IfMatch: "*"}); err != nil {
			t.Errorf("enable in development: got %v, want enabled", err)
		}
		if _, err := production.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.SAPFile, Name: "SAP", Settings: decoded(t, `{"path": "x.json"}`)},
			gen.CreateConnectorParams{}); err != nil {
			t.Errorf("another kind in production: got %v, want created", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestEInvoiceProviders: an einvoice_provider connector resolves into its
// settings with its credentials, read now, and fails closed on anything
// else; the stand-in is refused at use where production refuses it
// (ADR-0061 §2, §3). Names are given by id.
func TestEInvoiceProviders(t *testing.T) {
	f := fixture.New(t)
	vars := map[string]string{"PROVIDER_TOKEN": "s3cret"}
	p := integration.EInvoiceProviders{Secrets: secrets.Local{Getenv: func(n string) string { return vars[n] }}}
	conn := func(kind, name, settings, ref, status string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		var secret *string
		if ref != "" {
			secret = &ref
		}
		pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, f.Tenant.ID, id, kind, name, settings, secret, status, f.Tenant.Human.ID)
		return id
	}
	valid := conn(integration.EInvoiceProvider, "Provider", einvoiceSettings, "env:PROVIDER_TOKEN", "active")
	resolve := func(p integration.EInvoiceProviders, id uuid.UUID) (c integration.EInvoiceConnector, err error) {
		err = apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
			c, err = p.Provider(ctx, id)
			return err
		})
		return c, err
	}
	c, err := resolve(p, valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != valid || c.Name != "Provider" || c.Adapter != "standin" || c.BaseURL != "https://127.0.0.1:18444/provider" || c.Format != "pint_ae" ||
		c.Participants["AE01"] != "0235:100000000000003" || c.Secret != "s3cret" {
		t.Errorf("got %+v, want the connector's settings and secret", c)
	}
	for name, c := range map[string]struct {
		p    integration.EInvoiceProviders
		id   uuid.UUID
		code string
	}{
		"unknown":                {p, uuid.New(), "request.not_found"},
		"not a provider":         {p, conn("sap_file", "SAP", `{"path": "x.json"}`, "", "active"), "integration.not_an_einvoice_provider"},
		"disabled":               {p, conn(integration.EInvoiceProvider, "Off", einvoiceSettings, "env:PROVIDER_TOKEN", "disabled"), "integration.connector_disabled"},
		"no secret_ref":          {p, conn(integration.EInvoiceProvider, "No token", einvoiceSettings, "", "active"), "integration.secret_required"},
		"invalid settings":       {p, conn(integration.EInvoiceProvider, "Old", `{"adapter": "standin"}`, "env:PROVIDER_TOKEN", "active"), "integration.invalid_settings"},
		"secret missing":         {p, conn(integration.EInvoiceProvider, "Missing", einvoiceSettings, "env:MISSING", "active"), "integration.secret_unavailable"},
		"stand-in in production": {integration.EInvoiceProviders{Secrets: p.Secrets, RefuseStandIn: true}, valid, "integration.adapter_not_certified"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolve(c.p, c.id); problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}

	var names map[uuid.UUID]string
	if err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
		names, err = p.Names(ctx, []uuid.UUID{valid, uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[valid] != "Provider" {
		t.Errorf("names: got %v, want the provider's alone", names)
	}

	t.Run("outside a unit of work and failing statements", func(t *testing.T) {
		for name, call := range map[string]func(ctx context.Context) error{
			"provider": func(ctx context.Context) error { _, err := p.Provider(ctx, valid); return err },
			"names":    func(ctx context.Context) error { _, err := p.Names(ctx, []uuid.UUID{valid}); return err },
		} {
			t.Run(name, func(t *testing.T) {
				if err := call(context.Background()); err == nil {
					t.Error("outside a unit of work: got nil error, want an error")
				}
				apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, name, call)
			})
		}
	})
}
