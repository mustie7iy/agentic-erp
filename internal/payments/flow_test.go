package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestProposeReleaseTransmit is T5's exit criterion: the proposal runs as
// an operation, a person other than the proposer releases it, the payment
// clears the supplier's items and the bank file is transmitted.
func TestProposeReleaseTransmit(t *testing.T) {
	e := setup(t)
	var (
		inv1, later, op, run, file map[string]any
		tag                        string
	)
	if !t.Run("propose", func(t *testing.T) {
		e := e.in(t)
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier GmbH", "2026-01-01", "", "create")
		inv1 = e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
		e.credit(t, e.Supplier, "30.00", "2026-03-02")
		e.invoice(t, e.Supplier, "INV-2", "50.00", "2026-03-05")
		later = e.invoice(t, e.Supplier, "INV-3", "250.00", "2026-03-20") // due 19 April, after the due date
		op = e.propose(t, "2026-04-15", "2026-04-15")
		if op["kind"] != "payments.propose" {
			t.Errorf("operation kind: got %v, want payments.propose", op["kind"])
		}
		if op["status"] != "queued" {
			t.Errorf("operation status: got %v, want queued", op["status"])
		}
		if got := op["resource"].(string); !strings.HasPrefix(got, "/v1/payment-runs/") {
			t.Fatalf("operation resource: got %s, want a payment run", got)
		}
		run, _ = e.Must(200, "GET", op["resource"].(string), "", "")
		if run["status"] != "proposing" {
			t.Errorf("run status: got %v, want proposing", run["status"])
		}
		if run["operation_id"] != op["id"] {
			t.Errorf("run operation: got %v, want %v", run["operation_id"], op["id"])
		}
		if run["proposed_by"] != e.Tenant.Human.ID.String() {
			t.Errorf("run proposed by: got %v, want %v", run["proposed_by"], e.Tenant.Human.ID)
		}
		if got := run["document_number"].(string); !strings.HasPrefix(got, "PR") {
			t.Errorf("run document number: got %s, want prefix PR", got)
		}
		if got := run["total_amount"].(map[string]any)["amount"]; got != "0.00" {
			t.Errorf("run total while proposing: got %v, want 0.00", got)
		}
		if n := work(t); n != 1 {
			t.Fatalf("worker did %d jobs", n)
		}
	}) {
		t.FailNow()
	}
	if !t.Run("proposed run", func(t *testing.T) {
		e := e.in(t)
		run, tag = e.Must(200, "GET", op["resource"].(string), "", "")
		if run["status"] != "proposed" {
			t.Errorf("run status: got %v, want proposed", run["status"])
		}
		if got := run["total_amount"].(map[string]any)["amount"]; got != "120.00" {
			t.Errorf("run total: got %v, want 120.00", got)
		}
		if tag != `"2"` {
			t.Errorf("run etag: got %s, want \"2\"", tag)
		}
		checkProposeOperation(t, e, op)
		checkProposedPayment(t, e, run, inv1)
	}) {
		t.FailNow()
	}
	if !t.Run("release", func(t *testing.T) { checkRelease(t, e.in(t), run, inv1, later, tag) }) {
		t.FailNow()
	}
	if !t.Run("transmit", func(t *testing.T) { file = checkTransmit(t, e.in(t), run) }) {
		t.FailNow()
	}
	t.Run("status report", func(t *testing.T) {
		e := e.in(t)
		// The bank's status report: acknowledged, once; a rejection no longer applies.
		report := "/v1/bank-files/" + id(file) + ":report"
		e.Problem(412, "request.version_mismatch", "POST", report, `{"outcome": "accepted"}`, `"1"`)
		e.Problem(400, "request.invalid_body", "POST", report, `{}`, `"4"`)
		e.Problem(422, "payments.invalid_outcome", "POST", report, `{"outcome": "lost"}`, `"4"`)
		e.Problem(422, "payments.reason_required", "POST", report, `{"outcome": "rejected"}`, `"4"`)
		acked, tag := e.Must(200, "POST", report, `{"outcome": "accepted", "reference": "pain.002-0001"}`, `"4"`)
		wantBankCodes(t, "bank file", acked)
		if acked["status"] != "acknowledged" {
			t.Errorf("acknowledged status: got %v, want acknowledged", acked["status"])
		}
		if tag != `"5"` {
			t.Errorf("acknowledged etag: got %s, want \"5\"", tag)
		}
		ev := events(t, e.Tenant, "bank_file_acknowledged")
		if len(ev) != 1 {
			t.Fatalf("acknowledged events: got %d, want 1: %v", len(ev), ev)
		}
		if ev[0]["previous_status"] != "transmitted" {
			t.Errorf("acknowledged event previous status: got %v, want transmitted", ev[0]["previous_status"])
		}
		if ev[0]["status"] != "acknowledged" {
			t.Errorf("acknowledged event status: got %v, want acknowledged", ev[0]["status"])
		}
		if ev[0]["report_reference"] != "pain.002-0001" {
			t.Errorf("acknowledged event report reference: got %v, want pain.002-0001", ev[0]["report_reference"])
		}
		e.Problem(409, "payments.invalid_status", "POST", report, `{"outcome": "accepted"}`, `"5"`)
		e.Problem(409, "payments.invalid_status", "POST", report, `{"outcome": "rejected", "reason": "AC01"}`, `"5"`)
		e.Problem(404, "request.not_found", "POST", "/v1/bank-files/0192a000-0000-7000-8000-00000000dead:report", `{"outcome": "rejected", "reason": "AC01"}`, "*")
	})
}

