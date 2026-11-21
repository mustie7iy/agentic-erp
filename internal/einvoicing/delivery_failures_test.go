package einvoicing_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// reported is a sender whose tax invoice is completed at Evatra with the
// delivery script given, its first delivery read made: the e-invoice's
// id and its send's Operation.
func (s *sender) reported(t *testing.T, script einvoicetest.DeliveryScript) (string, uuid.UUID) {
	t.Helper()
	id := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	s.srv.Evatra.ScriptDelivery(got["customer_invoice_number"].(string), script)
	op := s.completed(t, id)
	s.later(30 * time.Second)
	s.advance(t, id, op)
	return id, op
}

// failedTaxData is a script whose tax data document fails, a retry and a
// withdrawal of it taken.
var failedTaxData = einvoicetest.DeliveryScript{Responses: []einvoicetest.Response{{Status: "ACCEPTANCE"}},
	TaxData: map[string][]string{"report": {"TDD_FAILED"}, "retry": {"TDD_APPROVED"}, "withdraw": {"WITHDRAW_SEND"}}}

// TestTaxDataRefusals: a request of an e-invoice not transmitted, of a tax
// data document in another status, at another ETag, of an e-invoice not
// found or through a connector disabled since is refused, nothing
// written; every statement of the request failing is an error.
func TestTaxDataRefusals(t *testing.T) {
	s := senderSetup(t)
	unsent := s.invoice["id"].(string)
	s.f.Problem(409, "einvoicing.not_transmitted", "POST", "/v1/outgoing-e-invoices/"+unsent+":retryTaxData", "", s.tag(t, unsent))
	op := s.send(t, unsent, s.connector)
	s.f.Problem(409, "einvoicing.not_transmitted", "POST", "/v1/outgoing-e-invoices/"+unsent+":withdrawTaxData", "", s.tag(t, unsent))
	s.advance(t, unsent, op)
	s.later(30 * time.Second)
	s.advance(t, unsent, op)
	s.f.Problem(409, "einvoicing.tax_data_not_withdrawable", "POST", "/v1/outgoing-e-invoices/"+unsent+":withdrawTaxData", "", s.tag(t, unsent))
	s.f.Problem(409, "einvoicing.tax_data_not_failed", "POST", "/v1/outgoing-e-invoices/"+unsent+":retryTaxData", "", s.tag(t, unsent))
	s.f.Problem(412, "request.version_mismatch", "POST", "/v1/outgoing-e-invoices/"+unsent+":retryTaxData", "", `"1"`)
	s.f.Problem(404, "request.not_found", "POST", "/v1/outgoing-e-invoices/"+uuid.NewString()+":retryTaxData", "", `"1"`)

	id, _ := s.reported(t, failedTaxData)
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, s.connector)
	s.f.Problem(409, "integration.connector_disabled", "POST", "/v1/outgoing-e-invoices/"+id+":retryTaxData", "", s.tag(t, id))
	pgtest.Exec(t, `UPDATE integration.connector SET status = 'active', version = version + 1 WHERE id = $1`, s.connector)
	check(t, "nothing written", s.delivery(t, id), "ACCEPTANCE accepted TDD_FAILED <nil>")

	retry := func(ctx context.Context) error {
		_, err := s.e.RetryOutgoingEInvoiceTaxData(ctx, gen.RetryOutgoingEInvoiceTaxDataParams{ID: uuid.MustParse(id), IfMatch: "*"})
		return err
	}
	if err := retry(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, "retry", retry)
}

