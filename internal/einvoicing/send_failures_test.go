package einvoicing_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestSendRefusals: a send is refused before anything is queued for an
// invalid e-invoice, through the stand-in's own protocol, through a
// connector naming no participant for the company or disabled, for an
// e-invoice queued already, and for one of no company the caller may act
// in; a business type outside B2B and B2G is refused by the contract.
func TestSendRefusals(t *testing.T) {
	s := senderSetup(t)
	id := s.invoice["id"].(string)
	bare := s.newCustomer(t, "BP80000002", "No TRN Trading", "", "AE-DU")
	invalid := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, bare, "", "AED", s.item("10.00", "AED", "Z0"))))
	standIn := s.sendConnector(t, "Stand-in", "standin", "", map[string]string{"5000": sellerParticipant})
	other := s.sendConnector(t, "Other company", "evatra", einvoicetest.EvatraCompany, map[string]string{"2000": "0235:2000000000"})
	off := s.sendConnector(t, "Off", "evatra", einvoicetest.EvatraCompany, map[string]string{"5000": sellerParticipant})
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, off)
	body := func(c uuid.UUID) string { return fmt.Sprintf(`{"connector_id": %q, "business_type": "B2G"}`, c) }
	for _, c := range []struct {
		name, id, body, code string
		status               int
	}{
		{"an invalid e-invoice", invalid["id"].(string), body(s.connector), "einvoicing.document_invalid", 409},
		{"the stand-in's protocol", id, body(standIn), "einvoicing.adapter_cannot_send", 422},
		{"no participant for the company", id, body(other), "einvoicing.not_a_participant", 422},
		{"a disabled connector", id, body(off), "integration.connector_disabled", 409},
		{"an unknown e-invoice", uuid.NewString(), body(s.connector), "request.not_found", 404},
		{"a business type outside the mandate", id, fmt.Sprintf(`{"connector_id": %q, "business_type": "B2C"}`, s.connector), "einvoicing.unknown_business_type", 422},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.f.Problem(c.status, c.code, "POST", "/v1/outgoing-e-invoices/"+c.id+":send", c.body, `"1"`)
		})
	}
	check(t, "nothing queued", s.transmission(t, id), "none")
	s.f.Problem(428, "request.precondition_required", "POST", "/v1/outgoing-e-invoices/"+id+":send", body(s.connector), "")
	stale := s.tag(t, id)
	s.send(t, id, s.connector)
	s.f.Problem(412, "request.version_mismatch", "POST", "/v1/outgoing-e-invoices/"+id+":send", body(s.connector), stale)
	s.f.Problem(409, "einvoicing.transmission_in_progress", "POST", "/v1/outgoing-e-invoices/"+id+":send", body(s.connector), s.tag(t, id))
}

