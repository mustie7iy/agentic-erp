package payments_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestCancelledAndFailedProposals(t *testing.T) {
	e := setup(t)
	op := e.propose(t, "2026-04-15", "2026-04-15")
	e.Must(200, "POST", "/v1/operations/"+id(op)+":cancel", "", `"1"`)
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if run["status"] != "failed" {
		t.Fatalf("cancelled: %v", run)
	}
	failed := events(t, e.Tenant, "payment_run_failed")
	if len(failed) != 1 {
		t.Fatalf("failed events: got %d, want 1: %v", len(failed), failed)
	}
	if failed[0]["previous_status"] != "proposing" {
		t.Errorf("failed event previous status: got %v, want proposing", failed[0]["previous_status"])
	}
	if failed[0]["released_item_count"] != float64(0) {
		t.Errorf("failed event released items: got %v, want 0", failed[0]["released_item_count"])
	}
	if failed[0]["operation_id"] != id(op) {
		t.Errorf("failed event operation: got %v, want %s", failed[0]["operation_id"], id(op))
	}

	// A problem while building fails the run and its operation.
	op = e.propose(t, "2026-04-15", "2026-04-15")
	pgtest.Exec(t, `DELETE FROM config.active_version WHERE tenant_id = $1`, e.Tenant.ID)
	work(t)
	run, _ = e.Must(200, "GET", op["resource"].(string), "", "")
	done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	if run["status"] != "failed" {
		t.Errorf("run status: got %v, want failed", run["status"])
	}
	if done["status"] != "failed" {
		t.Errorf("operation status: got %v, want failed", done["status"])
	}
	if got := done["error"].(map[string]any)["code"]; got != "config.not_bootstrapped" {
		t.Errorf("operation error: got %v, want config.not_bootstrapped", got)
	}
	e.decide(t, 409, run, "release", "")
}

func TestProposeValidation(t *testing.T) {
	e := setup(t)
	body := func(le, bank uuid.UUID, runDate string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": %q, "due_by": %q}`, le, bank, runDate, runDate)
	}
	var usBank uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, e.US).Scan(&usBank); err != nil {
		t.Fatal(err)
	}
	inactive := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Dormant', 'DE', 'EUR', gen_random_uuid(), 'inactive', $3, $3)`, e.Tenant.ID, inactive, e.Tenant.Human.ID)
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"unknown legal entity", body(uuid.New(), e.bank, "2026-04-15"), 422, "request.reference_not_found"},
		{"inactive legal entity", body(inactive, e.bank, "2026-04-15"), 422, "payments.legal_entity_not_active"},
		{"other legal entity's bank", body(e.DE, usBank, "2026-04-15"), 422, "request.reference_not_found"},
		{"unknown bank", body(e.DE, uuid.New(), "2026-04-15"), 422, "request.reference_not_found"},
		{"no fiscal year", body(e.DE, e.bank, "2031-01-15"), 422, "ledger.no_fiscal_period"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := e.Client
			api.T = t
			api.Problem(c.status, c.code, "POST", "/v1/payment-runs:propose", c.body, "")
		})
	}
	pgtest.Exec(t, `UPDATE payments.house_bank SET status = 'inactive', active_gl_account_id = NULL, version = version + 1 WHERE id = $1`, e.bank)
	e.Problem(422, "payments.house_bank_inactive", "POST", "/v1/payment-runs:propose", body(e.DE, e.bank, "2026-04-15"), "")
	e.Problem(404, "request.not_found", "GET", "/v1/payment-runs/"+uuid.NewString(), "", "")
}