// TestTaxDataRequestFailures: a request the provider refuses fails its
// Operation (einvoicing.tax_data_refused); one whose calls fail is tried
// again with backoff and fails after the fifth (einvoicing.
// provider_unavailable), as one whose last claim lapsed does; one whose
// connector was disabled since fails with that; one cancelled is dropped
// once no call is under way, before its call or after one failed; in each
// case the request is cleared and the delivery read again at once.
func TestTaxDataRequestFailures(t *testing.T) {
	s := senderSetup(t)
	t.Run("refused", func(t *testing.T) {
		id, _ := s.reported(t, einvoicetest.DeliveryScript{TaxData: map[string][]string{"report": {"TDD_REJECTED"}}})
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET tax_data_status = 'TDD_ACKNOWLEDGED', version = version + 1 WHERE outgoing_einvoice_id = $1`, id)
		op := s.request(t, id, "withdraw")
		if step := s.advance(t, id, op); step.Done || step.Wait != 0 {
			t.Errorf("got %+v, want the delivery read at once", step)
		}
		check(t, "failed, cleared", words(operation(t, op), s.delivery(t, id)),
			"failed /v1/outgoing-e-invoices/"+id+" einvoicing.tax_data_refused <nil> pending TDD_ACKNOWLEDGED <nil>")
	})
	t.Run("its calls failing", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		s.srv.Fail("POST /evatra/peppol/tdd/retry-failed", 5)
		for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
			if step := s.advance(t, id, op); step.Wait != wait {
				t.Errorf("call %d: got %+v, want a wait of %v", i+1, step, wait)
			}
			s.later(wait)
		}
		s.advance(t, id, op)
		check(t, "failed after five calls", words(operation(t, op), s.delivery(t, id)),
			"failed /v1/outgoing-e-invoices/"+id+" einvoicing.provider_unavailable ACCEPTANCE accepted TDD_FAILED <nil>")
	})
	t.Run("its last claim lapsed", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET tax_data_attempts = 5, version = version + 1 WHERE outgoing_einvoice_id = $1`, id)
		s.advance(t, id, op)
		check(t, "failed", operation(t, op), "failed /v1/outgoing-e-invoices/"+id+" einvoicing.provider_unavailable")
		check(t, "not asked", strings.Contains(fmt.Sprint(s.srv.Evatra.TaxDataRequests()), tracking(t, s, id)), false)
	})
	t.Run("its connector disabled since", func(t *testing.T) {
		id, op := s.reported(t, failedTaxData)
		request := s.request(t, id, "retry")
		pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, s.connector)
		defer pgtest.Exec(t, `UPDATE integration.connector SET status = 'active', version = version + 1 WHERE id = $1`, s.connector)
		s.advance(t, id, request)
		check(t, "failed", operation(t, request), "failed /v1/outgoing-e-invoices/"+id+" integration.connector_disabled")
		if step := s.advance(t, id, request); !step.Done {
			t.Errorf("a read refused, both statuses final: got %+v, want done", step)
		}
		if step := s.advance(t, id, op); !step.Done {
			t.Errorf("the send's step: got %+v, want done", step)
		}
	})
	t.Run("cancelled before its call", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		s.cancelSend(t, op)
		if step := s.advance(t, id, op); step.Done || step.Wait != 0 {
			t.Errorf("got %+v, want the delivery read at once", step)
		}
		check(t, "dropped", words(operation(t, op), s.delivery(t, id)), "cancelled /v1/outgoing-e-invoices/"+id+" - ACCEPTANCE accepted TDD_FAILED <nil>")
	})
	t.Run("cancelled while claimed", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET tax_data_attempts = 1, follow_check_at = $2, version = version + 1
			WHERE outgoing_einvoice_id = $1`, id, s.clock().Add(time.Minute))
		s.cancelSend(t, op)
		if step := s.advance(t, id, op); step.Wait != time.Minute {
			t.Errorf("got %+v, want a wait for the claim", step)
		}
		check(t, "kept meanwhile", s.delivery(t, id), "ACCEPTANCE accepted TDD_FAILED retry")
	})
	t.Run("cancelled during a call that failed", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		s.srv.Fail("POST /evatra/peppol/tdd/retry-failed", 1)
		calls := 0
		cancelling := func(ctx context.Context, fn func(ctx context.Context) error) error {
			calls++
			err := s.within()(ctx, fn)
			if calls == 3 { // the read, the connector, then the claim's commit: cancelled while the call is made
				s.cancelSend(t, op)
			}
			return err
		}
		if _, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, cancelling); err != nil {
			t.Fatal(err)
		}
		check(t, "dropped", words(operation(t, op), s.delivery(t, id)), "cancelled /v1/outgoing-e-invoices/"+id+" - ACCEPTANCE accepted TDD_FAILED <nil>")
	})
	t.Run("failing statements", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		advance := func(in einvoicing.Within) error {
			_, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, in)
			return err
		}
		s.sendEveryFailure(t, advance) // the request: a failure after the call leaves its claim to lapse
		s.later(5 * einvoicing.CallTimeout)
		s.advance(t, id, op) // claimed again: found made
		check(t, "made", operation(t, op), "succeeded /v1/outgoing-e-invoices/"+id+" -")
		s.sendEveryFailure(t, advance) // the delivery read
		check(t, "approved", s.delivery(t, id), "ACCEPTANCE accepted TDD_APPROVED <nil>")
	})
}

// TestDeliveryReadFailures: a delivery read that fails records nothing
// but the count and is read again after the backoff; the 500th read is
// the last, final or not.
func TestDeliveryReadFailures(t *testing.T) {
	s := senderSetup(t)
	id, send := s.reported(t, einvoicetest.DeliveryScript{Responses: []einvoicetest.Response{{Status: "ACKNOWLEDGING"}}})
	check(t, "not final", s.delivery(t, id), "ACKNOWLEDGING pending <nil> <nil>")
	s.later(time.Minute)
	s.srv.Fail("GET /evatra/peppol/invoice-mls-list", 1)
	if step := s.advance(t, id, send); step.Wait != 2*time.Minute {
		t.Errorf("the read failing: got %+v, want a read again in two minutes", step)
	}
	check(t, "kept", s.delivery(t, id), "ACKNOWLEDGING pending <nil> <nil>")
	pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET follow_polls = 499, tax_data_status = 'TDD_FAILED', tax_data_at = now(), tax_data_awaited = 'TDD_FAILED',
		version = version + 1
		WHERE outgoing_einvoice_id = $1`, id)
	s.later(2 * time.Minute)
	recorded := func() (audits, events int) {
		t.Helper()
		pgtest.Scan(t, `SELECT (SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.outgoing.transmission' AND object_id = $2),
			(SELECT count(*) FROM einvoicing.outbox WHERE tenant_id = $1 AND aggregate_id = $2)`, []any{s.f.Tenant.ID, id}, &audits, &events)
		return audits, events
	}
	audits, events := recorded()
	if step := s.advance(t, id, send); !step.Done {
		t.Errorf("the 500th read: got %+v, want done", step)
	}
	a, e := recorded()
	check(t, "no longer awaited: audit records more", a-audits, 1)
	check(t, "no longer awaited: events more", e-events, 1)
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	tr := got["transmission"].(map[string]any)
	check(t, "no longer followed nor awaited", words(tr["follow_check_at"], tr["tax_data_awaited"]), "<nil> <nil>")
}

