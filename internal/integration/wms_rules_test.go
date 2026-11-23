package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

const wmsBase = `"adapter": "standin", "base_url": "https://wms.example"`

// TestWMSSettings: a wms connector names the stand-in's adapter, its
// https base URL and the plants it manages by company code; anything else
// is refused naming the setting; production refuses the stand-in on
// create and enable (ADR-0082 As built S2q).
func TestWMSSettings(t *testing.T) {
	f := fixture.New(t)
	h := &integration.Endpoints{}
	for name, c := range map[string]struct {
		settings, field string // field: "" when valid
	}{
		"the stand-in":            {`{` + wmsBase + `, "plants": {"1000": ["P1", "P2"], "2000": ["P9"]}}`, ""},
		"another setting":         {`{` + wmsBase + `, "plants": {"1000": ["P1"]}, "token": "x"}`, "settings.token"},
		"another adapter":         {`{"adapter": "manhattan", "base_url": "https://wms.example", "plants": {"1000": ["P1"]}}`, "settings.adapter"},
		"plain http":              {`{"adapter": "standin", "base_url": "http://wms.example", "plants": {"1000": ["P1"]}}`, "settings.base_url"},
		"no plants":               {`{` + wmsBase + `, "plants": {}}`, "settings.plants"},
		"plants not a map":        {`{` + wmsBase + `, "plants": ["P1"]}`, "settings.plants"},
		"a company without any":   {`{` + wmsBase + `, "plants": {"1000": []}}`, "settings.plants"},
		"a company not a list":    {`{` + wmsBase + `, "plants": {"1000": "P1"}}`, "settings.plants"},
		"a plant twice":           {`{` + wmsBase + `, "plants": {"1000": ["P1", "P1"]}}`, "settings.plants"},
		"a malformed plant":       {`{` + wmsBase + `, "plants": {"1000": ["p 1"]}}`, "settings.plants"},
		"a plant not a string":    {`{` + wmsBase + `, "plants": {"1000": [1]}}`, "settings.plants"},
		"a malformed company":     {`{` + wmsBase + `, "plants": {"ab": ["P1"]}}`, "settings.plants"},
		"without settings at all": {`{}`, "settings.adapter"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.WMS, Name: name, Settings: decoded(t, c.settings),
					SecretRef: gen.NewOptString(vaulttest.TenantRef(f.Tenant.ID, "wms"))}, gen.CreateConnectorParams{})
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
	production := &integration.Endpoints{RefuseEBICS: true}
	t.Run("production refuses the stand-in", func(t *testing.T) {
		err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			_, err := production.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.WMS, Name: "In production",
				Settings: decoded(t, `{`+wmsBase+`, "plants": {"1000": ["P5"]}}`)}, gen.CreateConnectorParams{})
			return err
		})
		if p := problem.From(err); p.Code != "integration.adapter_not_certified" {
			t.Errorf("create: got %v, want integration.adapter_not_certified", err)
		}
		var id uuid.UUID
		if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			c, err := integration.CreateConnector(ctx, integration.NewConnector{Kind: integration.WMS, Name: "Disabled", Settings: map[string]any{
				"adapter": "standin", "base_url": "https://wms.example", "plants": map[string]any{"1000": []any{"P6"}}}})
			id = c.ID
			if err != nil {
				return err
			}
			_, err = integration.SetConnectorStatus(ctx, c.ID, func(int32) error { return nil }, "disabled", "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		err = apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			_, err := production.EnableConnector(ctx, gen.EnableConnectorParams{ID: id, IfMatch: etag.Format(2)})
			return err
		})
		if p := problem.From(err); p.Code != "integration.adapter_not_certified" {
			t.Errorf("enable: got %v, want integration.adapter_not_certified", err)
		}
	})
}