func TestForeignExchange(t *testing.T) {
	e := setup(t)
	rate := func(from, rate string) {
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', $2, $3, 'ECB', $4)`, e.Tenant.ID, from, rate, e.Tenant.Human.ID)
	}
	rate("2026-01-01", "0.90")
	rate("2026-04-01", "0.95")
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "US-1",
		"invoice_date": "2026-03-01", "gross_amount": {"amount": "100.00", "currency": "USD"}, "tax_amount": {"amount": "0.00", "currency": "USD"},
		"items": [{"account_id": %q, "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "100.00", "currency": "USD"}}]}`,
		e.US, e.Supplier, e.Account(t, "1000", "520000")), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-01"}`, `"1"`)
	var bank uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM payments.house_bank WHERE legal_entity_id = $1`, e.US).Scan(&bank); err != nil {
		t.Fatal(err)
	}
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, e.US, bank), "")
	work(t)
	run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	// The invoice is booked at 90.00 EUR and paid at 95.00 EUR: a loss in group currency, which needs an account.
	// The release operation fails with the ledger's problem; nothing was posted, so the run is proposed again.
	_, after := e.releaseFails(t, run, "ledger.no_fx_account")
	if after["status"] != "proposed" {
		t.Errorf("after the failed release, status: got %v, want proposed", after["status"])
	}
	if after["decided_by"] != nil {
		t.Errorf("after the failed release, decided by: got %v, want none", after["decided_by"])
	}
	loss := e.Account(t, "1000", "510000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_loss', $3, $4)`,
		e.Tenant.ID, e.US, loss, e.Tenant.Human.ID)
	run, _ = e.Must(200, "GET", op["resource"].(string), "", "")
	e.release(t, run, "")
	pay := e.payments(t, run)[0].(map[string]any)
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+pay["journal_entry_id"].(string), "", "")
	lines := je["lines"].([]any)
	fx := lines[2].(map[string]any)
	if len(lines) != 3 {
		t.Errorf("payment entry lines: got %d, want 3: %v", len(lines), lines)
	}
	if fx["account_id"] != loss.String() {
		t.Errorf("fx line account: got %v, want %v", fx["account_id"], loss)
	}
	if got := fx["amount"].(map[string]any)["amount"]; got != "0.00" {
		t.Errorf("fx line amount: got %v, want 0.00", got)
	}
	if got := fx["amount_gc"].(map[string]any)["amount"]; got != "5.00" {
		t.Errorf("fx line group amount: got %v, want 5.00", got)
	}
	if got := lines[1].(map[string]any)["amount_gc"].(map[string]any)["amount"]; got != "-95.00" {
		t.Errorf("bank line group amount: got %v, want -95.00", got)
	}
}

// TestTransmitFailures: a request no channel could take is refused; the
// bank not taking the file is tried again after each wait of
// transmitBackoff and fails after the sixth attempt, with its event; a
// refusal (a file that no longer matches its hash) fails at once; an
// internal error is tried again. Nothing is sent meanwhile.
func TestTransmitFailures(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, run, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	file := id(files["items"].([]any)[0].(map[string]any))

	// Without a connector nothing is asked for.
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints(nil).TransmitFile(ctx, uuidOf(file), func(int32) error { return nil })
		return err
	})
	if p := problem.From(err); p.Status != 503 {
		t.Fatalf("no connector: %v", err)
	}
	e.Problem(412, "request.version_mismatch", "POST", "/v1/bank-files/"+file+":transmit", "", `"9"`)

	e.connector.err = errBank
	op := e.transmit(t, file, `"1"`)
	checkBackoff(t, e, file)
	e.failedWith(t, op, "platform.unavailable")
	if ev := events(t, e.Tenant, "bank_file_transmission_failed"); len(ev) != 1 || ev[0]["attempts"] != float64(6) {
		t.Errorf("failed events: got %v, want one after 6 attempts", ev)
	}
	e.connector.err = nil

	// A run whose data no longer renders the recorded file is refused at once.
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = $2, version = version + 1 WHERE id = $1`, e.bank,
		apitest.Seal(t, e.Tenant.ID, "DE02120300000000202051"))
	e.failedWith(t, e.transmit(t, file, version(t, e, file)), "payments.bank_file_changed")
	// Bank details that cannot be opened are an internal error, tried again.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET account_cipher = '\x00' WHERE partner_id = '%s'; END $$`, e.Supplier))
	e.transmit(t, file, version(t, e, file))
	if status, attempts, code := fileState(t, file); status != "to_send" || attempts != 1 || code != "platform.internal" {
		t.Errorf("after an internal error: got %s, %d attempts, %s; want to_send, 1, platform.internal", status, attempts, code)
	}
	// So is a house bank IBAN that cannot be opened.
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = '\x00', version = version + 1 WHERE id = $1`, e.bank)
	tr := e.transmitter(t)
	next := due(t, file)
	tr.Now = func() time.Time { return next }
	if _, err := tr.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, attempts, code := fileState(t, file); status != "to_send" || attempts != 2 || code != "platform.internal" {
		t.Errorf("after the house bank's internal error: got %s, %d attempts, %s; want to_send, 2, platform.internal", status, attempts, code)
	}
	pgtest.Exec(t, `UPDATE payments.house_bank SET iban_cipher = $2, version = version + 1 WHERE id = $1`, e.bank,
		apitest.Seal(t, e.Tenant.ID, "DE89370400440532013000"))
	if len(e.connector.files) != 0 {
		t.Errorf("files transmitted after failures: got %v, want none", e.connector.files)
	}

	// A file the bank format refuses stops the release.
	e.bankAccount(t, e.Vendor, "DE89370400440532013000", " ", "2026-01-01", "", "create")
	e.invoice(t, e.Vendor, "V-1", "10.00", "2026-03-01")
	second := e.run(t, "2026-04-16", "2026-04-16")
	// The payment posts, then the file is refused: the operation fails and the run stays releasing with its posting, to be resumed once the data is fixed.
	if _, after := e.releaseFails(t, second, "payments.bank_file_invalid"); after["status"] != "releasing" {
		t.Fatalf("after the refused file: %v", after)
	}
	if pay := e.payments(t, second)[0].(map[string]any); pay["journal_entry_id"] == nil {
		t.Fatalf("posting undone: %v", pay)
	}
	// Resumed after the fix: the posted payment is left alone, the file is written.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET holder_name = 'Vendor' WHERE partner_id = '%s'; END $$`, e.Vendor)) // the data fixed behind the immutability rule, as a correction would
	second, _ = e.Must(200, "GET", "/v1/payment-runs/"+id(second), "", "")
	released := e.release(t, second, "")
	if e.payments(t, released)[0].(map[string]any)["journal_entry_id"] == nil {
		t.Fatal("payment posted twice or lost")
	}
}