// TestEvatraDeliveryAnswers: the adapter reads Evatra's message level
// responses (the last final one, a description trimmed) and the tax data
// document (none while 404), and makes a request (a 4xx refuses it,
// einvoicing.tax_data_refused); answers of another form, or failing, fail
// the call to be tried again.
func TestEvatraDeliveryAnswers(t *testing.T) {
	ctx := context.Background()
	deliveryIs := func(want string) func(p einvoicing.Provider) error {
		return func(p einvoicing.Provider) error {
			got, err := p.Delivery(ctx, "T-1")
			if err == nil && words(got.Response, got.Detail, got.TaxData) != want {
				return fmt.Errorf("read %+v, want %s", got, want)
			}
			return err
		}
	}
	request := func(r string) func(p einvoicing.Provider) error {
		return func(p einvoicing.Provider) error { return p.TaxData(ctx, "T-1", r) }
	}
	read := func(mls, tdd http.HandlerFunc) map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{"GET /peppol/invoice-mls-list": mls, "GET /peppol/get-tdd": tdd}
	}
	ok := answer(200, `{"content": [{"status": "REJECTION", "description": " rule "}, {"status": "ACKNOWLEDGING"}]}`)
	for name, c := range map[string]struct {
		routes map[string]http.HandlerFunc
		call   func(p einvoicing.Provider) error
		code   string // "": an error without a problem code; "-": none
	}{
		"read":                    {read(ok, answer(200, `{"status": "TDD_SEND"}`)), deliveryIs("REJECTION rule TDD_SEND"), "-"},
		"no tax data document":    {read(answer(200, `{"content": []}`), answer(404, ``)), deliveryIs(words("", "", "")), "-"},
		"responses failing":       {read(answer(503, ``), nil), deliveryIs(""), ""},
		"responses cut off":       {read(cutOff, nil), deliveryIs(""), ""},
		"responses not JSON":      {read(answer(200, `not json`), nil), deliveryIs(""), ""},
		"a response of no form":   {read(answer(200, `{"content": [{"status": "ok"}]}`), nil), deliveryIs(""), ""},
		"the document failing":    {read(ok, answer(500, ``)), deliveryIs(""), ""},
		"the document cut off":    {read(ok, cutOff), deliveryIs(""), ""},
		"the document of no form": {read(ok, answer(200, `{"status": 1}`)), deliveryIs(""), ""},
		"a retry":                 {map[string]http.HandlerFunc{"POST /peppol/tdd/retry-failed": answer(200, `{}`)}, request("retry"), "-"},
		"a withdrawal refused":    {map[string]http.HandlerFunc{"POST /peppol/tdd/withdraw": answer(400, `{}`)}, request("withdraw"), "einvoicing.tax_data_refused"},
		"a retry failing":         {map[string]http.HandlerFunc{"POST /peppol/tdd/retry-failed": answer(500, `{}`)}, request("retry"), ""},
		"a retry cut off":         {map[string]http.HandlerFunc{"POST /peppol/tdd/retry-failed": cutOff}, request("retry"), ""},
		"another request":         {map[string]http.HandlerFunc{}, request("cancel"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{}
			for k, v := range c.routes {
				if v != nil {
					routes[k] = v
				}
			}
			err := c.call(fakeEvatra(t, routes))
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
		_, readErr := p.Delivery(ctx, "T-1")
		check(t, "refused", words(problem.From(readErr).Code, problem.From(p.TaxData(ctx, "T-1", "retry")).Code),
			"einvoicing.adapter_cannot_send einvoicing.adapter_cannot_send")
	})
}

