package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// downPaymentSetup is setup with the permission to request down payments
// and company 2000's down payment account, 159000 (ADR-0037).
func downPaymentSetup(t *testing.T) env {
	t.Helper()
	return downPaymentSetupWith(t, func(*app.Config) {})
}

// downPaymentSetupWith is downPaymentSetup with more API wiring.
func downPaymentSetupWith(t *testing.T, configure func(*app.Config)) env {
	t.Helper()
	e := setupWith(t, []string{"payables.down_payment.request"}, configure)
	doc := fixture.Document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// request requests a down payment from the supplier in company 2000, due
// on the date, with extra fields of the body.
func (e env) request(t *testing.T, supplier uuid.UUID, amount, due, extra string) map[string]any {
	t.Helper()
	r, _ := e.Must(201, "POST", "/v1/down-payment-requests", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q,
		"amount": {"amount": %q, "currency": "EUR"}, "due_date": %q%s}`, e.DE, supplier, amount, due, extra), "")
	return r
}

// byKind are a run's payments by supplier and kind.
func byKind(pays []any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, p := range pays {
		m := p.(map[string]any)
		out[m["supplier_id"].(string)+" "+m["kind"].(string)] = m
	}
	return out
}

// TestDownPaymentPayments: a run pays each due down payment request as a
// payment of its own, whole and never netted with the supplier's items
// (T46c, ADR-0037); the release posts it as the supplier's special G/L
// item A on the down payment account, with its input tax, and the request
// is paid. A request in a live run is not cancelled; one of a supplier
// that cannot be paid is excluded with it, and one of a supplier whose
// items are a debit balance is still paid.
func TestDownPaymentPayments(t *testing.T) {
	e := downPaymentSetup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "500.00", "2026-03-01")
	taxed := e.request(t, e.Supplier, "1190.00", "2026-03-15", `, "tax_code": "V1", "reference": "AB-17"`)
	debit := e.request(t, e.Vendor, "300.00", "2026-03-20", "")
	e.credit(t, e.Vendor, "25.00", "2026-03-01")
	blocked := e.NewSupplier(t, "BP90000005")
	e.bankAccount(t, blocked, "DE89370400440532013000", "B", "2026-01-01", "", "create")
	blockedRequest := e.request(t, blocked, "70.00", "2026-03-10", "")
	e.invoice(t, blocked, "B-1", "10.00", "2026-03-01")
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1 WHERE id = $1`, blocked)
	e.request(t, e.Supplier, "50.00", "2026-05-01", "") // due after the run's due-by date

	run := e.run(t, "2026-03-31", "2026-03-31")
	pays := byKind(e.payments(t, run))
	if len(pays) != 3 {
		t.Fatalf("payments: got %v, want the supplier's items, its request and the vendor's request", pays)
	}
	for key, want := range map[string]string{e.Supplier.String() + " items": "500.00", e.Supplier.String() + " down_payment": "1190.00",
		e.Vendor.String() + " down_payment": "300.00"} {
		t.Run("payment "+key, func(t *testing.T) {
			if got := pays[key]["amount"].(map[string]any)["amount"]; got != want {
				t.Errorf("amount: got %v, want %s", got, want)
			}
		})
	}
	if got := pays[e.Supplier.String()+" down_payment"]["down_payment_request_id"]; got != id(taxed) {
		t.Errorf("request paid: got %v, want %s", got, id(taxed))
	}
	wantEx := map[string]string{e.Vendor.String(): "debit_balance -25.00", blocked.String(): "supplier_blocked 10.00",
		"request " + id(blockedRequest): "supplier_blocked 70.00"}
	if got := e.exclusions(t, run); fmt.Sprint(got) != fmt.Sprint(wantEx) {
		t.Errorf("exclusions: got %v, want %v", got, wantEx)
	}
	if got := run["total_amount"].(map[string]any)["amount"]; got != "1990.00" {
		t.Errorf("run total: got %v, want 1990.00", got)
	}
	e.Problem(409, "payables.request_in_payment_run", "POST", "/v1/down-payment-requests/"+id(taxed)+":cancel", `{"reason": "x"}`, `"1"`)

	released := e.release(t, run, "")
	postAgain(t, e, released, pays[e.Supplier.String()+" down_payment"])
	paid, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(taxed), "", "")
	if paid["status"] != "paid" {
		t.Fatalf("request after the release: got %v, want paid", paid["status"])
	}
	checkDownPaymentEntry(t, e, paid["journal_entry_id"].(string))
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	e.transmit(t, id(files["items"].([]any)[0].(map[string]any)), `"1"`)
	if want := "<Ustrd>Down payment " + paid["document_number"].(string) + " AB-17</Ustrd>"; !strings.Contains(string(e.connector.files[0].Content), want) {
		t.Errorf("bank file: got\n%s\nwant it to contain %s", e.connector.files[0].Content, want)
	}
	if ev := apitest.Published(t, e.Tenant, outbox.Event{Context: "payables", Name: "down_payment_request_paid"}); len(ev) != 2 {
		t.Errorf("DownPaymentRequestPaid: got %d events, want 2", len(ev))
	}
	if got, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(debit), "", ""); got["status"] != "paid" {
		t.Errorf("debit balance supplier's request: got %v, want paid", got["status"])
	}
	again := e.run(t, "2026-04-30", "2026-04-30")
	if pays := e.payments(t, again); len(pays) != 0 {
		t.Errorf("next run: got %v, want no payments for paid requests", pays)
	}
}