func TestListsAndAccess(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.invoice(t, e.Vendor, "INV-2", "50.00", "2026-03-01")
	first := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, first, "")
	none := e.NewSupplier(t, "BP90000003")
	e.invoice(t, none, "INV-3", "5.00", "2026-03-01")
	e.invoice(t, e.NewSupplier(t, "BP90000004"), "INV-4", "6.00", "2026-03-01")
	second := e.run(t, "2026-04-20", "2026-04-20")

	list := func(path string) map[string]any {
		t.Helper()
		page, _ := e.Must(200, "GET", path, "", "")
		return page
	}
	ids := func(page map[string]any) []string {
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, id(it.(map[string]any)))
		}
		return out
	}
	page := list("/v1/payment-runs?page_size=1")
	wantPageBankCodes(t, "payment run", page)
	if got, want := fmt.Sprint(ids(page)), fmt.Sprint([]string{id(first)}); got != want {
		t.Errorf("first page: got %s, want %s", got, want)
	}
	if page["next_page_token"] == nil {
		t.Fatalf("first page token: got none, want one: %v", page)
	}
	page = list("/v1/payment-runs?page_size=1&page_token=" + url.QueryEscape(page["next_page_token"].(string)))
	wantPageBankCodes(t, "payment run", page)
	if got, want := fmt.Sprint(ids(page)), fmt.Sprint([]string{id(second)}); got != want {
		t.Errorf("second page: got %s, want %s", got, want)
	}
	if page["next_page_token"] != nil {
		t.Errorf("second page token: got %v, want none", page["next_page_token"])
	}
	if got := ids(list("/v1/payment-runs?order_by=document_number%20desc&page_size=1")); fmt.Sprint(got) != fmt.Sprint([]string{id(second)}) {
		t.Fatalf("by number: %v", got)
	}
	page = list("/v1/payment-runs?order_by=document_number&page_size=1")
	if page["next_page_token"] == nil {
		t.Fatalf("by number, first page: %v", page)
	}
	if got := ids(list(fmt.Sprintf("/v1/payment-runs?status=released&legal_entity_id=%s&document_number=%s&run_date_from=2026-04-01&run_date_to=2026-04-15",
		e.DE, first["document_number"]))); fmt.Sprint(got) != fmt.Sprint([]string{id(first)}) {
		t.Fatalf("filtered: %v", got)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/payment-runs?order_by=total", "", "")

	pays := list("/v1/payment-runs/" + id(first) + "/payments?page_size=1")
	if got := len(pays["items"].([]any)); got != 1 {
		t.Errorf("payments page: got %d items, want 1", got)
	}
	if pays["next_page_token"] == nil {
		t.Fatalf("payments page token: got none, want one: %v", pays)
	}
	rest := list("/v1/payment-runs/" + id(first) + "/payments?page_size=1&page_token=" + url.QueryEscape(pays["next_page_token"].(string)))
	if got := len(rest["items"].([]any)); got != 1 {
		t.Errorf("payments second page: got %d items, want 1", got)
	}
	if rest["next_page_token"] != nil {
		t.Errorf("payments second page token: got %v, want none", rest["next_page_token"])
	}
	wantSupplierNames(t, "payment", pays, rest)
	wantItemNumbers(t, pays, rest)
	wantRequestNumbers(t, "payment", pays, rest)
	wantPaymentRefs(t, pays, rest)
	if got := list("/v1/payment-runs/" + id(first) + "/payments?supplier_id=" + e.Vendor.String()); len(got["items"].([]any)) != 1 {
		t.Fatalf("payments of the vendor: %v", got)
	}
	exs := list("/v1/payment-runs/" + id(second) + "/exclusions?page_size=1")
	if got := len(exs["items"].([]any)); got != 1 {
		t.Errorf("exclusions page: got %d items, want 1", got)
	}
	if exs["next_page_token"] == nil {
		t.Fatalf("exclusions page token: got none, want one")
	}
	exs2 := list("/v1/payment-runs/" + id(second) + "/exclusions?page_size=1&page_token=" + url.QueryEscape(exs["next_page_token"].(string)))
	wantSupplierNames(t, "exclusion", exs, exs2)
	wantRequestNumbers(t, "exclusion", exs, exs2)
	created := list("/v1/bank-files?status=created&page_size=1")
	if got := len(created["items"].([]any)); got != 1 {
		t.Errorf("created bank files: got %d, want 1", got)
	}
	if created["next_page_token"] != nil {
		t.Errorf("created bank files token: got %v, want none", created["next_page_token"])
	}
	if got := list("/v1/bank-files?status=transmitted"); len(got["items"].([]any)) != 0 {
		t.Fatalf("transmitted files: %v", got)
	}
	file := list("/v1/bank-files")["items"].([]any)[0].(map[string]any)

	// Two bank files page by creation time.
	e.invoice(t, e.Supplier, "INV-5", "7.00", "2026-03-01")
	third := e.run(t, "2026-04-21", "2026-04-21")
	e.release(t, third, "")
	files := list("/v1/bank-files?page_size=1")
	if files["next_page_token"] == nil {
		t.Fatalf("files page: %v", files)
	}
	more := list("/v1/bank-files?page_size=1&page_token=" + url.QueryEscape(files["next_page_token"].(string)))
	if len(more["items"].([]any)) != 1 {
		t.Fatalf("files second page: %v", more)
	}
	wantPageBankCodes(t, "bank file", created, files, more)

	// People without access to company 2000 see none of it.
	us := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	us.Subject = "human|" + us.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'US only', 'active', $2, $2)`, e.Tenant.ID, us.ID, pgtest.Issuer(t), us.Subject)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, e.Tenant.ID, us.ID)
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, e.Tenant.ID, us.ID, e.US)
	other := e.As(us)
	for _, path := range []string{"/v1/payment-runs/" + id(first), "/v1/payment-runs/" + id(first) + "/payments",
		"/v1/payment-runs/" + id(first) + "/exclusions", "/v1/bank-files/" + id(file)} {
		t.Run("get "+path, func(t *testing.T) {
			api := other
			api.T = t
			api.Problem(404, "request.not_found", "GET", path, "", "")
		})
	}
	for _, path := range []string{"/v1/payment-runs", "/v1/bank-files"} {
		t.Run("list "+path, func(t *testing.T) {
			api := other
			api.T = t
			if got, _ := api.Must(200, "GET", path, "", ""); len(got["items"].([]any)) != 0 {
				t.Errorf("got %v, want no items", got["items"])
			}
		})
	}
	other.Problem(422, "request.reference_not_found", "POST", "/v1/payment-runs:propose",
		fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q, "run_date": "2026-04-15", "due_by": "2026-04-15"}`, e.DE, e.bank), "")
}

