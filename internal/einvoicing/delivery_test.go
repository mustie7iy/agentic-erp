package einvoicing_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// delivery is the e-invoice's transmission as E4 shows it: the delivery
// status, its outcome, the tax data document's status and the request
// not yet made.
func (s *sender) delivery(t *testing.T, id string) string {
	t.Helper()
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	tr := got["transmission"].(map[string]any)
	return words(tr["delivery_status"], tr["delivery_outcome"], tr["tax_data_status"], tr["tax_data_request"])
}

// completed sends the e-invoice and carries its send to COMPLETED,
// answering the send's Operation; its delivery read is due 30 seconds on.
func (s *sender) completed(t *testing.T, id string) uuid.UUID {
	t.Helper()
	op := s.send(t, id, s.connector)
	s.advance(t, id, op)
	s.later(30 * time.Second)
	s.advance(t, id, op)
	check(t, "completed", s.transmission(t, id), "completed COMPLETED 1 0 -")
	return op
}

// request asks for the e-invoice's tax data document to be sent again or
// withdrawn, answering the request's Operation, carried out by the test.
func (s *sender) request(t *testing.T, id, action string) uuid.UUID {
	t.Helper()
	op, _ := s.f.Must(202, "POST", "/v1/outgoing-e-invoices/"+id+":"+action+"TaxData", "", s.tag(t, id))
	check(t, "its kind", op["kind"], einvoicing.KindTaxData)
	opID := uuid.MustParse(op["id"].(string))
	pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1 WHERE id = $1`, opID)
	return opID
}

// TestDeliveryStatus (As built E4): once completed, the send reads what
// Peppol and the authority report, from 30 seconds on doubling: the final
// message level response listed (not the ACKNOWLEDGING before it) with
// its detail, and the tax data document's status, until both are final;
// a fatal rule puts the e-invoice on the worklist of rejected ones, which
// a credit note naming it takes it off once transmitted; each change is
// audited and published, and a step run again reads nothing twice.
func TestDeliveryStatus(t *testing.T) {
	s := senderSetup(t)
	id, number := s.invoice["id"].(string), s.invoice["customer_invoice_number"].(string)
	s.srv.Evatra.ScriptDelivery(number, einvoicetest.DeliveryScript{
		Responses: []einvoicetest.Response{{Status: "ACKNOWLEDGING"}, {Status: "BUSINESS_RULE_FATAL", Description: " IBR-128-AE: the emirate is missing "}},
		TaxData:   map[string][]string{"report": {"TDD_SEND", "TDD_APPROVED"}}})
	op := s.completed(t, id)
	check(t, "nothing reported yet", s.delivery(t, id), "<nil> pending <nil> <nil>")
	if step := s.advance(t, id, op); step.Done || step.Wait != 30*time.Second {
		t.Errorf("before its read is due: got %+v, want a wait of 30s", step)
	}
	s.later(30 * time.Second)
	if step := s.advance(t, id, op); step.Done || step.Wait != time.Minute {
		t.Errorf("the tax data document not final: got %+v, want a read again in a minute", step)
	}
	check(t, "the fatal rule, the tax data document sent", s.delivery(t, id), "BUSINESS_RULE_FATAL rejected TDD_SEND <nil>")
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	tr := got["transmission"].(map[string]any)
	check(t, "its detail, when and the next read", words(tr["delivery_detail"], tr["delivered_at"] != nil, tr["tax_data_at"] != nil, tr["follow_check_at"] != nil),
		"IBR-128-AE: the emirate is missing true true true")
	page, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?delivery_outcome=rejected", "", "")
	check(t, "on the worklist", len(page["items"].([]any)), 1)
	s.later(time.Minute)
	if step := s.advance(t, id, op); !step.Done {
		t.Errorf("both final: got %+v, want done", step)
	}
	check(t, "approved", s.delivery(t, id), "BUSINESS_RULE_FATAL rejected TDD_APPROVED <nil>")
	_, before := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	if step := s.advance(t, id, op); !step.Done {
		t.Errorf("run again: got %+v, want done", step)
	}
	if _, after := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", ""); after != before {
		t.Errorf("run again: got the ETag %s, want %s unchanged", after, before)
	}
	var events int
	pgtest.Scan(t, `SELECT count(*) FROM einvoicing.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.einvoicing.outgoing_einvoice_transmission_changed.v1'
		AND payload->>'tax_data_status' IS NOT NULL`, []any{s.f.Tenant.ID}, &events)
	check(t, "events of the two reads", events, 2)

	memo := s.posted(t, s.customer, fmt.Sprintf(`"kind": "credit_memo", "reference_invoice_id": %q, `, s.invoice["customer_invoice_id"]), "AED",
		s.item("10000.00", "AED", "S5@AE-DU"))
	note := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q, "credit_note_reason": {"code": "CR-01", "text": "Rejected by the buyer"}}`, memo))
	noteID := note["id"].(string)
	noteOp := s.send(t, noteID, s.connector)
	s.advance(t, noteID, noteOp)
	check(t, "a credit note not yet transmitted corrects nothing", s.delivery(t, id), "BUSINESS_RULE_FATAL rejected TDD_APPROVED <nil>")
	s.later(30 * time.Second)
	s.advance(t, noteID, noteOp)
	check(t, "corrected once the credit note is transmitted", s.delivery(t, id), "BUSINESS_RULE_FATAL corrected TDD_APPROVED <nil>")
	for outcome, want := range map[string]int{"rejected": 0, "corrected": 1, "pending": 1} {
		page, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?delivery_outcome="+outcome, "", "")
		check(t, outcome, len(page["items"].([]any)), want)
	}
}