// checkDownPaymentEntry checks a down payment's entry: Dr 1000.00 on the
// down payment account as special G/L item A, Dr 190.00 input tax V1, Cr
// 1190.00 bank.
func checkDownPaymentEntry(t *testing.T, e env, entry string) {
	t.Helper()
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+entry, "", "")
	lines := je["lines"].([]any)
	if len(lines) != 3 {
		t.Fatalf("down payment entry lines: got %d, want 3", len(lines))
	}
	for i, want := range []struct{ account, amount, special, tax string }{
		{e.Account(t, "2000", "159000").String(), "1000.00", "A", ""},
		{e.Account(t, "2000", "140000").String(), "190.00", "", "V1"},
		{"", "-1190.00", "", ""},
	} {
		t.Run(fmt.Sprintf("line %d", i+1), func(t *testing.T) {
			l := lines[i].(map[string]any)
			if want.account != "" && l["account_id"] != want.account {
				t.Errorf("account: got %v, want %s", l["account_id"], want.account)
			}
			if got := l["amount"].(map[string]any)["amount"]; got != want.amount {
				t.Errorf("amount: got %v, want %s", got, want.amount)
			}
			if got, _ := l["special_gl"].(string); got != want.special {
				t.Errorf("special G/L: got %q, want %q", got, want.special)
			}
			if got, _ := l["tax_code"].(string); got != want.tax {
				t.Errorf("tax code: got %q, want %q", got, want.tax)
			}
		})
	}
}

// TestRejectFreesRequests: a rejected run no longer pays its requests, so
// one can be cancelled, and another be paid by the next run.
func TestRejectFreesRequests(t *testing.T) {
	e := downPaymentSetup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	first := e.request(t, e.Supplier, "100.00", "2026-03-15", "")
	second := e.request(t, e.Supplier, "200.00", "2026-03-15", "")
	run := e.run(t, "2026-03-31", "2026-03-31")
	if other := e.run(t, "2026-04-01", "2026-04-01"); len(e.payments(t, other)) != 0 {
		t.Errorf("second run: got payments, want none for requests the first run pays")
	}
	e.decide(t, 200, run, "reject", `{"reason": "Wrong date"}`)
	e.Must(200, "POST", "/v1/down-payment-requests/"+id(first)+":cancel", `{"reason": "Order cancelled"}`, `"1"`)
	next := e.run(t, "2026-04-02", "2026-04-02")
	pays := e.payments(t, next)
	if len(pays) != 1 {
		t.Fatalf("next run: got %d payments, want the open request's", len(pays))
	}
	if got := pays[0].(map[string]any)["down_payment_request_id"]; got != id(second) {
		t.Errorf("next run pays: got %v, want %s", got, id(second))
	}
}