// TestSendProviderFailures: Evatra refusing the document fails it at once
// (einvoicing.send_refused); five calls failing fail it as unavailable,
// each tried again after its backoff; a connector disabled since fails it
// with the connector's problem; FAILED fails it (einvoicing.provider_failed);
// a status read failing, or still PROCESSING, is read again after the
// backoff, and 500 reads without an end fail it as unconfirmed; a stored
// XML that does not match its hash is not sent.
func TestSendProviderFailures(t *testing.T) {
	s := senderSetup(t)
	next := func(t *testing.T) string { // a valid e-invoice of a new posted invoice
		t.Helper()
		got := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))
		return got["id"].(string)
	}
	t.Run("refused", func(t *testing.T) {
		wrong := s.sendConnector(t, "Wrong company", "evatra", "NOT-OURS", map[string]string{"5000": sellerParticipant})
		id := next(t)
		op := s.send(t, id, wrong)
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("got %+v, want done", step)
		}
		check(t, "refused", words(s.transmission(t, id), operation(t, op)), "failed <nil> 1 0 einvoicing.send_refused failed /v1/outgoing-e-invoices/"+id+
			" einvoicing.send_refused")
	})
	t.Run("unavailable", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		s.srv.Fail("POST /evatra/invoice/send-document-async", 5)
		var waits []string
		for i := 0; i < 5; i++ {
			step := s.advance(t, id, op)
			waits = append(waits, fmt.Sprint(step.Wait))
			s.later(step.Wait)
		}
		check(t, "the backoff", strings.Join(waits, " "), "1m0s 5m0s 15m0s 1h0m0s 0s")
		check(t, "after five calls", s.transmission(t, id), "failed <nil> 5 0 einvoicing.provider_unavailable")
	})
	t.Run("the connector disabled since", func(t *testing.T) {
		conn := s.sendConnector(t, "Soon off", "evatra", einvoicetest.EvatraCompany, map[string]string{"5000": sellerParticipant})
		id := next(t)
		op := s.send(t, id, conn)
		pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, conn)
		s.advance(t, id, op)
		check(t, "failed, no call made", s.transmission(t, id), "failed <nil> 0 0 integration.connector_disabled")
	})
	t.Run("status reads", func(t *testing.T) {
		id := next(t)
		got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
		s.srv.Evatra.Script(got["customer_invoice_number"].(string), []string{"PROCESSING", "PROCESSING", "FAILED"})
		op := s.send(t, id, s.connector)
		s.advance(t, id, op)
		tracking := s.srv.Evatra.Sends()[len(s.srv.Evatra.Sends())-1].TrackingID
		s.later(30 * time.Second)
		s.srv.Fail("GET /evatra/invoice/document-status/"+tracking, 1)
		if step := s.advance(t, id, op); step.Wait != time.Minute {
			t.Errorf("a read failing: got %+v, want a wait of a minute", step)
		}
		s.later(time.Minute)
		s.advance(t, id, op)
		check(t, "processing", s.transmission(t, id), "submitted PROCESSING 1 0 -")
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET next_check_at = now() - interval '1 day', version = version + 1
			WHERE outgoing_einvoice_id = $1`, id)
		s.later(time.Hour)
		s.advance(t, id, op)
		s.later(time.Hour)
		s.advance(t, id, op)
		check(t, "FAILED", s.transmission(t, id), "failed FAILED 1 0 einvoicing.provider_failed")
	})
	t.Run("unconfirmed", func(t *testing.T) {
		id := next(t)
		got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
		s.srv.Evatra.Script(got["customer_invoice_number"].(string), []string{"PROCESSING"})
		op := s.send(t, id, s.connector)
		s.advance(t, id, op)
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET polls = 499, version = version + 1 WHERE outgoing_einvoice_id = $1`, id)
		s.later(time.Hour)
		s.advance(t, id, op)
		check(t, "unconfirmed", s.transmission(t, id), "failed PROCESSING 1 0 einvoicing.status_unconfirmed")
	})
	t.Run("the stored XML changed", func(t *testing.T) {
		id := next(t)
		got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
		path := filepath.Join(s.store.Root, "tenants", s.f.Tenant.ID.String(), "documents", got["sha256"].(string))
		if err := os.WriteFile(path, []byte("<Invoice>changed</Invoice>"), 0o600); err != nil {
			t.Fatal(err)
		}
		op := s.send(t, id, s.connector)
		s.advance(t, id, op)
		check(t, "not sent", s.transmission(t, id), "failed <nil> 0 0 einvoicing.document_changed")
	})
}