// checkProposeOperation checks the finished proposal operation.
func checkProposeOperation(t *testing.T, e env, op map[string]any) {
	t.Helper()
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if done["status"] != "succeeded" {
		t.Errorf("operation status: got %v, want succeeded", done["status"])
	}
	if got := done["progress"].(map[string]any)["done"]; got != float64(3) {
		t.Errorf("operation progress done: got %v, want 3", got)
	}
	if got := done["progress"].(map[string]any)["total"]; got != float64(3) {
		t.Errorf("operation progress total: got %v, want 3", got)
	}
	if got := done["result"].(map[string]any)["payment_count"]; got != float64(1) {
		t.Errorf("operation payment count: got %v, want 1", got)
	}
}

// checkProposedPayment checks the proposed run's one payment, its items and
// the proposed event.
func checkProposedPayment(t *testing.T, e env, run, inv1 map[string]any) {
	t.Helper()
	pays := e.payments(t, run)
	pay := pays[0].(map[string]any)
	items := pay["items"].([]any)
	if len(pays) != 1 {
		t.Errorf("payments: got %d, want 1", len(pays))
	}
	if pay["supplier_id"] != e.Supplier.String() {
		t.Errorf("payment supplier: got %v, want %v", pay["supplier_id"], e.Supplier)
	}
	if got := pay["amount"].(map[string]any)["amount"]; got != "120.00" {
		t.Errorf("payment amount: got %v, want 120.00", got)
	}
	if want := "2000-" + run["document_number"].(string) + "-000001"; pay["end_to_end_id"] != want {
		t.Errorf("payment end-to-end id: got %v, want %s", pay["end_to_end_id"], want)
	}
	if pay["journal_entry_id"] != nil {
		t.Errorf("payment journal entry before release: got %v, want none", pay["journal_entry_id"])
	}
	if len(items) != 3 {
		t.Fatalf("payment items: got %d, want 3: %v", len(items), items)
	}
	first, memo := items[0].(map[string]any), items[1].(map[string]any)
	if first["journal_entry_id"] != inv1["journal_entry_id"] {
		t.Errorf("first item entry: got %v, want %v", first["journal_entry_id"], inv1["journal_entry_id"])
	}
	if first["due_date"] != "2026-03-31" {
		t.Errorf("first item due date: got %v, want 2026-03-31", first["due_date"])
	}
	if got := first["amount"].(map[string]any)["amount"]; got != "100.00" {
		t.Errorf("first item amount: got %v, want 100.00", got)
	}
	if first["is_active"] != true {
		t.Errorf("first item active: got %v, want true", first["is_active"])
	}
	if got := memo["amount"].(map[string]any)["amount"]; got != "-30.00" {
		t.Errorf("credit memo amount: got %v, want -30.00", got)
	}
	ev := events(t, e.Tenant, "payment_run_proposed")
	if len(ev) != 1 {
		t.Fatalf("proposed events: got %d, want 1: %v", len(ev), ev)
	}
	if ev[0]["payment_count"] != float64(1) {
		t.Errorf("proposed event payment count: got %v, want 1", ev[0]["payment_count"])
	}
	if ev[0]["exclusion_count"] != float64(0) {
		t.Errorf("proposed event exclusion count: got %v, want 0", ev[0]["exclusion_count"])
	}
}