// postAgain posts a released run's payment again, as a retried release
// step does: nothing is posted twice.
func postAgain(t *testing.T, e env, run, pay map[string]any) {
	t.Helper()
	err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(run)))
		if err != nil {
			return err
		}
		posted, err := endpoints(nil).PostPayment(ctx, r, uuidOf(id(pay)))
		if posted {
			t.Error("posting a posted down payment again: got posted, want nothing posted")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDownPaymentDatabaseFailures fails each database statement in turn of
// the steps that pay down payment requests: a proposal with requests, the
// run a request is in, a request payment's posting and a release that
// writes it to the bank file.
func TestDownPaymentDatabaseFailures(t *testing.T) {
	e := downPaymentSetup(t)
	workLeftovers(t)
	h := endpoints(nil)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.request(t, e.Vendor, "30.00", "2026-03-15", `, "reference": "V-1"`)
	finishing := e.run(t, "2026-03-31", "2026-03-31")
	e.decide(t, 202, finishing, "release", "")
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(finishing)))
		if err != nil {
			return err
		}
		_, err = h.PostPayment(ctx, r, uuidOf(id(e.payments(t, finishing)[0].(map[string]any))))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pending := e.request(t, e.Supplier, "119.00", "2026-03-15", `, "tax_code": "V1"`)
	releasing := e.run(t, "2026-04-01", "2026-04-01")
	e.decide(t, 202, releasing, "release", "")
	pay := uuidOf(id(e.payments(t, releasing)[0].(map[string]any)))
	e.request(t, e.Vendor, "40.00", "2026-03-15", `, "tax_code": "V1"`)
	proposedRun := e.run(t, "2026-04-01", "2026-04-01")
	e.request(t, e.Supplier, "23.80", "2026-03-15", `, "tax_code": "V1"`)
	queued := e.propose(t, "2026-04-02", "2026-04-02")
	var job payments.Job
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx)
		j, _, err := h.NextJob(ctx, tx)
		job = j
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if job.RunID.String() != queued["resource"].(string)[len("/v1/payment-runs/"):] {
		t.Fatalf("queued job: got run %s, want %s", job.RunID, queued["resource"])
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"build with requests": func(ctx context.Context) error { return h.Process(ctx, job) },
		"request run of": func(ctx context.Context) error {
			_, _, err := payments.Reservations{}.RequestRunOf(ctx, e.DE, uuidOf(id(pending)))
			return err
		},
		"post a down payment": func(ctx context.Context) error {
			r, err := payments.GetRun(ctx, uuidOf(id(releasing)))
			if err != nil {
				return err
			}
			_, err = h.PostPayment(ctx, r, pay)
			return err
		},
		"release a run with a request": func(ctx context.Context) error {
			_, err := h.StartRelease(ctx, uuidOf(id(proposedRun)), yes, "")
			return err
		},
		"finish with a down payment": func(ctx context.Context) error {
			r, err := payments.GetRun(ctx, uuidOf(id(finishing)))
			if err != nil {
				return err
			}
			_, err = h.FinishRelease(ctx, r)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Approver, name, call)
		})
	}
}

// operationError is the problem code a proposal's operation failed with.
func (e env) operationError(t *testing.T, op map[string]any) any {
	t.Helper()
	failed, _ := e.Must(200, "GET", "/v1/operations/"+id(op), "", "")
	why, _ := failed["error"].(map[string]any)
	return why["code"]
}