// TestDeliveryOutcomes: what each response means for the e-invoice.
func TestDeliveryOutcomes(t *testing.T) {
	s := senderSetup(t)
	cases := []struct{ response, taxData, want string }{
		{"ACCEPTANCE", "TDD_APPROVED", "accepted"},
		{"BUSINESS_RULE_WARNING", "", "accepted"},
		{"REJECTION", "", "rejected"},
		{"SYNTAX_VIOLATION", "", "rejected"},
		{"ACCEPTANCE", "TDD_REJECTED", "rejected"},
		{"DELIVERY_FAILURE", "", "undelivered"},
		{"ACKNOWLEDGING", "TDD_SEND", "pending"},
	}
	for _, c := range cases {
		t.Run(c.response+" "+c.taxData, func(t *testing.T) {
			id := s.render(t, 201, fmt.Sprintf(`{"customer_invoice_id": %q}`, s.posted(t, s.customer, "", "AED", s.item("10.00", "AED", "Z0"))))["id"].(string)
			got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
			script := einvoicetest.DeliveryScript{Responses: []einvoicetest.Response{{Status: c.response}}}
			if c.taxData != "" {
				script.TaxData = map[string][]string{"report": {c.taxData}}
			}
			s.srv.Evatra.ScriptDelivery(got["customer_invoice_number"].(string), script)
			op := s.completed(t, id)
			s.later(30 * time.Second)
			s.advance(t, id, op)
			got, _ = s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
			check(t, "the outcome", got["transmission"].(map[string]any)["delivery_outcome"], c.want)
		})
	}
}

// TestTaxDataRetryAndWithdraw (As built E4): a person asks for a failed
// tax data document to be sent again: the request is written with its
// Operation (kind einvoicing.tax_data), which takes over the
// transmission, and a second is refused while it is not made; its step
// makes the call, succeeds the Operation and reads the delivery status
// again at once, its reads counted afresh; the send's step is done, the
// request's carries the reading on. A withdrawal of the approved one is
// made the same way. A step run again after its record was lost asks the
// provider nothing twice.
func TestTaxDataRetryAndWithdraw(t *testing.T) {
	s := senderSetup(t)
	id, number := s.invoice["id"].(string), s.invoice["customer_invoice_number"].(string)
	s.srv.Evatra.ScriptDelivery(number, einvoicetest.DeliveryScript{Responses: []einvoicetest.Response{{Status: "ACCEPTANCE"}},
		TaxData: map[string][]string{"report": {"TDD_FAILED"}, "retry": {"TDD_SEND", "TDD_APPROVED"}, "withdraw": {"WITHDRAW_SEND"}}})
	send := s.completed(t, id)
	s.later(30 * time.Second)
	if step := s.advance(t, id, send); !step.Done {
		t.Errorf("TDD_FAILED is final: got %+v, want done", step)
	}
	check(t, "failed at the authority's corner", s.delivery(t, id), "ACCEPTANCE accepted TDD_FAILED <nil>")
	failed, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?tax_data_status=TDD_FAILED", "", "")
	check(t, "found by its tax data document's status", len(failed["items"].([]any)), 1)
	s.f.Problem(409, "einvoicing.tax_data_not_withdrawable", "POST", "/v1/outgoing-e-invoices/"+id+":withdrawTaxData", "", s.tag(t, id))
	retry := s.request(t, id, "retry")
	check(t, "the request written", s.delivery(t, id), "ACCEPTANCE accepted TDD_FAILED retry")
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	check(t, "its Operation takes over", got["transmission"].(map[string]any)["operation_id"], retry.String())
	s.f.Problem(409, "einvoicing.tax_data_request_pending", "POST", "/v1/outgoing-e-invoices/"+id+":retryTaxData", "", s.tag(t, id))
	if step := s.advance(t, id, send); !step.Done {
		t.Errorf("the send's step: got %+v, want done, the request's Operation owns it", step)
	}
	calls := 0
	losing := func(ctx context.Context, fn func(ctx context.Context) error) error {
		calls++
		if calls == 4 { // the read, the connector, the claim, then the record: lost
			return errors.New("the record lost")
		}
		return s.within()(ctx, fn)
	}
	if _, err := s.e.AdvanceSubmission(context.Background(), uuid.MustParse(id), retry, losing); err == nil {
		t.Fatal("the record lost: got no error, want it")
	}
	check(t, "made once", fmt.Sprint(s.srv.Evatra.TaxDataRequests()), fmt.Sprint([]string{"retry " + tracking(t, s, id)}))
	s.later(5 * einvoicing.CallTimeout)
	if step := s.advance(t, id, retry); step.Done || step.Wait != 0 {
		t.Errorf("after its claim lapsed: got %+v, want the delivery read at once", step)
	}
	check(t, "found made, not asked twice", len(s.srv.Evatra.TaxDataRequests()), 1)
	check(t, "its Operation", operation(t, retry), "succeeded /v1/outgoing-e-invoices/"+id+" -")
	check(t, "sent again, as found", s.delivery(t, id), "ACCEPTANCE accepted TDD_SEND <nil>")
	if step := s.advance(t, id, retry); !step.Done {
		t.Errorf("approved: got %+v, want done", step)
	}
	check(t, "approved", s.delivery(t, id), "ACCEPTANCE accepted TDD_APPROVED <nil>")
	s.f.Problem(409, "einvoicing.tax_data_not_failed", "POST", "/v1/outgoing-e-invoices/"+id+":retryTaxData", "", s.tag(t, id))
	withdraw := s.request(t, id, "withdraw")
	s.advance(t, id, withdraw)
	s.advance(t, id, withdraw)
	check(t, "withdrawn", words(s.delivery(t, id), operation(t, withdraw)), "ACCEPTANCE accepted WITHDRAW_SEND <nil> succeeded /v1/outgoing-e-invoices/"+id+" -")
	check(t, "the requests made", len(s.srv.Evatra.TaxDataRequests()), 2)
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.outgoing.tax_data'`, []any{s.f.Tenant.ID}, &audits)
	check(t, "the requests audited", audits, 2)
}