// checkRelease: the proposer can never decide the run and agents never
// release; the approver's release posts the payment, which clears the
// items it pays.
func checkRelease(t *testing.T, e env, run, inv1, later map[string]any, tag string) {
	t.Helper()
	path := "/v1/payment-runs/" + id(run)
	e.Problem(403, "identity.sod_conflict", "POST", path+":release", "", tag)
	e.Problem(403, "identity.sod_conflict", "POST", path+":reject", `{"reason": "no"}`, tag)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", path+":release", "", tag)
	released := e.release(t, run, `{"comment": "checked"}`)
	if released["decided_by"] != e.Approver.ID.String() {
		t.Errorf("released decided by: got %v, want %v", released["decided_by"], e.Approver.ID)
	}
	if released["decided_at"] == nil {
		t.Errorf("released decided at: got none, want a time")
	}
	done, _ := e.Must(200, "GET", "/v1/operations/"+released["release_operation_id"].(string), "", "")
	if got := done["progress"].(map[string]any)["done"]; got != float64(1) {
		t.Errorf("release operation progress: got %v, want 1", got)
	}
	if done["result"].(map[string]any)["bank_file_id"] == nil {
		t.Errorf("release operation bank file: got none, want one: %v", done)
	}
	checkPaymentEntry(t, e, run)
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Supplier.String(), "", "")
	if items := open["items"].([]any); len(items) != 1 || items[0].(map[string]any)["journal_entry_id"] != later["journal_entry_id"] {
		t.Fatalf("open items after payment: %v", open)
	}
	e.Problem(409, "payables.invoice_cleared", "POST", "/v1/supplier-invoices/"+id(inv1)+":reverse", `{"reason": "r"}`, `"2"`)
	ev := events(t, e.Tenant, "payment_run_released")
	if len(ev) != 1 {
		t.Fatalf("released events: got %d, want 1: %v", len(ev), ev)
	}
	if ev[0]["payment_count"] != float64(1) {
		t.Errorf("released event payment count: got %v, want 1", ev[0]["payment_count"])
	}
	if ev[0]["workflow_request_id"] != nil {
		t.Errorf("released event workflow request: got %v, want none", ev[0]["workflow_request_id"])
	}
	e.decide(t, 409, run, "release", "")
}

// checkPaymentEntry checks the journal entry the released payment posted.
func checkPaymentEntry(t *testing.T, e env, run map[string]any) {
	t.Helper()
	pay := e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+pay["journal_entry_id"].(string), "", "")
	lines := je["lines"].([]any)
	if je["document_type"] != "ZP" {
		t.Errorf("payment entry document type: got %v, want ZP", je["document_type"])
	}
	if je["source_context"] != "payments" {
		t.Errorf("payment entry source context: got %v, want payments", je["source_context"])
	}
	if je["reference"] != pay["end_to_end_id"] {
		t.Errorf("payment entry reference: got %v, want %v", je["reference"], pay["end_to_end_id"])
	}
	if len(lines) != 4 {
		t.Fatalf("payment entry lines: got %d, want 4: %v", len(lines), je)
	}
	bank := lines[3].(map[string]any)
	if want := e.Account(t, "2000", "100000").String(); bank["account_id"] != want {
		t.Errorf("bank line account: got %v, want %s", bank["account_id"], want)
	}
	if got := bank["amount"].(map[string]any)["amount"]; got != "-120.00" {
		t.Errorf("bank line amount: got %v, want -120.00", got)
	}
}

