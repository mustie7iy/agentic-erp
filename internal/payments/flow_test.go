package payments_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestProposeReleaseTransmit is T5's exit criterion: the proposal runs as
// an operation, a person other than the proposer releases it, the payment
// clears the supplier's items and the bank file is transmitted.
func TestProposeReleaseTransmit(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier GmbH", "2026-01-01", "", "create")
	inv1 := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.credit(t, e.Supplier, "30.00", "2026-03-02")
	e.invoice(t, e.Supplier, "INV-2", "50.00", "2026-03-05")
	later := e.invoice(t, e.Supplier, "INV-3", "250.00", "2026-03-20") // due 19 April, after the due date

	op := e.propose(t, "2026-04-15", "2026-04-15")
	if op["kind"] != "payments.propose" || op["status"] != "queued" || !strings.HasPrefix(op["resource"].(string), "/v1/payment-runs/") {
		t.Fatalf("operation: %v", op)
	}
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if run["status"] != "proposing" || run["operation_id"] != op["id"] || run["proposed_by"] != e.Tenant.Human.ID.String() ||
		!strings.HasPrefix(run["document_number"].(string), "PR") || run["total_amount"].(map[string]any)["amount"] != "0.00" {
		t.Fatalf("proposing: %v", run)
	}
	if n := work(t); n != 1 {
		t.Fatalf("worker did %d jobs", n)
	}
	run, tag := e.Must(200, "GET", op["resource"].(string), "", "")
	if run["status"] != "proposed" || run["total_amount"].(map[string]any)["amount"] != "120.00" || tag != `"2"` {
		t.Fatalf("proposed: %v %s", run, tag)
	}
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if done["status"] != "succeeded" || done["progress"].(map[string]any)["done"] != float64(3) || done["progress"].(map[string]any)["total"] != float64(3) ||
		done["result"].(map[string]any)["payment_count"] != float64(1) {
		t.Fatalf("operation done: %v", done)
	}
	pays := e.payments(t, run)
	pay := pays[0].(map[string]any)
	items := pay["items"].([]any)
	if len(pays) != 1 || pay["supplier_id"] != e.Supplier.String() || pay["amount"].(map[string]any)["amount"] != "120.00" ||
		pay["end_to_end_id"] != "2000-"+run["document_number"].(string)+"-000001" || len(items) != 3 || pay["journal_entry_id"] != nil {
		t.Fatalf("payments: %v", pays)
	}
	if first, memo := items[0].(map[string]any), items[1].(map[string]any); first["journal_entry_id"] != inv1["journal_entry_id"] ||
		first["due_date"] != "2026-03-31" || first["amount"].(map[string]any)["amount"] != "100.00" || first["is_active"] != true ||
		memo["amount"].(map[string]any)["amount"] != "-30.00" {
		t.Fatalf("items: %v", items)
	}
	if ev := events(t, e.Tenant, "payment_run_proposed"); len(ev) != 1 || ev[0]["payment_count"] != float64(1) || ev[0]["exclusion_count"] != float64(0) {
		t.Fatalf("proposed events: %v", ev)
	}

	// The proposer can never decide the run; agents never release.
	path := "/v1/payment-runs/" + id(run)
	e.Problem(403, "identity.sod_conflict", "POST", path+":release", "", tag)
	e.Problem(403, "identity.sod_conflict", "POST", path+":reject", `{"reason": "no"}`, tag)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", path+":release", "", tag)

	released := e.release(t, run, `{"comment": "checked"}`)
	if released["decided_by"] != e.Approver.ID.String() || released["decided_at"] == nil {
		t.Fatalf("released: %v", released)
	}
	if done, _ := e.Must(200, "GET", "/v1/operations/"+released["release_operation_id"].(string), "", ""); done["progress"].(map[string]any)["done"] != float64(1) ||
		done["result"].(map[string]any)["bank_file_id"] == nil {
		t.Fatalf("release operation: %v", done)
	}
	pay = e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+pay["journal_entry_id"].(string), "", "")
	lines := je["lines"].([]any)
	if je["document_type"] != "ZP" || je["source_context"] != "payments" || je["reference"] != pay["end_to_end_id"] || len(lines) != 4 ||
		lines[3].(map[string]any)["account_id"] != e.Account(t, "2000", "100000").String() ||
		lines[3].(map[string]any)["amount"].(map[string]any)["amount"] != "-120.00" {
		t.Fatalf("payment entry: %v", je)
	}
	open, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.Supplier.String(), "", "")
	if items := open["items"].([]any); len(items) != 1 || items[0].(map[string]any)["journal_entry_id"] != later["journal_entry_id"] {
		t.Fatalf("open items after payment: %v", open)
	}
	e.Problem(409, "payables.invoice_cleared", "POST", "/v1/supplier-invoices/"+id(inv1)+":reverse", `{"reason": "r"}`, `"2"`)
	if ev := events(t, e.Tenant, "payment_run_released"); len(ev) != 1 || ev[0]["payment_count"] != float64(1) || ev[0]["workflow_request_id"] != nil {
		t.Fatalf("released events: %v", ev)
	}
	e.decide(t, 409, run, "release", "")

	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := files["items"].([]any)[0].(map[string]any)
	created := events(t, e.Tenant, "bank_file_created")
	if file["status"] != "created" || file["format"] != "pain.001.001.03" || len(created) != 1 || created[0]["bank_file_id"] != file["id"] {
		t.Fatalf("bank file: %v %v", file, created)
	}
	sent, _ := e.Must(202, "POST", "/v1/bank-files/"+id(file)+":transmit", "", `"1"`)
	if sent["status"] != "succeeded" || sent["kind"] != "payments.transmit" || sent["resource"] != "/v1/bank-files/"+id(file) {
		t.Fatalf("transmit: %v", sent)
	}
	got := e.connector.files[0]
	xml := string(got.Content)
	if got.Name != "2000-"+run["document_number"].(string)+"-"+id(file)+".xml" || !strings.Contains(xml, "<IBAN>DE89370400440532013000</IBAN>") ||
		!strings.Contains(xml, `<InstdAmt Ccy="EUR">120.00</InstdAmt>`) || !strings.Contains(xml, "<Ustrd>INV-1 CM-1 INV-2</Ustrd>") ||
		!strings.Contains(xml, "<Nm>Supplier GmbH</Nm>") || !strings.Contains(xml, "<IBAN>DE89370400440532013000</IBAN>") {
		t.Fatalf("file %s:\n%s", got.Name, xml)
	}
	file, _ = e.Must(200, "GET", "/v1/bank-files/"+id(file), "", "")
	if file["status"] != "transmitted" {
		t.Fatalf("transmitted: %v", file)
	}
	if ev := events(t, e.Tenant, "bank_file_transmitted"); len(ev) != 1 || ev[0]["sha256"] != created[0]["sha256"] {
		t.Fatalf("transmitted events: %v", ev)
	}
	e.Problem(409, "payments.invalid_status", "POST", "/v1/bank-files/"+id(file)+":transmit", "", `"2"`)
	// The bank's status report: acknowledged, once; a rejection no longer applies.
	report := "/v1/bank-files/" + id(file) + ":report"
	e.Problem(412, "request.version_mismatch", "POST", report, `{"outcome": "accepted"}`, `"1"`)
	e.Problem(400, "request.invalid_body", "POST", report, `{}`, `"2"`)
	e.Problem(422, "payments.invalid_outcome", "POST", report, `{"outcome": "lost"}`, `"2"`)
	e.Problem(422, "payments.reason_required", "POST", report, `{"outcome": "rejected"}`, `"2"`)
	acked, tag := e.Must(200, "POST", report, `{"outcome": "accepted", "reference": "pain.002-0001"}`, `"2"`)
	if acked["status"] != "acknowledged" || tag != `"3"` {
		t.Fatalf("acknowledged: %v %v", acked, tag)
	}
	if ev := events(t, e.Tenant, "bank_file_acknowledged"); len(ev) != 1 || ev[0]["previous_status"] != "transmitted" || ev[0]["status"] != "acknowledged" || ev[0]["report_reference"] != "pain.002-0001" {
		t.Fatalf("acknowledged event: %v", ev)
	}
	e.Problem(409, "payments.invalid_status", "POST", report, `{"outcome": "accepted"}`, `"3"`)
	e.Problem(409, "payments.invalid_status", "POST", report, `{"outcome": "rejected", "reason": "AC01"}`, `"3"`)
	e.Problem(404, "request.not_found", "POST", "/v1/bank-files/0192a000-0000-7000-8000-00000000dead:report", `{"outcome": "rejected", "reason": "AC01"}`, "*")
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
	if ev := events(t, e.Tenant, "payment_run_proposed"); ev[0]["exclusion_count"] != float64(6) || ev[0]["total_amount"].(map[string]any)["amount"] != "10.00" {
		t.Fatalf("event: %v", ev)
	}
}

