package einvoicing_test

import (
	"context"
	"errors"
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
	// The clock starts on a whole microsecond: PostgreSQL keeps microseconds, so a
	// due time read back from a row taken on Linux's nanosecond clock would make
	// every expected wait a fraction of a microsecond short.
	s := &sender{uae: uaeSetup(t, dubai), srv: einvoicetest.NewServer(t, "tok"), now: time.Now().UTC().Truncate(time.Microsecond), pool: pgtest.Pool(t)}
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
// completes it and succeeds the Operation with the e-invoice, its delivery
// read due 30 seconds later (As built E4); a step run again changes
// nothing and sends nothing; a second send is refused.
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
	if step := s.advance(t, id, op); step.Done || step.Wait != 30*time.Second {
		t.Errorf("the status read COMPLETED: got %+v, want its delivery read in 30s (As built E4)", step)
	}
	check(t, "completed", s.transmission(t, id), "completed COMPLETED 1 0 -")
	check(t, "the operation", operation(t, op), "succeeded /v1/outgoing-e-invoices/"+id+" -")
	if step := s.advance(t, id, op); step.Done || step.Wait != 30*time.Second || len(s.srv.Evatra.Sends()) != 1 {
		t.Errorf("run again: got %+v and %d sends, want the same wait and one send", step, len(s.srv.Evatra.Sends()))
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
// Operation, its attempts and resends counted afresh), the old one's step
// done; after three resends a fourth SENDING_FAILED fails it with
// einvoicing.sending_failed.
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
	check(t, "sent again by a person, counted afresh", s.transmission(t, id), "queued <nil> 0 0 -")
	if step := s.advance(t, id, op); !step.Done {
		t.Errorf("the old send's step: got %+v, want done", step)
	}
	for i := 0; i < 12 && !s.advance(t, id, again).Done; i++ {
		s.later(time.Minute)
	}
	check(t, "after three resends", s.transmission(t, id), "failed SENDING_FAILED 1 3 einvoicing.sending_failed")
}

// cancelSend cancels the send's Operation as a person does.
func (s *sender) cancelSend(t *testing.T, op uuid.UUID) {
	t.Helper()
	_, tag := s.f.Must(200, "GET", "/v1/operations/"+op.String(), "", "")
	s.f.Must(200, "POST", "/v1/operations/"+op.String()+":cancel", "", tag)
}

// TestSendCancelled: a send cancelled before its first step is ended by
// it, nothing sent; one cancelled once submitted is ended by its next
// step, its tracking id kept, and a person's send again looks it up and
// takes the submission found, not sending it twice; one cancelled during
// its call ends with the provider's answer kept, unless the answer is
// final (Evatra refusing it, COMPLETED), which is recorded as it is; one
// cancelled while a claimed call may be under way waits for the claim to
// lapse. The others end failed with einvoicing.send_cancelled; the
// Operation stays cancelled.
func TestSendCancelled(t *testing.T) {
	s := senderSetup(t)
	next := func(t *testing.T) string {
		t.Helper()
		return s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
	}
	t.Run("before its first step", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		s.cancelSend(t, op)
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("got %+v, want done", step)
		}
		check(t, "ended, nothing sent", words(s.transmission(t, id), operation(t, op), len(s.srv.Evatra.Sends())),
			"failed <nil> 0 0 einvoicing.send_cancelled cancelled /v1/outgoing-e-invoices/"+id+" - 0")
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("run again: got %+v, want done", step)
		}
	})
	t.Run("once submitted, then sent again", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		s.advance(t, id, op)
		sends := len(s.srv.Evatra.Sends())
		tracking := s.srv.Evatra.Sends()[sends-1].TrackingID
		s.cancelSend(t, op)
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("got %+v, want done at once, its read not due", step)
		}
		got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
		check(t, "ended, its tracking id kept", words(s.transmission(t, id), got["transmission"].(map[string]any)["tracking_id"]),
			"failed PENDING 1 0 einvoicing.send_cancelled "+tracking)
		again := s.send(t, id, s.connector)
		s.advance(t, id, again)
		got, _ = s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
		check(t, "found, not sent twice", words(len(s.srv.Evatra.Sends()), s.transmission(t, id), got["transmission"].(map[string]any)["tracking_id"]),
			words(sends, "submitted PENDING 1 0 - "+tracking))
	})
	t.Run("during its call", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		calls := 0
		cancelling := func(c context.Context, fn func(ctx context.Context) error) error {
			calls++
			err := s.within()(c, fn)
			if calls == 4 { // the read, the connector, the XML, then the claim's commit: cancelled while the call is made
				s.cancelSend(t, op)
			}
			return err
		}
		if step, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, cancelling); err != nil || !step.Done {
			t.Errorf("got %+v and %v, want done", step, err)
		}
		got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
		check(t, "the answer kept", words(s.transmission(t, id), got["transmission"].(map[string]any)["tracking_id"], operation(t, op)),
			words("failed PENDING 1 0 einvoicing.send_cancelled", s.srv.Evatra.Sends()[len(s.srv.Evatra.Sends())-1].TrackingID,
				"cancelled /v1/outgoing-e-invoices/"+id+" -"))
	})
	t.Run("during each kind of call", func(t *testing.T) {
		refuses := s.sendConnector(t, "Refused while cancelled", "evatra", "NOT-OURS", map[string]string{"5000": sellerParticipant})
		failed, refused, processing, completed, readFails := next(t), next(t), next(t), next(t), next(t)
		got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+processing, "", "")
		s.srv.Evatra.Script(got["customer_invoice_number"].(string), []string{"PROCESSING"})
		ops := map[string]uuid.UUID{failed: s.send(t, failed, s.connector), refused: s.send(t, refused, refuses),
			processing: s.send(t, processing, s.connector), completed: s.send(t, completed, s.connector), readFails: s.send(t, readFails, s.connector)}
		s.advance(t, processing, ops[processing]) // submitted
		s.advance(t, completed, ops[completed])
		s.advance(t, readFails, ops[readFails])
		tracking := s.srv.Evatra.Sends()[len(s.srv.Evatra.Sends())-1].TrackingID
		s.later(30 * time.Second)
		for _, id := range []string{failed, refused, processing, completed, readFails} {
			switch id {
			case failed:
				s.srv.Fail("POST /evatra/invoice/send-document-async", 1)
			case readFails:
				s.srv.Fail("GET /evatra/invoice/document-status/"+tracking, 1)
			}
			claimed := 4 // the read, the connector, the XML, then the claim's commit
			if id == processing || id == completed || id == readFails {
				claimed = 3 // a status read reads no XML
			}
			calls := 0
			cancelling := func(ctx context.Context, fn func(ctx context.Context) error) error {
				calls++
				err := s.within()(ctx, fn)
				if calls == claimed { // cancelled while the call is made
					s.cancelSend(t, ops[id])
				}
				return err
			}
			if _, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), ops[id], cancelling); err != nil {
				t.Errorf("%s: %v", id, err)
			}
		}
		check(t, "a failed call: nothing to keep", s.transmission(t, failed), "failed <nil> 1 0 einvoicing.send_cancelled")
		check(t, "a refusal: final, recorded as it is", s.transmission(t, refused), "failed <nil> 1 0 einvoicing.send_refused")
		check(t, "a status in progress: kept", s.transmission(t, processing), "failed PROCESSING 1 0 einvoicing.send_cancelled")
		check(t, "a status read failing: the status before kept", s.transmission(t, readFails), "failed PENDING 1 0 einvoicing.send_cancelled")
		check(t, "COMPLETED: final, recorded as it is", words(s.transmission(t, completed), operation(t, ops[completed])),
			"completed COMPLETED 1 0 - cancelled /v1/outgoing-e-invoices/"+completed+" -")
	})
	t.Run("failing statements on the way to its end", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		s.cancelSend(t, op)
		s.sendEveryFailure(t, func(in einvoicing.Within) error {
			_, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, in)
			return err
		})
		check(t, "ended", s.transmission(t, id), "failed <nil> 0 0 einvoicing.send_cancelled")
	})
	t.Run("a call claimed", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET status = 'sending', attempts = 1, next_check_at = $2, version = version + 1
			WHERE outgoing_einvoice_id = $1`, id, s.clock().Add(time.Minute))
		s.cancelSend(t, op)
		if step := s.advance(t, id, op); step.Done || step.Wait != time.Minute {
			t.Errorf("its claim not lapsed: got %+v, want a wait of a minute", step)
		}
		check(t, "still claimed", s.transmission(t, id), "sending <nil> 1 0 -")
		s.later(time.Minute)
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("its claim lapsed: got %+v, want done", step)
		}
		check(t, "ended", s.transmission(t, id), "failed <nil> 1 0 einvoicing.send_cancelled")
	})
}

// TestUnfinishedSends: the in-process sender's work in a tenant is the
// submissions no workflow carries out that are due, or whose Operation was
// cancelled, and not those finished or not yet due.
func TestUnfinishedSends(t *testing.T) {
	s := senderSetup(t)
	ids := map[string]string{}
	ops := map[string]uuid.UUID{}
	for _, name := range []string{"due", "not due", "cancelled", "finished"} {
		ids[name] = s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
		ops[name] = s.send(t, ids[name], s.connector)
	}
	pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET status = 'sending', attempts = 1, next_check_at = $2, version = version + 1
		WHERE outgoing_einvoice_id = ANY($1::uuid[])`, []string{ids["not due"], ids["cancelled"]}, s.clock().Add(time.Hour))
	pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET status = 'failed', next_check_at = NULL, problem_code = 'einvoicing.test', problem_detail = 'failed',
		version = version + 1 WHERE outgoing_einvoice_id = $1`, ids["finished"])
	s.cancelSend(t, ops["cancelled"])
	var got map[uuid.UUID]uuid.UUID
	err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		// as a cell without a workflow engine has them: within this transaction alone, so no other runner sees them
		if _, err := tx.Exec(ctx, `UPDATE platform.operation SET workflow_id = NULL, version = version + 1 WHERE id = ANY($1)`,
			[]uuid.UUID{ops["due"], ops["not due"], ops["cancelled"], ops["finished"]}); err != nil {
			return err
		}
		got, err = s.e.UnfinishedSends(ctx, tx, s.clock())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	mine := map[uuid.UUID]uuid.UUID{}
	for _, name := range []string{"due", "not due", "cancelled", "finished"} {
		if op, ok := got[uuid.MustParse(ids[name])]; ok {
			mine[uuid.MustParse(ids[name])] = op
		}
	}
	want := map[uuid.UUID]uuid.UUID{uuid.MustParse(ids["due"]): ops["due"], uuid.MustParse(ids["cancelled"]): ops["cancelled"]}
	check(t, "the sends to advance", fmt.Sprint(mine), fmt.Sprint(want))
	if err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		_, err = s.e.UnfinishedSends(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}, s.clock())
		return err
	}); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("its statement failing: got %v, want the injected failure", err)
	}
}