// TestWMSPlantOnce: a plant is managed by one active wms connector at
// most: creating, changing or enabling one naming a plant another active
// one manages is refused (409 integration.plant_has_wms); a disabled one
// may name it, a connector keeps its own plants when changed, and another
// kind's connector is not concerned.
func TestWMSPlantOnce(t *testing.T) {
	f := fixture.New(t)
	h := &integration.Endpoints{}
	create := func(name, plants string) (gen.Connector, error) {
		var out gen.Connector
		err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			res, err := h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.WMS, Name: name, Settings: decoded(t, `{`+wmsBase+`, "plants": `+plants+`}`),
				SecretRef: gen.NewOptString(vaulttest.TenantRef(f.Tenant.ID, "wms"))}, gen.CreateConnectorParams{})
			if err == nil {
				out = res.(*gen.ConnectorHeaders).Response
			}
			return err
		})
		return out, err
	}
	first, err := create("First", `{"1000": ["P1", "P2"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := create("Second", `{"1000": ["P2"]}`); problem.From(err).Code != "integration.plant_has_wms" || !strings.Contains(err.Error(), "First") {
		t.Errorf("a second connector for P2: got %v, want integration.plant_has_wms naming First", err)
	}
	if _, err := create("Other company", `{"2000": ["P2"]}`); err != nil {
		t.Errorf("another company's P2: got %v, want created", err)
	}
	update := func(id uuid.UUID, version int32, plants string) error {
		return apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			_, err := h.UpdateConnector(ctx, &gen.ConnectorUpdate{Settings: gen.NewOptConnectorUpdateSettings(gen.ConnectorUpdateSettings(decoded(t,
				`{`+wmsBase+`, "plants": `+plants+`}`)))}, gen.UpdateConnectorParams{ID: id, IfMatch: etag.Format(version)})
			return err
		})
	}
	if err := update(first.ID, first.Version, `{"1000": ["P1", "P2", "P3"]}`); err != nil {
		t.Errorf("the first keeping its plants: got %v, want changed", err)
	}
	third, err := create("Third", `{"1000": ["P4"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := update(third.ID, third.Version, `{"1000": ["P4", "P3"]}`); problem.From(err).Code != "integration.plant_has_wms" {
		t.Errorf("the third taking P3: got %v, want integration.plant_has_wms", err)
	}
	if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		_, err := integration.SetConnectorStatus(ctx, third.ID, func(int32) error { return nil }, "disabled", "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := update(third.ID, third.Version+1, `{"1000": ["P4", "P3"]}`); err != nil {
		t.Errorf("the disabled third naming P3: got %v, want changed", err)
	}
	err = apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		_, err := h.EnableConnector(ctx, gen.EnableConnectorParams{ID: third.ID, IfMatch: etag.Format(third.Version + 2)})
		return err
	})
	if problem.From(err).Code != "integration.plant_has_wms" {
		t.Errorf("enabling the third with P3: got %v, want integration.plant_has_wms", err)
	}
	pgtest.Exec(t, `UPDATE integration.connector SET settings = '{"adapter": "acme"}', version = version + 1 WHERE tenant_id = $1 AND id = $2`, f.Tenant.ID, first.ID)
	err = apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		_, err := h.EnableConnector(ctx, gen.EnableConnectorParams{ID: third.ID, IfMatch: etag.Format(third.Version + 2)})
		return err
	})
	if err != nil {
		t.Errorf("enabling the third once the first's settings no longer resolve (it manages nothing): got %v, want enabled", err)
	}
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', settings = '{"adapter": "acme"}', version = version + 1 WHERE tenant_id = $1 AND id = $2`, f.Tenant.ID,
		third.ID)
	err = apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		_, err := h.EnableConnector(ctx, gen.EnableConnectorParams{ID: third.ID, IfMatch: etag.Format(third.Version + 4)})
		return err
	})
	if problem.From(err).Code != "integration.invalid_settings" {
		t.Errorf("enabling a connector whose settings were changed by hand: got %v, want integration.invalid_settings", err)
	}
}

// TestWMSQueueEvents: the queue skips other events and those of
// another status, and refuses, logging, a malformed one or one whose
// subject names another tenant, acknowledging both.
func TestWMSQueueEvents(t *testing.T) {
	e := setupWMS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	m := e.busEvent(t, id)
	other := func(change func(*outbox.Message)) outbox.Message {
		h := map[string]string{}
		for k, v := range m.Header {
			h[k] = v
		}
		c := outbox.Message{ID: m.ID, Subject: m.Subject, Data: m.Data, Header: h}
		change(&c)
		return c
	}
	for name, c := range map[string]struct {
		m      outbox.Message
		logged bool
	}{
		"another type":   {other(func(c *outbox.Message) { c.Header["ce-type"] = "com.example.erp.shipping.other.v1" }), false},
		"malformed data": {other(func(c *outbox.Message) { c.Data = []byte(`{`) }), true},
		"another tenant": {other(func(c *outbox.Message) { c.Header["ce-tenantid"] = uuid.NewString() }), true},
		"another status": {other(func(c *outbox.Message) {
			c.Data = []byte(strings.Replace(string(c.Data), `"status": "created"`, `"status": "x"`, 1))
		}), false},
		"not its first event": {other(func(c *outbox.Message) {
			c.Data = []byte(strings.Replace(string(c.Data), `"version": 1`, `"version": 2`, 1))
		}), false},
	} {
		t.Run(name, func(t *testing.T) {
			var log pgtest.SyncBuffer
			if err := e.queue(t, &log).Handle(context.Background(), c.m); err != nil {
				t.Fatalf("got %v, want nil", err)
			}
			if got := strings.Contains(log.String(), "refused an event"); got != c.logged {
				t.Errorf("logged: got %v, want %v (%s)", got, c.logged, log.String())
			}
			page, _ := e.Must(200, "GET", "/v1/wms-messages?outbound_delivery_id="+id, "", "")
			if n := len(page["items"].([]any)); n != 0 {
				t.Errorf("messages: got %d, want 0", n)
			}
		})
	}
}

// wmsEntities is the directory, failing or knowing no company.
type wmsEntities struct {
	integration.LegalEntities
	fail bool
}

func (w wmsEntities) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	if w.fail {
		return tenancy.Entity{}, false, errors.New("directory down")
	}
	return tenancy.Entity{}, false, nil
}