// TestSendLapsedClaim: a claim whose outcome never came (the worker
// stopped during the call) lapses; the next step claims it again and
// looks the document up before sending, and a lapse on the fifth call
// fails it as unavailable. A worker stopping during the call records
// nothing. A row another step moved since the claim is not recorded over.
// A send whose e-invoice has no submission is not this send's.
func TestSendLapsedClaim(t *testing.T) {
	s := senderSetup(t)
	id := s.invoice["id"].(string)
	op := s.send(t, id, s.connector)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	stopping := func(c context.Context, fn func(ctx context.Context) error) error {
		calls++
		err := s.within()(c, fn)
		if calls == 4 { // the read, the connector, the XML, then the claim's commit
			cancel()
		}
		return err
	}
	if _, err := s.e.AdvanceSubmission(ctx, uuid.MustParse(id), op, stopping); !errors.Is(err, context.Canceled) {
		t.Errorf("the worker stopping: got %v, want context.Canceled", err)
	}
	check(t, "claimed, no outcome", s.transmission(t, id), "sending <nil> 1 0 -")
	s.later(einvoicing.CallTimeout * 5)
	s.advance(t, id, op)
	check(t, "claimed again and sent", words(len(s.srv.Evatra.Sends()), s.transmission(t, id)), "1 submitted PENDING 2 0 -")

	other := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
	op2 := s.send(t, other, s.connector)
	pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET status = 'sending', attempts = 5, next_check_at = now() - interval '1 hour',
		version = version + 1 WHERE outgoing_einvoice_id = $1`, other)
	if step := s.advance(t, other, op2); !step.Done {
		t.Errorf("a lapse on the last call: got %+v, want done", step)
	}
	check(t, "a lapse on the last call", s.transmission(t, other), "failed <nil> 5 0 einvoicing.provider_unavailable")

	third := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
	op3 := s.send(t, third, s.connector)
	calls = 0
	moving := func(c context.Context, fn func(ctx context.Context) error) error {
		calls++
		if calls == 5 { // before the answer is recorded, another step moved the row
			pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET version = version + 1 WHERE outgoing_einvoice_id = $1`, third)
		}
		return s.within()(c, fn)
	}
	if _, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(third), op3, moving); err != nil {
		t.Fatal(err)
	}
	check(t, "not recorded over", s.transmission(t, third), "sending <nil> 1 0 -")

	if step := s.advance(t, uuid.NewString(), op3); !step.Done {
		t.Errorf("no submission of the e-invoice: got %+v, want done", step)
	}
	check(t, "its operation", operation(t, op3), "failed /v1/outgoing-e-invoices/"+third+" einvoicing.transmission_superseded")
}

// TestSendDatabaseFailures: every statement of the send request and of a
// step failing is an error, and the next try goes on from what committed.
func TestSendDatabaseFailures(t *testing.T) {
	s := senderSetup(t)
	id := uuid.MustParse(s.invoice["id"].(string))
	send := func(ctx context.Context) error {
		_, err := s.e.SendOutgoingEInvoice(ctx, &gen.OutgoingEInvoiceSend{ConnectorID: s.connector, BusinessType: "B2B"},
			gen.SendOutgoingEInvoiceParams{ID: id, IfMatch: "*"})
		return err
	}
	deadlines := func(ctx context.Context) error {
		_, err := s.e.ListEInvoiceTransmissionDeadlines(ctx, gen.ListEInvoiceTransmissionDeadlinesParams{LegalEntityID: s.le, KeyDate: s.clock()})
		return err
	}
	for name, call := range map[string]func(ctx context.Context) error{"send": send, "deadlines": deadlines} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, name, call)
		})
	}
	op := s.send(t, id.String(), s.connector)
	advance := func(in einvoicing.Within) error {
		_, err := s.e.AdvanceSubmission(context.Background(), id, op, in)
		return err
	}
	s.sendEveryFailure(t, advance) // the send: a failure after the call leaves its claim to lapse
	s.later(time.Hour)
	s.advance(t, id.String(), op) // claimed again, looked up, found
	s.later(time.Hour)
	s.sendEveryFailure(t, advance) // the status read
	check(t, "completed", s.transmission(t, id.String()), "completed COMPLETED 2 0 -")
	s.sendEveryFailure(t, advance) // the operation ended again
	t.Run("outside a unit of work", func(t *testing.T) {
		outside := func(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }
		if err := advance(outside); err == nil {
			t.Error("got nil error, want an error")
		}
	})
}