// checkTransmit checks the run's bank file and transmits it; it returns the
// file as transmitted.
func checkTransmit(t *testing.T, e env, run map[string]any) map[string]any {
	t.Helper()
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	created := events(t, e.Tenant, "bank_file_created")
	if file["status"] != "created" {
		t.Errorf("bank file status: got %v, want created", file["status"])
	}
	if file["format"] != "pain.001.001.03" {
		t.Errorf("bank file format: got %v, want pain.001.001.03", file["format"])
	}
	if len(created) != 1 {
		t.Fatalf("bank file created events: got %d, want 1: %v", len(created), created)
	}
	if created[0]["bank_file_id"] != file["id"] {
		t.Errorf("bank file created event: got %v, want %v", created[0]["bank_file_id"], file["id"])
	}
	asked := e.transmit(t, id(file), `"1"`)
	if asked["status"] != "queued" {
		t.Errorf("transmit status as asked for: got %v, want queued", asked["status"])
	}
	sent, _ := e.Must(200, "GET", "/v1/operations/"+id(asked), "", "")
	if sent["status"] != "succeeded" {
		t.Errorf("transmit status: got %v, want succeeded", sent["status"])
	}
	if sent["kind"] != "payments.transmit" {
		t.Errorf("transmit kind: got %v, want payments.transmit", sent["kind"])
	}
	if want := "/v1/bank-files/" + id(file); sent["resource"] != want {
		t.Errorf("transmit resource: got %v, want %s", sent["resource"], want)
	}
	got := e.connector.files[0]
	xml := string(got.Content)
	if want := "2000-" + run["document_number"].(string) + "-" + id(file) + ".xml"; got.Name != want {
		t.Errorf("file name: got %s, want %s", got.Name, want)
	}
	for _, want := range []string{"<IBAN>DE89370400440532013000</IBAN>", `<InstdAmt Ccy="EUR">120.00</InstdAmt>`, "<Ustrd>INV-1 CM-1 INV-2</Ustrd>", "<Nm>Supplier GmbH</Nm>"} {
		if !strings.Contains(xml, want) {
			t.Errorf("file %s: got\n%s\nwant it to contain %s", got.Name, xml, want)
		}
	}
	file, _ = e.Must(200, "GET", "/v1/bank-files/"+id(file), "", "")
	wantBankCodes(t, "bank file", file)
	if file["status"] != "transmitted" {
		t.Fatalf("transmitted: %v", file)
	}
	if ev := events(t, e.Tenant, "bank_file_transmitted"); len(ev) != 1 || ev[0]["sha256"] != created[0]["sha256"] {
		t.Fatalf("transmitted events: %v", ev)
	}
	if ev := events(t, e.Tenant, "bank_file_transmitted"); ev[0]["connector_id"] != nil {
		t.Errorf("transmitted event connector: got %v, want none (the server's directory)", ev[0]["connector_id"])
	}
	e.Problem(409, "payments.invalid_status", "POST", "/v1/bank-files/"+id(file)+":transmit", "", `"4"`)
	return file
}

