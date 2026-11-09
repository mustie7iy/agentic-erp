package einvoicing_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// listScene is setup with three documents recorded by one fetch: MSG-1
// for 1000, MSG-2 for 2000 and MSG-3 for none of the companies, whose
// acknowledgement was lost.
func listScene(t *testing.T) (*scene, uuid.UUID) {
	t.Helper()
	s := setup(t)
	s.srv.Deliver("MSG-1", usParticipant, einvoicetest.Invoice("INV-1"))
	s.srv.Deliver("MSG-2", deParticipant, einvoicetest.Invoice("INV-2"))
	s.srv.Deliver("MSG-3", "", einvoicetest.Invoice("INV-3"))
	s.srv.Fail("POST /messages/MSG-3/acknowledge", 1) // MSG-3 recorded, its acknowledgement lost
	op := s.fetch(t, s.connector)
	ctx := context.Background()
	pending, err := s.e.PendingEInvoices(ctx, s.connector, s.within())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pending {
		if _, err := s.e.RecordEInvoice(ctx, s.connector, op, m, s.within()); err != nil && m.ID != "MSG-3" {
			t.Fatal(err)
		}
	}
	return s, op
}

// TestListEInvoices: the list names each document's connector and
// company, newest received first, filtered by company, connector, status,
// fetch and when received, and paged by cursor; a person sees the
// documents of the companies they may act in, and those addressed to none
// only when they may act in every company (ADR-0061 §15).
func TestListEInvoices(t *testing.T) {
	s, op := listScene(t)
	s.f.API = apitest.NewWith(t, func(c *app.Config) { c.Objects, c.Providers, c.Secrets = s.cfg.Objects, s.cfg.Providers, s.cfg.Secrets })
	page, _ := s.f.Must(http.StatusOK, http.MethodGet, "/v1/e-invoices", "", "")
	items := apitest.List(t, page, "items")
	if got := messages(items); got != "MSG-3 MSG-2 MSG-1" {
		t.Errorf("the list: got %s, want MSG-3 MSG-2 MSG-1, newest first", got)
	}
	first := items[2].(map[string]any)
	for field, want := range map[string]any{"connector_name": "Provider", "company_code": "1000", "company_name": "Acme Corp US", "status": "received",
		"receiver": usParticipant, "fetch_operation_id": op.String()} {
		if first[field] != want {
			t.Errorf("MSG-1's %s: got %v, want %v", field, first[field], want)
		}
	}
	if none := items[0].(map[string]any); none["legal_entity_id"] != nil || none["company_code"] != nil || none["receiver"] != nil ||
		none["acknowledged_at"] != nil {
		t.Errorf("MSG-3: got %v, want no company, no receiver and not acknowledged", none)
	}
	in := func(when time.Time) string {
		return strings.ReplaceAll(when.UTC().Format(time.RFC3339Nano), "+", "%2B")
	}
	for query, want := range map[string]string{
		"legal_entity_id=" + s.f.US.String():             "MSG-1",
		"connector_id=" + s.connector.String():           "MSG-3 MSG-2 MSG-1",
		"connector_id=" + uuid.NewString():               "",
		"status=received":                                "MSG-3 MSG-2 MSG-1",
		"status=entered":                                 "",
		"fetch_operation_id=" + op.String():              "MSG-3 MSG-2 MSG-1",
		"fetch_operation_id=" + uuid.NewString():         "",
		"received_from=" + in(time.Now().Add(time.Hour)): "",
		"received_to=" + in(time.Now().Add(time.Hour)):   "MSG-3 MSG-2 MSG-1",
	} {
		t.Run(query, func(t *testing.T) {
			page, _ := s.f.In(t).Must(http.StatusOK, http.MethodGet, "/v1/e-invoices?"+query, "", "")
			if got := messages(apitest.List(t, page, "items")); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	t.Run("paged", func(t *testing.T) {
		page, _ := s.f.In(t).Must(http.StatusOK, http.MethodGet, "/v1/e-invoices?page_size=2", "", "")
		token := apitest.String(t, page, "next_page_token")
		next, _ := s.f.In(t).Must(http.StatusOK, http.MethodGet, "/v1/e-invoices?page_size=2&page_token="+token, "", "")
		if got := messages(apitest.List(t, page, "items")) + " | " + messages(apitest.List(t, next, "items")); got != "MSG-3 MSG-2 | MSG-1" {
			t.Errorf("got %s, want MSG-3 MSG-2 | MSG-1", got)
		}
		if _, ok := next["next_page_token"]; ok {
			t.Error("the last page: got a next page token, want none")
		}
	})
	t.Run("a person of company 1000 alone", func(t *testing.T) {
		clerk := s.f.Tenant.Service // holds the read permission, and access to no company yet
		pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`, s.f.Tenant.ID, clerk.ID, s.f.US, s.f.Tenant.Human.ID)
		as := s.f.Client
		as.As = clerk
		page, _ := as.In(t).Must(http.StatusOK, http.MethodGet, "/v1/e-invoices", "", "")
		if got := messages(apitest.List(t, page, "items")); got != "MSG-1" {
			t.Errorf("the list: got %s, want MSG-1 alone", got)
		}
		var other string
		pgtest.Scan(t, `SELECT id FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'MSG-3'`, []any{s.f.Tenant.ID}, &other)
		as.In(t).Problem(http.StatusNotFound, "request.not_found", http.MethodGet, "/v1/e-invoices/"+other, "", "")
	})
}

// messages are the items' message ids, in order.
func messages(items []any) string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.(map[string]any)["message_id"].(string))
	}
	return strings.Join(ids, " ")
}

// TestGetEInvoice: one document as the list gives it, with its ETag; one
// that does not exist is not found.
func TestGetEInvoice(t *testing.T) {
	s, _ := listScene(t)
	var id uuid.UUID
	pgtest.Scan(t, `SELECT id FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'MSG-2'`, []any{s.f.Tenant.ID}, &id)
	err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
		res, err := s.e.GetEInvoice(ctx, gen.GetEInvoiceParams{ID: id})
		if err != nil {
			return err
		}
		h := res.(*gen.EInvoiceHeaders)
		if h.ETag.Value != `"2"` || h.Response.CompanyCode.Value != "2000" || h.Response.ConnectorName != "Provider" || h.Response.AcknowledgedAt.Null {
			t.Errorf("got %+v, want MSG-2 of 2000 acknowledged, its ETag 2", h)
		}
		if _, err := s.e.GetEInvoice(ctx, gen.GetEInvoiceParams{ID: uuid.New()}); problem.From(err).Code != "request.not_found" {
			t.Errorf("an unknown e-invoice: got %v, want request.not_found", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("outside a unit of work and failing statements", func(t *testing.T) {
		for name, call := range map[string]func(ctx context.Context) error{
			"list": func(ctx context.Context) error {
				_, err := s.e.ListEInvoices(ctx, gen.ListEInvoicesParams{})
				return err
			},
			"get": func(ctx context.Context) error {
				_, err := s.e.GetEInvoice(ctx, gen.GetEInvoiceParams{ID: id})
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				if err := call(context.Background()); err == nil {
					t.Error("outside a unit of work: got nil error, want an error")
				}
				apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, name, call)
			})
		}
	})
	t.Run("a bad page token", func(t *testing.T) {
		err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := s.e.ListEInvoices(ctx, gen.ListEInvoicesParams{PageToken: gen.NewOptString("x")})
			return err
		})
		if problem.From(err).Code != "request.invalid_page_token" {
			t.Errorf("got %v, want request.invalid_page_token", err)
		}
	})
	t.Run("a name unknown to its context", func(t *testing.T) {
		e := app.EInvoicing(s.cfg)
		e.Connectors = forgetful{e.Connectors}
		err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := e.GetEInvoice(ctx, gen.GetEInvoiceParams{ID: id})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "not held by the integration context") {
			t.Errorf("got %v, want the connector named missing", err)
		}
	})
}

// forgetful is connectors that know no names.
type forgetful struct{ einvoicing.Connectors }

func (forgetful) Names(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return map[uuid.UUID]string{}, nil
}