// TestProposalTotalsOutOfRange: invoices the API accepts can add up to more
// than an amount holds, for one supplier or for the run; the proposal then
// fails with a problem rather than paying a wrapped total.
func TestProposalTotalsOutOfRange(t *testing.T) {
	const half = "50000000000000000.00" // 5·10^18 minor units: in range alone, not twice
	for _, tc := range []struct {
		name   string
		second func(t *testing.T, e env) uuid.UUID
	}{
		{"one supplier", func(_ *testing.T, e env) uuid.UUID { return e.Supplier }},
		{"the run", func(t *testing.T, e env) uuid.UUID {
			t.Helper()
			e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor AG", "2026-01-01", "", "create")
			return e.Vendor
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier GmbH", "2026-01-01", "", "create")
			e.invoice(t, e.Supplier, "INV-1", half, "2026-03-01")
			e.invoice(t, tc.second(t, e), "INV-2", half, "2026-03-01")
			op := e.propose(t, "2026-04-15", "2026-04-15")
			work(t)
			run, _ := e.Must(200, "GET", op["resource"].(string), "", "")
			done, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
			if run["status"] != "failed" {
				t.Fatalf("run status: got %v, want failed: total %v", run["status"], run["total_amount"])
			}
			if got := done["error"].(map[string]any)["code"]; got != "payments.amount_too_large" {
				t.Errorf("operation error: got %v, want payments.amount_too_large", got)
			}
		})
	}
}