func TestExclusions(t *testing.T) {
	e := setup(t)
	payable := func(number string) string {
		s := e.NewSupplier(t, number)
		e.invoice(t, s, "X-"+number, "10.00", "2026-03-01")
		return s.String()
	}
	noBank := e.Vendor.String()
	e.invoice(t, e.Vendor, "NB-1", "40.00", "2026-03-01")
	held := payable("BP90000003")
	e.bankAccount(t, uuidOf(held), "DE89370400440532013000", "Held", "2026-01-01", "2026-04-12", "create")
	e.bankAccount(t, uuidOf(held), "DE02120300000000202051", "Held", "2026-04-12", "", "bank_change") // hold until 17 April
	paymentBlocked := payable("BP90000004")
	e.bankAccount(t, uuidOf(paymentBlocked), "DE89370400440532013000", "PB", "2026-01-01", "", "create")
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_payment_blocked = true, version = version + 1 WHERE partner_id = $1`, uuidOf(paymentBlocked))
	blocked := payable("BP90000005")
	e.bankAccount(t, uuidOf(blocked), "DE89370400440532013000", "B", "2026-01-01", "", "create")
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1 WHERE id = $1`, uuidOf(blocked))
	pending := payable("BP90000006")
	e.bankAccount(t, uuidOf(pending), "DE89370400440532013000", "P", "2026-01-01", "", "create")
	pgtest.Exec(t, `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'PC-PENDING', 'bank_change', $2, '{}', 'new bank', 'pending', $3, $3, $3)`, e.Tenant.ID, uuidOf(pending), e.Tenant.Human.ID)
	debit := payable("BP90000007")
	e.bankAccount(t, uuidOf(debit), "DE89370400440532013000", "D", "2026-01-01", "", "create")
	e.credit(t, uuidOf(debit), "25.00", "2026-03-01")
	afterHold := payable("BP90000008")
	e.bankAccount(t, uuidOf(afterHold), "DE89370400440532013000", "A", "2026-01-01", "2026-04-01", "create")
	e.bankAccount(t, uuidOf(afterHold), "DE02120300000000202051", "A", "2026-04-01", "", "bank_change") // hold ended 6 April

	run := e.run(t, "2026-04-15", "2026-04-15")
	want := map[string]string{noBank: "no_bank_account 40.00", held: "bank_change_hold 10.00", paymentBlocked: "payment_blocked 10.00",
		blocked: "supplier_blocked 10.00", pending: "bank_change_pending 10.00", debit: "debit_balance -15.00"}
	got := e.exclusions(t, run)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("exclusions:\n got %v\nwant %v", got, want)
	}
	if pays := e.payments(t, run); len(pays) != 1 || pays[0].(map[string]any)["supplier_id"] != afterHold {
		t.Fatalf("payments: %v", pays)
	}
	ev := events(t, e.Tenant, "payment_run_proposed")
	if ev[0]["exclusion_count"] != float64(6) {
		t.Errorf("proposed event exclusion count: got %v, want 6", ev[0]["exclusion_count"])
	}
	if got := ev[0]["total_amount"].(map[string]any)["amount"]; got != "10.00" {
		t.Errorf("proposed event total: got %v, want 10.00", got)
	}
}

func TestRejectFreesItems(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	inv := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	first := e.run(t, "2026-04-15", "2026-04-15")
	// Items in a live run are reserved: another run does not pay them, and the invoice cannot be reversed.
	second := e.run(t, "2026-04-16", "2026-04-16")
	if pays := e.payments(t, second); len(pays) != 0 {
		t.Errorf("second run payments: got %v, want none for reserved items", pays)
	}
	if exs := e.exclusions(t, second); len(exs) != 0 {
		t.Errorf("second run exclusions: got %v, want none for reserved items", exs)
	}
	e.decide(t, 409, second, "release", "")
	refused := e.Problem(409, "payables.invoice_in_payment_run", "POST", "/v1/supplier-invoices/"+id(inv)+":reverse", `{"reason": "r"}`, `"2"`)
	if detail, _ := refused["detail"].(string); !strings.Contains(detail, "Reject the run once it is proposed") {
		t.Errorf("detail for a proposed run: got %q, want it to say to reject the run", refused["detail"])
	}

	e.decide(t, 400, first, "reject", `{}`)
	rejected := e.decide(t, 200, first, "reject", `{"reason": "Wrong due date"}`)
	if rejected["status"] != "rejected" {
		t.Errorf("rejected status: got %v, want rejected", rejected["status"])
	}
	if rejected["decided_by"] != e.Approver.ID.String() {
		t.Errorf("rejected decided by: got %v, want %v", rejected["decided_by"], e.Approver.ID)
	}
	if item := e.payments(t, first)[0].(map[string]any)["items"].([]any)[0].(map[string]any); item["is_active"] != false {
		t.Fatalf("item still active: %v", item)
	}
	if ev := events(t, e.Tenant, "payment_run_rejected"); len(ev) != 1 || ev[0]["released_item_count"] != float64(1) {
		t.Fatalf("rejected events: %v", ev)
	}
	e.decide(t, 409, first, "reject", `{"reason": "again"}`)
	third := e.run(t, "2026-04-17", "2026-04-17")
	if pays := e.payments(t, third); len(pays) != 1 {
		t.Fatalf("freed items not proposed again: %v", pays)
	}
}