// tracking is the e-invoice's tracking id.
func tracking(t *testing.T, s *sender, id string) string {
	t.Helper()
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	return got["transmission"].(map[string]any)["tracking_id"].(string)
}

// TestTaxDataOutcomeLater: when the provider still shows the status the
// call found after a request was made, the delivery is read on until it
// moves (As built E4), not taken as final; the in-process sender finds the
// tenant though its Operations have finished.
func TestTaxDataOutcomeLater(t *testing.T) {
	s := senderSetup(t)
	id, number := s.invoice["id"].(string), s.invoice["customer_invoice_number"].(string)
	s.srv.Evatra.ScriptDelivery(number, einvoicetest.DeliveryScript{Responses: []einvoicetest.Response{{Status: "ACCEPTANCE"}},
		TaxData: map[string][]string{"report": {"TDD_FAILED"}, "retry": {"TDD_FAILED", "TDD_FAILED", "TDD_REJECTED"}}})
	send := s.completed(t, id)
	s.later(30 * time.Second)
	s.advance(t, id, send)
	retry := s.request(t, id, "retry")
	s.advance(t, id, retry) // made: the status before the call kept, awaited
	if step := s.advance(t, id, retry); step.Done || step.Wait != time.Minute {
		t.Errorf("still TDD_FAILED after the call: got %+v, want a read again in a minute", step)
	}
	check(t, "made, awaited", words(operation(t, retry), s.delivery(t, id)), "succeeded /v1/outgoing-e-invoices/"+id+" - ACCEPTANCE accepted TDD_FAILED <nil>")
	got, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	check(t, "shown awaited", got["transmission"].(map[string]any)["tax_data_awaited"], "TDD_FAILED")
	s.f.Problem(409, "einvoicing.tax_data_request_pending", "POST", "/v1/outgoing-e-invoices/"+id+":retryTaxData", "", s.tag(t, id))
	failed, _ := s.f.Must(200, "GET", "/v1/outgoing-e-invoices?tax_data_status=TDD_FAILED", "", "")
	check(t, "not among those to send again", len(failed["items"].([]any)), 0)
	var tenants []uuid.UUID
	err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_worker", ReadOnly: true}, func(_ context.Context, tx pgx.Tx) error {
		var err error
		tenants, err = einvoicing.FollowedTenants(context.Background(), tx, s.clock().Add(time.Minute))
		return err
	})
	check(t, "its tenant followed", words(err, slices.Contains(tenants, s.f.Tenant.ID)), "<nil> true")
	s.later(time.Minute)
	s.advance(t, id, retry)
	s.later(2 * time.Minute)
	if step := s.advance(t, id, retry); !step.Done {
		t.Errorf("moved to TDD_REJECTED: got %+v, want done", step)
	}
	check(t, "rejected by the authority", s.delivery(t, id), "ACCEPTANCE rejected TDD_REJECTED <nil>")
	got, _ = s.f.Must(200, "GET", "/v1/outgoing-e-invoices/"+id, "", "")
	check(t, "no longer awaited", got["transmission"].(map[string]any)["tax_data_awaited"], nil)
	if err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_worker", ReadOnly: true}, func(_ context.Context, tx pgx.Tx) error {
		_, err := einvoicing.FollowedTenants(context.Background(), &pgtest.FailingTx{Tx: tx, FailAt: 1}, s.clock())
		return err
	}); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("its statement failing: got %v, want the injected failure", err)
	}
}