// wantSupplierNames: every row on the pages names its supplier as the
// partner context holds it, on a later page as on the first.
func wantSupplierNames(t *testing.T, what string, pages ...map[string]any) {
	t.Helper()
	for _, page := range pages {
		for _, it := range page["items"].([]any) {
			row := it.(map[string]any)
			var name string
			if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT name FROM partner.business_partner WHERE id = $1`, row["supplier_id"]).Scan(&name); err != nil {
				t.Fatal(err)
			}
			if row["supplier_name"] != name {
				t.Errorf("%s of supplier %v: got name %v, want its partner's name %s", what, row["supplier_id"], row["supplier_name"], name)
			}
		}
	}
}

// wantRequestNumbers: a row on the pages (what: payment or exclusion)
// that names a down payment request gives the request's document number as
// the payables context holds it, and any other row gives none, on a later
// page as on the first.
func wantRequestNumbers(t *testing.T, what string, pages ...map[string]any) {
	t.Helper()
	for _, page := range pages {
		for _, it := range page["items"].([]any) {
			row := it.(map[string]any)
			request, ok := row["down_payment_request_id"]
			if !ok {
				if n, has := row["down_payment_request_number"]; has {
					t.Errorf("%s of supplier %v's items: got request number %v, want none", what, row["supplier_id"], n)
				}
				continue
			}
			want := documentNumber(t, `SELECT document_number FROM payables.down_payment_request WHERE id = $1`, request)
			if row["down_payment_request_number"] != want {
				t.Errorf("%s of request %v: got number %v, want its document number %s", what, request, row["down_payment_request_number"], want)
			}
		}
	}
}

// wantItemNumbers: every open item of the pages' payments gives its
// journal entry's document number as the ledger holds it, on a later page
// as on the first.
func wantItemNumbers(t *testing.T, pages ...map[string]any) {
	t.Helper()
	for _, page := range pages {
		for _, p := range page["items"].([]any) {
			for _, it := range p.(map[string]any)["items"].([]any) {
				item := it.(map[string]any)
				want := documentNumber(t, `SELECT document_number FROM ledger.journal_entry WHERE id = $1`, item["journal_entry_id"])
				if item["document_number"] != want {
					t.Errorf("item of entry %v: got document number %v, want %s", item["journal_entry_id"], item["document_number"], want)
				}
			}
		}
	}
}

// documentNumber is the number query reads for the document id.
func documentNumber(t *testing.T, query string, id any) string {
	t.Helper()
	var number string
	if err := pgtest.Admin(t).QueryRow(context.Background(), query, id).Scan(&number); err != nil {
		t.Fatal(err)
	}
	return number
}

// wantPaymentRefs: every payment on the pages names its posting by the
// entry's document number exactly when it is posted, its house bank by
// its code, and its bank account as the partner context holds it, masked,
// on a later page as on the first.
func wantPaymentRefs(t *testing.T, pages ...map[string]any) {
	t.Helper()
	for _, page := range pages {
		for _, it := range page["items"].([]any) {
			p := it.(map[string]any)
			apitest.WantNamed(t, "payment", p, "journal_entry_id", "journal_entry_number", `SELECT document_number FROM ledger.journal_entry WHERE id = $1`)
			apitest.WantNamed(t, "payment", p, "house_bank_id", "house_bank_code", apitest.HouseBankCode)
			wantBankAccount(t, p)
		}
	}
}

// wantBankAccount: payment p shows its bank account's masked fields as the
// partner context holds them, an optional one absent when the account has
// none, and never the account number.
func wantBankAccount(t *testing.T, p map[string]any) {
	t.Helper()
	var last4, scheme, holder string
	var bic, institution, transit *string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT account_last4, account_scheme, holder_name, bic, institution_number, transit_number
		FROM partner.bank_account WHERE id = $1`, p["bank_account_id"]).Scan(&last4, &scheme, &holder, &bic, &institution, &transit); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{"bank_account_last4": last4, "bank_account_scheme": scheme, "bank_account_holder_name": holder,
		"bank_account_bic": text(bic), "bank_account_institution_number": text(institution), "bank_account_transit_number": text(transit)} {
		if got := p[field]; got != want {
			t.Errorf("payment %v: got %s %v, want %v", p["id"], field, got, want)
		}
	}
}

// text is v as a decoded JSON response gives it: nil for an absent field.
func text(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