func TestProposalOutdated(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	// A new bank account valid on the run date is not the one proposed.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET valid_to = '2026-02-01' WHERE partner_id = '%s'; END $$`, e.Supplier))
	e.bankAccount(t, e.Supplier, "DE02120300000000202051", "Supplier", "2026-02-01", "", "create")
	got := e.decide(t, 409, run, "release", "")
	if got["code"] != "payments.proposal_outdated" {
		t.Errorf("bank account changed code: got %v, want payments.proposal_outdated", got["code"])
	}
	if detail := got["detail"].(string); !strings.Contains(detail, "bank_account_changed") {
		t.Errorf("bank account changed detail: got %q, want it to name bank_account_changed", detail)
	}
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_payment_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	if got := e.decide(t, 409, run, "release", ""); !strings.Contains(got["detail"].(string), "payment_blocked") {
		t.Fatalf("payment blocked: %v", got)
	}
}

// TestReverseDuringResumedRelease: a run whose release failed after one
// payment posted stays releasing, to be resumed; the invoice another of its
// payments still has to pay cannot be reversed meanwhile, or the resumed
// release would pay a reversed invoice.
func TestReverseDuringResumedRelease(t *testing.T) {
	e := setup(t)
	h := endpoints(e.connector)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-A", "100.00", "2026-03-01")
	x := e.invoice(t, e.Vendor, "INV-X", "40.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	var payA uuid.UUID
	for _, pay := range e.payments(t, run) {
		if m := pay.(map[string]any); m["supplier_id"] == e.Supplier.String() {
			payA = uuidOf(id(m))
		}
	}
	if payA == uuid.Nil {
		t.Fatalf("payments: got none for supplier %s, want one", e.Supplier)
	}
	e.decide(t, 202, run, "release", "")
	// Payment A posts; the payment of invoice X fails, which fails the release with A posted.
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, payA); err != nil {
			return err
		}
		_, err = h.FailRelease(ctx, r, problem.Unprocessable("ledger.posting_refused", "test"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", ""); got["status"] != "releasing" {
		t.Fatalf("run after the failed release: got %v, want releasing", got["status"])
	}
	refused := e.Problem(409, "payables.invoice_in_payment_run", "POST", "/v1/supplier-invoices/"+id(x)+":reverse", `{"reason": "r"}`, `"2"`)
	if detail, _ := refused["detail"].(string); !strings.Contains(detail, "reverse the payment first") {
		t.Errorf("detail for a releasing run: got %q, want it to say to reverse the payment", refused["detail"])
	}
	// The resumed release pays invoice X, which still stands.
	e.release(t, run, "")
	if got, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(x), "", ""); got["status"] != "posted" {
		t.Fatalf("invoice X after the release: got %v, want posted", got["status"])
	}
}

// TestReservedItemsAreNotClearedByHand: an open item a live payment run
// pays is reserved in the ledger, so a manual clearing of it is refused
// (409 ledger.item_reserved) and the release does not later fail on an
// item cleared behind the run's back; once the run is rejected the item is
// free again.
func TestReservedItemsAreNotClearedByHand(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	memo := e.credit(t, e.Supplier, "100.00", "2026-05-01") // after the due date: not in the run
	run := e.run(t, "2026-04-15", "2026-04-15")
	item := e.payments(t, run)[0].(map[string]any)["items"].([]any)[0].(map[string]any)
	body := fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-05-02", "items": [
		{"fiscal_year": %v, "journal_entry_id": %q, "line_no": %v}, {"fiscal_year": %v, "journal_entry_id": %q, "line_no": 1}]}`,
		e.DE, item["fiscal_year"], item["journal_entry_id"], item["line_no"], memo["fiscal_year"], id(memo))
	refused := e.Problem(409, "ledger.item_reserved", "POST", "/v1/clearings", body, "")
	if detail, _ := refused["detail"].(string); !strings.Contains(detail, run["document_number"].(string)) {
		t.Errorf("detail: got %q, want it to name run %s", refused["detail"], run["document_number"])
	}
	e.decide(t, 200, run, "reject", `{"reason": "Clear by hand"}`)
	e.Must(201, "POST", "/v1/clearings", body, "")
}