// TestDownPaymentRefusals: a run that pays a request fails at proposal in
// a company without a down payment account; a proposed run whose request's
// tax code has ended since is refused at release, to be rejected, and the
// next run leaves the request out (tax_code_invalid) and pays the
// supplier's items.
func TestDownPaymentRefusals(t *testing.T) {
	t.Run("no down payment account", func(t *testing.T) {
		e := setupWith(t, []string{"payables.down_payment.request"}, func(*app.Config) {})
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		e.request(t, e.Supplier, "100.00", "2026-03-15", "")
		op := e.propose(t, "2026-03-31", "2026-03-31")
		work(t)
		if got := e.operationError(t, op); got != "payments.no_down_payment_account" {
			t.Errorf("proposal: got %v, want payments.no_down_payment_account", got)
		}
	})
	t.Run("too large", func(t *testing.T) {
		e := downPaymentSetup(t)
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		e.request(t, e.Supplier, "50000000000000000.00", "2026-03-15", "")
		e.request(t, e.Supplier, "50000000000000000.00", "2026-03-15", "")
		op := e.propose(t, "2026-03-31", "2026-03-31")
		work(t)
		if got := e.operationError(t, op); got != "payments.amount_too_large" {
			t.Errorf("proposal: got %v, want payments.amount_too_large", got)
		}
	})
	t.Run("tax code ended", func(t *testing.T) {
		e := downPaymentSetup(t)
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		taxed := e.request(t, e.Supplier, "119.00", "2026-03-15", `, "tax_code": "V1"`)
		run := e.run(t, "2026-03-31", "2026-03-31")
		// The run dates are in the past, where a tax code's validity is not
		// ended by the application (its trigger); the test ends it there.
		pgtest.Exec(t, `ALTER TABLE ledger.tax_code DISABLE TRIGGER USER`)
		pgtest.Exec(t, `UPDATE ledger.tax_code SET valid_to = '2026-03-20' WHERE tenant_id = $1 AND legal_entity_id = $2 AND code = 'V1'`, e.Tenant.ID, e.DE)
		pgtest.Exec(t, `ALTER TABLE ledger.tax_code ENABLE TRIGGER USER`)
		_, tag := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
		e.As(e.Approver).Problem(409, "payments.proposal_outdated", "POST", "/v1/payment-runs/"+id(run)+":release", "", tag)
		e.decide(t, 200, run, "reject", `{"reason": "Tax code ended"}`)
		e.invoice(t, e.Supplier, "INV-1", "40.00", "2026-03-01")
		next := e.run(t, "2026-04-01", "2026-04-01")
		want := map[string]string{"request " + id(taxed): "tax_code_invalid 119.00"}
		if got := e.exclusions(t, next); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("exclusions: got %v, want %v", got, want)
		}
		pays := byKind(e.payments(t, next))
		if _, ok := pays[e.Supplier.String()+" items"]; len(pays) != 1 || !ok {
			t.Fatalf("payments: got %v, want the supplier's items only", pays)
		}
		e.release(t, next, "")
		if got, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(taxed), "", ""); got["status"] != "open" {
			t.Errorf("request left out: got %v, want open", got["status"])
		}
	})
}

// TestDropBesideAnExcludedRequest: a release that drops a supplier's items
// payment (its invoice reversed since the proposal) records its exclusion
// beside the one of the supplier's request left out for its tax code, and
// releases the run with the payments that stand.
func TestDropBesideAnExcludedRequest(t *testing.T) {
	e := downPaymentSetup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-A", "100.00", "2026-03-01")
	x := e.invoice(t, e.Vendor, "INV-X", "40.00", "2026-03-01")
	taxed := e.request(t, e.Vendor, "119.00", "2026-03-15", `, "tax_code": "V1"`)
	pgtest.Exec(t, `ALTER TABLE ledger.tax_code DISABLE TRIGGER USER`) // the run date is in the past, where the application ends no validity
	pgtest.Exec(t, `UPDATE ledger.tax_code SET valid_to = '2026-03-20' WHERE tenant_id = $1 AND legal_entity_id = $2 AND code = 'V1'`, e.Tenant.ID, e.DE)
	pgtest.Exec(t, `ALTER TABLE ledger.tax_code ENABLE TRIGGER USER`)
	run := e.run(t, "2026-04-15", "2026-04-15")
	e.decide(t, 202, run, "release", "")
	e.unpayable(t, x)
	if _, err := e.releaser(t).RunOnce(context.Background()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
	if got["status"] != "released" {
		t.Fatalf("run status: got %v, want released", got["status"])
	}
	want := map[string]string{e.Vendor.String(): "item_reversed 40.00", "request " + id(taxed): "tax_code_invalid 119.00"}
	if exs := e.exclusions(t, run); fmt.Sprint(exs) != fmt.Sprint(want) {
		t.Errorf("exclusions: got %v, want %v", exs, want)
	}
	if pays := e.payments(t, run); len(pays) != 1 {
		t.Errorf("payments listed: got %d, want the supplier's alone", len(pays))
	}
	if got, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+id(taxed), "", ""); got["status"] != "open" {
		t.Errorf("request left out: got %v, want open", got["status"])
	}
	if ev := events(t, e.Tenant, "payment_excluded"); len(ev) != 1 {
		t.Errorf("PaymentExcluded: got %d events, want 1, the dropped items payment's", len(ev))
	}
}