// sendEveryFailure runs fn with the nth statement of its transactions
// failing, n from 1, until it passes.
func (s *sender) sendEveryFailure(t *testing.T, fn func(in einvoicing.Within) error) {
	t.Helper()
	for n := 1; n < 200; n++ {
		count := 0
		in := func(ctx context.Context, fn func(ctx context.Context) error) error {
			return uow.Job{Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
				tx, err := s.pool.Begin(ctx, scope)
				if err != nil {
					return nil, err
				}
				return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
			}, Scope: database.Scope{Role: "erp_app", TenantID: s.f.Tenant.ID}, Name: "test send"}.As(ctx,
				func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
					return einvoicing.Sender(s.f.Tenant.ID), true, nil
				}, fn)
		}
		err := fn(in)
		if err == nil {
			return
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d failed, but the error was %v", n, err)
		}
	}
	t.Fatal("still failing after 200 statements")
}

// TestEvatraSendAnswers: the adapter reads Evatra's answers to a send, a
// look-up and a status read: a tracking id is required, a status of
// another form is read as PENDING on a send and refused on a read; a 4xx
// other than 401 and 429 refuses the document (einvoicing.send_refused),
// 201 and 202 are taken, anything else fails the call to be tried again;
// a look-up takes a submission of the filename that Evatra did not fail,
// and none other.
func TestEvatraSendAnswers(t *testing.T) {
	ctx := context.Background()
	d := einvoicing.Outgoing{Filename: "INV-1", BusinessType: "B2B", Content: []byte("<Invoice/>")}
	look := d
	look.LookUp, look.Failed = true, []string{"T-OLD"}
	sendIs := func(o einvoicing.Outgoing, want string) func(p einvoicing.Provider) error {
		return func(p einvoicing.Provider) error {
			got, err := p.Send(ctx, o)
			if err == nil && words(got.TrackingID, got.Status, got.Found) != want {
				return fmt.Errorf("sent %+v, want %s", got, want)
			}
			return err
		}
	}
	statusIs := func(want string) func(p einvoicing.Provider) error {
		return func(p einvoicing.Provider) error {
			got, err := p.Status(ctx, "T-1")
			if err == nil && got.Status != want {
				return fmt.Errorf("status %s, want %s", got.Status, want)
			}
			return err
		}
	}
	search := func(body string) map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{"POST /invoice/document-process/search": answer(200, body),
			"POST /invoice/send-document-async": answer(200, `{"trackingId": "T-NEW", "status": "PENDING"}`)}
	}
	for name, c := range map[string]struct {
		routes map[string]http.HandlerFunc
		call   func(p einvoicing.Provider) error
		code   string // "": an error without a problem code; "-": none
	}{
		"sent":                  {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(200, `{"trackingId": "T-1", "status": "PENDING"}`)}, sendIs(d, "T-1 PENDING false"), "-"},
		"accepted, no status":   {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(202, `{"trackingId": "T-1"}`)}, sendIs(d, "T-1 PENDING false"), "-"},
		"created":               {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(201, `{"trackingId": "T-1", "status": "QUEUED"}`)}, sendIs(d, "T-1 QUEUED false"), "-"},
		"no tracking id":        {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(200, `{}`)}, sendIs(d, ""), ""},
		"refused":               {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(400, `{}`)}, sendIs(d, ""), "einvoicing.send_refused"},
		"too many requests":     {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(429, `{}`)}, sendIs(d, ""), ""},
		"failing":               {map[string]http.HandlerFunc{"POST /invoice/send-document-async": answer(500, `{}`)}, sendIs(d, ""), ""},
		"cut off":               {map[string]http.HandlerFunc{"POST /invoice/send-document-async": cutOff}, sendIs(d, ""), ""},
		"found":                 {search(`{"content": [{"trackingId": "T-OLD", "status": "PENDING", "filename": "INV-1"}, {"trackingId": "T-2", "status": "PROCESSING", "filename": "INV-1"}]}`), sendIs(look, "T-2 PROCESSING true"), "-"},
		"none found but failed": {search(`{"content": [{"trackingId": "T-3", "status": "SENDING_FAILED", "filename": "INV-1"}, {"trackingId": "T-4", "status": "COMPLETED", "filename": "INV-2"}]}`), sendIs(look, "T-NEW PENDING false"), "-"},
		"a look-up failing":     {map[string]http.HandlerFunc{"POST /invoice/document-process/search": answer(503, ``)}, sendIs(look, ""), ""},
		"a look-up not JSON":    {search(`not json`), sendIs(look, ""), ""},
		"a look-up cut off":     {map[string]http.HandlerFunc{"POST /invoice/document-process/search": cutOff}, sendIs(look, ""), ""},
		"a status":              {map[string]http.HandlerFunc{"GET /invoice/document-status/T-1": answer(200, `{"status": "COMPLETED"}`)}, statusIs("COMPLETED"), "-"},
		"a status failing":      {map[string]http.HandlerFunc{"GET /invoice/document-status/T-1": answer(404, ``)}, statusIs(""), ""},
		"a status of no form":   {map[string]http.HandlerFunc{"GET /invoice/document-status/T-1": answer(200, `{"status": "done!"}`)}, statusIs(""), ""},
		"a status cut off":      {map[string]http.HandlerFunc{"GET /invoice/document-status/T-1": cutOff}, statusIs(""), ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.call(fakeEvatra(t, c.routes))
			switch {
			case c.code == "-" && err != nil:
				t.Errorf("got %v, want done", err)
			case c.code == "" && (err == nil || !strings.HasPrefix(problem.From(err).Code, "platform.")):
				t.Errorf("got %v, want the call failed without a problem of its own", err)
			case c.code != "" && c.code != "-" && problem.From(err).Code != c.code:
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
	t.Run("the stand-in's protocol", func(t *testing.T) {
		p := &einvoicing.StandIn{}
		p.SendsNothing()
		_, sendErr := p.Send(ctx, d)
		_, statusErr := p.Status(ctx, "T-1")
		check(t, "refused", words(problem.From(sendErr).Code, problem.From(statusErr).Code), "einvoicing.adapter_cannot_send einvoicing.adapter_cannot_send")
	})
}

// failingCustomers is Sales whose customers cannot be read.
type failingCustomers struct{ einvoicing.Sales }

func (failingCustomers) Customers(context.Context, []uuid.UUID) (map[uuid.UUID]einvoicing.CustomerName, error) {
	return nil, errors.New("partner down")
}

// TestSendEdges: a step after a failure ends its Operation again; a
// connector whose adapter changed to one that sends nothing fails a
// submitted one at its status read; a row moved between the step's
// preparation and its claim is taken again with nothing called; a server
// without a store, or whose store fails, changes nothing; a send without a
// store is refused, as is one whose connector's secret is not Evatra's;
// the worklist with a page token of another listing is refused, and its
// customers failing fail it.
func TestSendEdges(t *testing.T) {
	s := senderSetup(t)
	ctx := context.Background()
	next := func(t *testing.T) string {
		t.Helper()
		return s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
	}
	t.Run("ended again", func(t *testing.T) {
		wrong := s.sendConnector(t, "Wrong", "evatra", "NOT-OURS", map[string]string{"5000": sellerParticipant})
		id := next(t)
		op := s.send(t, id, wrong)
		s.advance(t, id, op)
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("after the failure: got %+v, want done", step)
		}
	})
	t.Run("an adapter that sends nothing", func(t *testing.T) {
		conn := s.sendConnector(t, "Switched", "evatra", einvoicetest.EvatraCompany, map[string]string{"5000": sellerParticipant})
		id := next(t)
		op := s.send(t, id, conn)
		s.advance(t, id, op)
		pgtest.Exec(t, `UPDATE integration.connector SET settings = jsonb_build_object('adapter', 'standin', 'base_url', $2::text, 'format', 'pint_ae',
			'participants', settings->'participants'), version = version + 1 WHERE id = $1`, conn, s.srv.URL)
		s.later(time.Minute)
		s.advance(t, id, op)
		check(t, "failed at its read", s.transmission(t, id), "failed PENDING 1 0 einvoicing.adapter_cannot_send")
	})
	t.Run("moved before the claim", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		s.advance(t, id, op)
		s.later(time.Minute)
		calls := 0
		moving := func(c context.Context, fn func(ctx context.Context) error) error {
			calls++
			if calls == 3 { // the read and the connector, then the claim
				pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET version = version + 1 WHERE outgoing_einvoice_id = $1`, id)
			}
			return s.within()(c, fn)
		}
		step, err := s.e.AdvanceSubmission(ctx, uuid.MustParse(id), op, moving)
		if err != nil || step.Done || step.Wait != 0 {
			t.Errorf("got %+v and %v, want it taken again at once", step, err)
		}
		check(t, "nothing read", s.transmission(t, id), "submitted PENDING 1 0 -")
	})
	t.Run("the store", func(t *testing.T) {
		id := next(t)
		op := s.send(t, id, s.connector)
		e := app.EInvoicing(s.cfg)
		for name, store := range map[string]blobstore.Store{"none": nil, "failing": failingGets{s.store}} {
			e.Objects = store
			if _, err := e.AdvanceSubmission(ctx, uuid.MustParse(id), op, s.within()); err == nil {
				t.Errorf("%s: got nil error, want an error", name)
			}
		}
		check(t, "unchanged", s.transmission(t, id), "queued <nil> 0 0 -")
		e.Objects = nil
		err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := e.SendOutgoingEInvoice(ctx, &gen.OutgoingEInvoiceSend{ConnectorID: s.connector, BusinessType: "B2B"},
				gen.SendOutgoingEInvoiceParams{ID: uuid.MustParse(id), IfMatch: "*"})
			return err
		})
		check(t, "a send without a store", problem.From(err).Status, 503)
	})
	t.Run("a secret not Evatra's", func(t *testing.T) {
		conn := uuid.Must(uuid.NewV7())
		pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
			VALUES ($1, $2, 'einvoice_provider', 'Bad secret', jsonb_build_object('adapter', 'evatra', 'environment', 'stage', 'company_id', 'C',
			'format', 'pint_ae', 'participants', jsonb_build_object('5000', $3::text)), $4, 'active', $5, $5)`,
			s.f.Tenant.ID, conn, sellerParticipant, vaulttest.TenantRef(s.f.Tenant.ID, "provider"), s.f.Tenant.Human.ID)
		s.f.Problem(422, "integration.secret_unavailable", "POST", "/v1/outgoing-e-invoices/"+next(t)+":send",
			fmt.Sprintf(`{"connector_id": %q, "business_type": "B2B"}`, conn), `"1"`)
	})
	t.Run("the worklist", func(t *testing.T) {
		s.f.Problem(400, "request.invalid_page_token", "GET", "/v1/e-invoice-transmission-deadlines?legal_entity_id="+s.le.String()+
			"&key_date=2026-03-31&page_token=not-a-token", "", "")
		e := app.EInvoicing(s.cfg)
		e.Sales = failingCustomers{e.Sales}
		err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := e.ListEInvoiceTransmissionDeadlines(ctx, gen.ListEInvoiceTransmissionDeadlinesParams{LegalEntityID: s.le,
				KeyDate: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "partner down") {
			t.Errorf("got %v, want the partner's error", err)
		}
	})
	t.Run("failing statements on the way to a failure", func(t *testing.T) {
		conn := s.sendConnector(t, "Off later", "evatra", einvoicetest.EvatraCompany, map[string]string{"5000": sellerParticipant})
		id := next(t)
		op := s.send(t, id, conn)
		pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, conn)
		s.sendEveryFailure(t, func(in einvoicing.Within) error {
			_, err := s.e.AdvanceSubmission(ctx, uuid.MustParse(id), op, in)
			return err
		})
		check(t, "failed", s.transmission(t, id), "failed <nil> 0 0 integration.connector_disabled")
	})
}