// TestTaxDataRecordFailures: every statement of a request's record
// failing is an error, the claim left to lapse and the next step found
// the request made (or, cancelled, dropped); a step finding its
// submission moved since it was prepared takes it again at once; a
// request made while its Operation was cancelled is recorded made, the
// Operation left cancelled.
func TestTaxDataRecordFailures(t *testing.T) {
	s := senderSetup(t)
	// recording runs a step whose fourth transaction (the record, after the
	// read, the connector and the claim) fails at its nth statement, n from
	// 1, the claim reset before each, until the step passes.
	recording := func(t *testing.T, id string, op uuid.UUID, before func()) {
		t.Helper()
		for n := 1; n < 100; n++ {
			pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET tax_data_attempts = 0, follow_check_at = $2, version = version + 1
				WHERE outgoing_einvoice_id = $1 AND tax_data_request IS NOT NULL`, id, s.clock())
			before()
			calls := 0
			failing := func(ctx context.Context, fn func(ctx context.Context) error) error {
				calls++
				if calls != 4 {
					return s.within()(ctx, fn)
				}
				count := 0
				return uow.Job{Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
					tx, err := s.pool.Begin(ctx, scope)
					if err != nil {
						return nil, err
					}
					return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
				}, Scope: database.Scope{Role: "erp_app", TenantID: s.f.Tenant.ID}, Name: "test record"}.As(ctx,
					func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
						return einvoicing.Sender(s.f.Tenant.ID), true, nil
					}, fn)
			}
			_, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, failing)
			if err == nil {
				return
			}
			if !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("statement %d failed, but the error was %v", n, err)
			}
		}
		t.Fatal("still failing after 100 statements")
	}
	t.Run("a call failing", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		recording(t, id, op, func() { s.srv.Fail("POST /evatra/peppol/tdd/retry-failed", 1) })
		check(t, "to be asked again", s.delivery(t, id), "ACCEPTANCE accepted TDD_FAILED retry")
	})
	t.Run("a call failing, cancelled", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		s.cancelSend(t, op)
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET follow_check_at = $2, version = version + 1 WHERE outgoing_einvoice_id = $1`, id,
			s.clock().Add(time.Minute))
		s.sendEveryFailure(t, func(in einvoicing.Within) error {
			pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET follow_check_at = $2, version = version + 1 WHERE outgoing_einvoice_id = $1
				AND tax_data_request IS NOT NULL`, id, s.clock())
			_, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, in)
			return err
		})
		check(t, "dropped", s.delivery(t, id), "ACCEPTANCE accepted TDD_FAILED <nil>")
	})
	t.Run("refused", func(t *testing.T) {
		id, _ := s.reported(t, einvoicetest.DeliveryScript{TaxData: map[string][]string{"report": {"TDD_REJECTED"}}})
		pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET tax_data_status = 'TDD_APPROVED', version = version + 1 WHERE outgoing_einvoice_id = $1`, id)
		op := s.request(t, id, "withdraw")
		recording(t, id, op, func() {})
		check(t, "failed", operation(t, op), "failed /v1/outgoing-e-invoices/"+id+" einvoicing.tax_data_refused")
	})
	t.Run("made", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		recording(t, id, op, func() {})
		check(t, "made once", words(operation(t, op), len(s.srv.Evatra.TaxDataRequests())), "succeeded /v1/outgoing-e-invoices/"+id+" - 1")
	})
	t.Run("moved since it was prepared", func(t *testing.T) {
		id, op := s.reported(t, einvoicetest.DeliveryScript{Responses: []einvoicetest.Response{{Status: "ACKNOWLEDGING"}}})
		s.later(time.Minute)
		calls := 0
		moving := func(ctx context.Context, fn func(ctx context.Context) error) error {
			calls++
			if calls == 3 { // before the claim: another step moved it
				pgtest.Exec(t, `UPDATE einvoicing.outgoing_submission SET version = version + 1 WHERE outgoing_einvoice_id = $1`, id)
			}
			return s.within()(ctx, fn)
		}
		if step, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, moving); err != nil || step.Done || step.Wait != 0 {
			t.Errorf("got %+v and %v, want it taken again at once", step, err)
		}
	})
	t.Run("made while cancelled", func(t *testing.T) {
		id, _ := s.reported(t, failedTaxData)
		op := s.request(t, id, "retry")
		calls := 0
		cancelling := func(ctx context.Context, fn func(ctx context.Context) error) error {
			calls++
			err := s.within()(ctx, fn)
			if calls == 3 { // the claim's commit: cancelled while the call is made
				s.cancelSend(t, op)
			}
			return err
		}
		if _, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), op, cancelling); err != nil {
			t.Fatal(err)
		}
		check(t, "made, the Operation cancelled", words(operation(t, op), s.delivery(t, id)),
			"cancelled /v1/outgoing-e-invoices/"+id+" - ACCEPTANCE accepted TDD_FAILED <nil>")
	})
}