func TestRejectFreesItems(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	inv := e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	first := e.run(t, "2026-04-15", "2026-04-15")
	// Items in a live run are reserved: another run does not pay them, and the invoice cannot be reversed.
	second := e.run(t, "2026-04-16", "2026-04-16")
	if pays := e.payments(t, second); len(pays) != 0 || len(e.exclusions(t, second)) != 0 {
		t.Fatalf("second run pays reserved items: %v", pays)
	}
	e.decide(t, 409, second, "release", "")
	e.Problem(409, "payables.invoice_in_payment_run", "POST", "/v1/supplier-invoices/"+id(inv)+":reverse", `{"reason": "r"}`, `"2"`)

	e.decide(t, 400, first, "reject", `{}`)
	rejected := e.decide(t, 200, first, "reject", `{"reason": "Wrong due date"}`)
	if rejected["status"] != "rejected" || rejected["decided_by"] != e.Approver.ID.String() {
		t.Fatalf("rejected: %v", rejected)
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
	if got["code"] != "payments.proposal_outdated" || !strings.Contains(got["detail"].(string), "bank_account_changed") {
		t.Fatalf("bank account changed: %v", got)
	}
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_payment_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	if got := e.decide(t, 409, run, "release", ""); !strings.Contains(got["detail"].(string), "payment_blocked") {
		t.Fatalf("payment blocked: %v", got)
	}
}
