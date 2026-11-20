package einvoicing_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// sellerParticipant is company 5000's participant at Evatra: 0235 and its
// TRN's first ten digits (ADR-0061 §5 C6).
const sellerParticipant = "0235:" + "1002345678"

// sender is the UAE scene with Evatra's stand-in, an evatra connector
// naming 5000's participant, the endpoints on a clock the test moves, and
// a valid tax invoice to send.
type sender struct {
	*uae
	srv       *einvoicetest.Server
	cfg       app.Config
	e         *einvoicing.Endpoints
	connector uuid.UUID
	invoice   map[string]any // the valid e-invoice
	pool      *database.Pool
	mu        sync.Mutex
	now       time.Time
}

func (s *sender) clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// later moves the clock on by d.
func (s *sender) later(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

func senderSetup(t *testing.T) *sender {
	t.Helper()
	s := &sender{uae: uaeSetup(t, dubai), srv: einvoicetest.NewServer(t, "tok"), now: time.Now().UTC(), pool: pgtest.Pool(t)}
	client, err := einvoicing.NewClient(egress.LoopbackForTests(s.srv.Roots))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	s.cfg = app.Config{Secrets: vaulttest.ByPath{"provider": "tok", "evatra": evatraSecret}, Objects: s.store, Providers: client, Pages: pages,
		EvatraHosts: map[string]string{"stage": s.srv.EvatraURL, "prod": s.srv.EvatraURL}, FieldKeys: apitest.FieldKeys(), Now: s.clock}
	s.e = app.EInvoicing(s.cfg)
	s.f.API = apitest.NewWith(t, func(c *app.Config) {
		c.Objects, c.Secrets, c.Providers, c.EvatraHosts, c.Now = s.store, s.cfg.Secrets, client, s.cfg.EvatraHosts, s.clock
	})
	s.connector = s.sendConnector(t, "Maple Gulf on Evatra", "evatra", einvoicetest.EvatraCompany, map[string]string{"5000": sellerParticipant})
	t.Cleanup(func() { // the test carries out its own sends, never another package's runner
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1
			WHERE tenant_id = $1 AND kind = $2 AND workflow_id IS NULL`, s.f.Tenant.ID, einvoicing.KindSend)
	})
	id := s.posted(t, s.customer, "", "AED", s.item("10000.00", "AED", "S5@AE-DU"))
	s.invoice = s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, id))
	return s
}

// sendConnector adds an active einvoice_provider connector of the adapter
// with Evatra's company id and the participants.
func (s *sender) sendConnector(t *testing.T, name, adapter, company string, participants map[string]string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	settings := map[string]any{"adapter": adapter, "format": "pint_ae", "participants": participants}
	path := "provider"
	if adapter == "evatra" {
		settings["environment"], settings["company_id"], path = "stage", company, "evatra"
	} else {
		settings["base_url"] = s.srv.URL
	}
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'einvoice_provider', $3, $4, $5, 'active', $6, $6)`,
		s.f.Tenant.ID, id, name, settings, vaulttest.TenantRef(s.f.Tenant.ID, path), s.f.Tenant.Human.ID)
	return id
}

// send asks for the e-invoice to be sent through the connector and
// answers its Operation's id.
func (s *sender) send(t *testing.T, id string, connector uuid.UUID) uuid.UUID {
	t.Helper()
	op, _ := s.f.Must(202, "POST", "/v1/outgoing-e-invoices/"+id+":send", fmt.Sprintf(`{"connector_id": %q, "business_type": "B2B"}`, connector), s.tag(t, id))
	opID := uuid.MustParse(op["id"].(string))
	pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1 WHERE id = $1`, opID)
	return opID
}

// tag is the e-invoice's ETag as read now.
func (s *sender) tag(t *testing.T, id string) string {
	t.Helper()
	_, tag := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	return tag
}

// within is the send's Within: a committed transaction of the tenant as
// the sender.
func (s *sender) within() einvoicing.Within {
	return func(ctx context.Context, fn func(ctx context.Context) error) error {
		return uow.Job{Begin: s.pool.Begin, Scope: database.Scope{Role: "erp_app", TenantID: s.f.Tenant.ID}, Name: "test send"}.As(ctx,
			func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
				return einvoicing.Sender(s.f.Tenant.ID), true, nil
			}, fn)
	}
}

// advance takes one step of the e-invoice's send.
func (s *sender) advance(t *testing.T, id string, op uuid.UUID) einvoicing.Step {
	t.Helper()
	step, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, s.within())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	return step
}

// transmission is the e-invoice's transmission as read: status, provider
// status, attempts, resends and problem code.
func (s *sender) transmission(t *testing.T, id string) string {
	t.Helper()
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	tr, ok := got["transmission"].(map[string]any)
	if !ok {
		return "none"
	}
	code := "-"
	if p, ok := tr["problem"].(map[string]any); ok {
		code = p["code"].(string)
	}
	return words(tr["status"], tr["provider_status"], tr["attempts"], tr["resends"], code)
}

// operation is the Operation's status with its resource or problem code.
func operation(t *testing.T, op uuid.UUID) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT status || ' ' || coalesce(resource, '-') || ' ' || coalesce(error->>'code', '-') FROM platform.operation WHERE id = $1`,
		[]any{op}, &got)
	return got
}

// TestSendEInvoice: a valid e-invoice is queued for Evatra with its
// Operation, audited and published, before any call; the first step sends
// its XML as kept (B2B, UAE_PINT, ERP) and records the tracking id; a step
// before the status is due calls nothing; the status read COMPLETED
// completes it and succeeds the Operation with the e-invoice; a step run
// again changes nothing and sends nothing; a second send is refused.
func TestSendEInvoice(t *testing.T) {
	s := senderSetup(t)
	id := s.invoice["id"].(string)
	op := s.send(t, id, s.connector)
	check(t, "queued", s.transmission(t, id), "queued <nil> 0 0 -")
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	tr := got["transmission"].(map[string]any)
	check(t, "its connector and operation", words(tr["connector_name"], tr["operation_id"], tr["business_type"]), words("Maple Gulf on Evatra", op, "B2B"))
	check(t, "nothing sent before the step", len(s.srv.Evatra.Sends()), 0)
	step := s.advance(t, id, op)
	check(t, "the first step", words(step.Done, step.Wait), "false 30s")
	sent := s.srv.Evatra.Sends()
	content := s.f.API.Do(apitest.Req{Method: "GET", Path: "/v1/outgoing-e-invoices/" + id + "/content", Tenant: s.f.Tenant.ID, As: s.f.Tenant.Human})
	check(t, "sent", words(len(sent), sent[0].Filename, sent[0].BusinessType, sent[0].InvoiceType, sent[0].InvoiceSource, sent[0].CompanyID,
		string(sent[0].Document) == content.Body.String()), words(1, s.invoice["customer_invoice_number"], "B2B UAE_PINT ERP", einvoicetest.EvatraCompany, true))
	check(t, "submitted", s.transmission(t, id), "submitted PENDING 1 0 -")
	s.later(10 * time.Second)
	if step := s.advance(t, id, op); step.Done || step.Wait != 20*time.Second {
		t.Errorf("before the status read is due: got %+v, want a wait of 20s", step)
	}
	s.later(20 * time.Second)
	if step := s.advance(t, id, op); !step.Done {
		t.Errorf("the status read COMPLETED: got %+v, want done", step)
	}
	check(t, "completed", s.transmission(t, id), "completed COMPLETED 1 0 -")
	check(t, "the operation", operation(t, op), "succeeded /v1/outgoing-e-invoices/"+id+" -")
	if step := s.advance(t, id, op); !step.Done || len(s.srv.Evatra.Sends()) != 1 {
		t.Errorf("run again: got %+v and %d sends, want done and one send", step, len(s.srv.Evatra.Sends()))
	}
	var events, audits int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM einvoicing.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.einvoicing.outgoing_einvoice_transmission_changed.v1'),
		(SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action IN ('einvoicing.outgoing.send', 'einvoicing.outgoing.transmission'))`,
		[]any{s.f.Tenant.ID}, &events, &audits)
	check(t, "events and audit records: queued, sending, submitted, completed", words(events, audits), "4 4")
	s.f.Problem(409, "einvoicing.already_transmitted", "POST", "/v1/outgoing-e-invoices/"+id+":send",
		fmt.Sprintf(`{"connector_id": %q, "business_type": "B2B"}`, s.connector), s.tag(t, id))
	page, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?transmission_status=completed", "", "")
	check(t, "listed as completed", len(page["items"].([]any)), 1)
	none, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?transmission_status=none", "", "")
	check(t, "none unsent", len(none["items"].([]any)), 0)
}

// TestSendLostAnswer: Evatra takes the document but its answer is lost;
// the call is tried again after a minute, and since a call was made
// before, the document is looked up by its filename first and the
// submission found is taken: it is not sent twice (ADR-0061 As built E3).
func TestSendLostAnswer(t *testing.T) {
	s := senderSetup(t)
	id := s.invoice["id"].(string)
	op := s.send(t, id, s.connector)
	s.srv.Evatra.LoseAnswers(1)
	if step := s.advance(t, id, op); step.Done || step.Wait != time.Minute {
		t.Errorf("the answer lost: got %+v, want a wait of a minute", step)
	}
	check(t, "queued again", s.transmission(t, id), "queued <nil> 1 0 -")
	s.later(time.Minute)
	s.advance(t, id, op)
	check(t, "found, not sent again", words(len(s.srv.Evatra.Sends()), s.transmission(t, id)), "1 submitted PENDING 2 0 -")
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	check(t, "its tracking id", got["transmission"].(map[string]any)["tracking_id"], s.srv.Evatra.Sends()[0].TrackingID)
}

// TestSendResend: Evatra fails to send the document (SENDING_FAILED): it
// is sent again, the failed submission not taken as found; the second
// submission's PERMANENT_FAILED fails it and its Operation, and it is on
// the worklist of failed submissions; a person sends it again (a new
// Operation), the old one's step done; after three resends a fourth
// SENDING_FAILED fails it with einvoicing.sending_failed.
func TestSendResend(t *testing.T) {
	s := senderSetup(t)
	id := s.invoice["id"].(string)
	number := s.invoice["customer_invoice_number"].(string)
	s.srv.Evatra.Script(number, []string{"SENDING_FAILED"}, []string{"PERMANENT_FAILED"}, []string{"SENDING_FAILED"})
	op := s.send(t, id, s.connector)
	s.advance(t, id, op)
	s.later(time.Minute)
	if step := s.advance(t, id, op); step.Done || step.Wait != 0 {
		t.Errorf("SENDING_FAILED: got %+v, want it sent again at once", step)
	}
	check(t, "to be sent again", s.transmission(t, id), "queued SENDING_FAILED 0 1 -")
	s.advance(t, id, op)
	check(t, "sent again", words(len(s.srv.Evatra.Sends()), s.transmission(t, id)), "2 submitted PENDING 1 1 -")
	s.later(time.Minute)
	if step := s.advance(t, id, op); !step.Done {
		t.Errorf("PERMANENT_FAILED: got %+v, want done", step)
	}
	check(t, "failed", s.transmission(t, id), "failed PERMANENT_FAILED 1 1 einvoicing.permanent_failure")
	check(t, "its operation", operation(t, op), "failed /v1/outgoing-e-invoices/"+id+" einvoicing.permanent_failure")
	page, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?transmission_status=failed", "", "")
	check(t, "the worklist", len(page["items"].([]any)), 1)
	again := s.send(t, id, s.connector)
	check(t, "sent again by a person", s.transmission(t, id), "queued <nil> 0 1 -")
	if step := s.advance(t, id, op); !step.Done {
		t.Errorf("the old send's step: got %+v, want done", step)
	}
	for i := 0; i < 8 && !s.advance(t, id, again).Done; i++ {
		s.later(time.Minute)
	}
	check(t, "after three resends", s.transmission(t, id), "failed SENDING_FAILED 1 3 einvoicing.sending_failed")
}