// TestWMSFailures: creating, changing, enabling wms connectors and
// listing their messages fail with each database statement; the list
// fails when the directory fails or does not hold a message's company;
// production refuses a wms connector of another adapter by its settings.
func TestWMSFailures(t *testing.T) {
	e := setupWMS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	e.queued(t, id)
	h := app.Integration(app.Config{Pages: wmsPages(t)})
	conn := uuid.MustParse(e.conn)
	settings := `{` + wmsBase + `, "plants": {"2000": ["P7"]}}`
	for name, fn := range map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.WMS, Name: "Another", Settings: decoded(t, settings)}, gen.CreateConnectorParams{})
			return err
		},
		"change": func(ctx context.Context) error {
			_, err := h.UpdateConnector(ctx, &gen.ConnectorUpdate{Settings: gen.NewOptConnectorUpdateSettings(gen.ConnectorUpdateSettings(decoded(t,
				`{`+wmsBase+`, "plants": {"2000": ["P1", "P7"]}}`)))}, gen.UpdateConnectorParams{ID: conn, IfMatch: etag.Format(1)})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListWMSMessages(ctx, gen.ListWMSMessagesParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn) })
	}
	t.Run("enable", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, conn)
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "enable", func(ctx context.Context) error {
			_, err := h.EnableConnector(ctx, gen.EnableConnectorParams{ID: conn, IfMatch: etag.Format(2)})
			return err
		})
	})
	for name, c := range map[string]struct {
		entities integration.LegalEntities
		want     string
	}{
		"the directory down": {wmsEntities{fail: true}, "directory down"},
		"a company not held": {wmsEntities{}, "legal entity"},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Integration(app.Config{Pages: wmsPages(t)})
			h.LegalEntities = c.entities
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.ListWMSMessages(ctx, gen.ListWMSMessagesParams{})
				return err
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
	t.Run("outside a request", func(t *testing.T) {
		if _, err := h.ListWMSMessages(context.Background(), gen.ListWMSMessagesParams{}); err == nil {
			t.Error("got nil, want refused")
		}
	})
	t.Run("a page token of another listing", func(t *testing.T) {
		e.Problem(400, "request.invalid_page_token", "GET", "/v1/wms-messages?page_token=nonsense", "", "")
	})
	t.Run("pages", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE integration.connector SET status = 'active', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, conn)
		other, _ := e.delivery(t, e.p1, e.s1, "1")
		e.queued(t, other)
		first, _ := e.Must(200, "GET", "/v1/wms-messages?page_size=1", "", "")
		tok, _ := first["next_page_token"].(string)
		if tok == "" {
			t.Fatal("the first page names no next page")
		}
		second, _ := e.Must(200, "GET", "/v1/wms-messages?page_size=1&page_token="+tok, "", "")
		if a, b := first["items"].([]any)[0].(map[string]any)["id"], second["items"].([]any)[0].(map[string]any)["id"]; a == b {
			t.Errorf("the second page: got %v again, want the next message", b)
		}
	})
	t.Run("another adapter in production", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := (&integration.Endpoints{RefuseEBICS: true}).CreateConnector(ctx, &gen.ConnectorCreate{Kind: integration.WMS, Name: "Acme",
				Settings: decoded(t, `{"adapter": "acme", "base_url": "https://wms.example", "plants": {"2000": ["P8"]}}`)}, gen.CreateConnectorParams{})
			return err
		})
		if problem.From(err).Code != "integration.invalid_settings" {
			t.Errorf("got %v, want integration.invalid_settings", err)
		}
	})
}

func wmsPages(t *testing.T) *pagination.Codec {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

// TestWMSConnectorEditDoesNotStallQueue: a change of a wms connector's
// settings holds its row FOR NO KEY UPDATE, which leaves the key share
// the queue's insert takes for its foreign key free, so a delivery is
// queued for the connector while the change is still open (FOR UPDATE
// held the queue up until the change ended).
func TestWMSConnectorEditDoesNotStallQueue(t *testing.T) {
	e := setupWMS(t)
	id, _ := e.delivery(t, e.p1, e.s1, "1")
	m := e.busEvent(t, id)
	h := app.Integration(app.Config{Pages: wmsPages(t)})
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		if _, err := h.UpdateConnector(ctx, &gen.ConnectorUpdate{Settings: gen.NewOptConnectorUpdateSettings(gen.ConnectorUpdateSettings(decoded(t,
			`{`+wmsBase+`, "plants": {"2000": ["P1", "P7"]}}`)))}, gen.UpdateConnectorParams{ID: uuid.MustParse(e.conn), IfMatch: etag.Format(1)}); err != nil {
			return err
		}
		qctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var log pgtest.SyncBuffer
		return e.queue(t, &log).Handle(qctx, m)
	})
	if err != nil {
		t.Fatalf("the queue while the change was open: got %v, want queued", err)
	}
	if got := e.message(t, id)["status"]; got != "pending" {
		t.Errorf("the delivery's message: got %v, want pending", got)
	}
}
