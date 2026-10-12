package payments_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(&connector{})
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.credit(t, e.Supplier, "10.00", "2026-03-01")
	released := e.run(t, "2026-04-15", "2026-04-15")
	e.decide(t, 200, released, "release", "")
	files, _ := e.Must(200, "GET", "/v1/bank-files", "", "")
	file := uuidOf(id(files["items"].([]any)[0].(map[string]any)))
	transmitted := func(number, invoice string) uuid.UUID { // a released run of one invoice, its file transmitted
		s := e.NewSupplier(t, number)
		e.bankAccount(t, s, "DE89370400440532013000", "S", "2026-01-01", "", "create")
		e.invoice(t, s, invoice, "7.00", "2026-03-01")
		r := e.run(t, "2026-04-15", "2026-04-15")
		e.decide(t, 200, r, "release", "")
		fs, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(r), "", "")
		f := id(fs["items"].([]any)[0].(map[string]any))
		e.Must(202, "POST", "/v1/bank-files/"+f+":transmit", "", `"1"`)
		return uuidOf(f)
	}
	toAck, toReject := transmitted("BP90000004", "INV-4"), transmitted("BP90000005", "INV-5")
	e.invoice(t, e.Vendor, "INV-2", "50.00", "2026-03-01")
	e.invoice(t, e.NewSupplier(t, "BP90000003"), "INV-3", "5.00", "2026-03-01")
	proposed := e.run(t, "2026-04-16", "2026-04-16")
	run := uuidOf(id(proposed))
	e.Must(202, "POST", "/v1/payment-runs:propose", `{"legal_entity_id": "`+e.DE.String()+`", "house_bank_id": "`+e.bank.String()+`",
		"run_date": "2026-04-17", "due_by": "2026-04-17"}`, "")
	cancelled := e.propose(t, "2026-04-18", "2026-04-18")
	e.Must(200, "POST", "/v1/operations/"+id(cancelled)+":cancel", "", `"1"`)
	var queued, gone payments.Job
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx)
		j, _, err := payments.Next(ctx, tx)
		queued = j
		return err
	}); err != nil || queued.Cancelled {
		t.Fatalf("next job: %+v %v", queued, err)
	}
	gone = payments.Job{RunID: uuidOf(cancelled["resource"].(string)[len("/v1/payment-runs/"):]), Cancelled: true, ProposedBy: e.Tenant.Human.ID,
		ProposerKind: "human"}
	inv, _ := e.Must(200, "GET", "/v1/supplier-invoices?supplier_id="+e.Vendor.String(), "", "")
	vendorEntry := uuidOf(inv["items"].([]any)[0].(map[string]any)["journal_entry_id"].(string))
	yes := func(int32) error { return nil }

	calls := map[string]func(ctx context.Context) error{
		"propose": func(ctx context.Context) error {
			_, err := h.ProposePaymentRun(ctx, &gen.PaymentRunPropose{LegalEntityID: e.DE, HouseBankID: e.bank, RunDate: date("2026-04-20"),
				DueBy: date("2026-04-20")}, gen.ProposePaymentRunParams{})
			return err
		},
		"build": func(ctx context.Context) error { return h.Process(ctx, queued) },
		"fail cancelled": func(ctx context.Context) error {
			return h.Process(ctx, gone)
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetPaymentRun(ctx, gen.GetPaymentRunParams{ID: run})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListPaymentRuns(ctx, gen.ListPaymentRunsParams{})
			return err
		},
		"payments": func(ctx context.Context) error {
			_, err := h.ListPayments(ctx, gen.ListPaymentsParams{ID: run})
			return err
		},
		"exclusions": func(ctx context.Context) error {
			_, err := h.ListPaymentExclusions(ctx, gen.ListPaymentExclusionsParams{ID: run})
			return err
		},
		"bank file": func(ctx context.Context) error {
			_, err := h.GetBankFile(ctx, gen.GetBankFileParams{ID: file})
			return err
		},
		"bank files": func(ctx context.Context) error {
			_, err := h.ListBankFiles(ctx, gen.ListBankFilesParams{})
			return err
		},
		"transmit": func(ctx context.Context) error {
			_, err := h.TransmitBankFile(ctx, gen.TransmitBankFileParams{ID: file, IfMatch: "*"})
			return err
		},
		"report accepted": func(ctx context.Context) error {
			_, err := h.ReportBankFileStatus(ctx, &gen.BankStatusReport{Outcome: "accepted"}, gen.ReportBankFileStatusParams{ID: toAck, IfMatch: "*"})
			return err
		},
		"report rejected": func(ctx context.Context) error {
			_, err := h.ReportBankFileStatus(ctx, &gen.BankStatusReport{Outcome: "rejected", Reference: gen.NewOptString("pain.002-0002"),
				Reason: gen.NewOptString("AC01 Incorrect account number")}, gen.ReportBankFileStatusParams{ID: toReject, IfMatch: "*"})
			return err
		},
		"in run": func(ctx context.Context) error {
			_, _, err := payments.Reservations{}.RunOf(ctx, 2026, vendorEntry)
			return err
		},
		"check house banks": func(ctx context.Context) error {
			_, err := houseBanks().Check(ctx, nil, fixture.Document(t))
			return err
		},
		"apply house banks": func(ctx context.Context) error { return houseBanks().Apply(ctx, fixture.Document(t)) },
		"payees": func(ctx context.Context) error {
			_, err := partner.Payees{Keys: apitest.FieldKeys()}.Payees(ctx, e.DE, []uuid.UUID{e.Supplier}, date("2026-04-15"))
			return err
		},
		"bank details": func(ctx context.Context) error {
			_, err := partner.Payees{Keys: apitest.FieldKeys()}.BankDetails(ctx, []uuid.UUID{uuid.New()})
			return err
		},
	}
	decisions := map[string]func(ctx context.Context) error{
		"release": func(ctx context.Context) error {
			_, err := h.ReleasePaymentRun(ctx, gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString("ok")}),
				gen.ReleasePaymentRunParams{ID: run, IfMatch: "*"})
			return err
		},
		"reject": func(ctx context.Context) error {
			_, err := h.RejectPaymentRun(ctx, &gen.ReasonInput{Reason: "r"}, gen.RejectPaymentRunParams{ID: run, IfMatch: "*"})
			return err
		},
	}
	for as, set := range map[*pgtest.Actor]map[string]func(ctx context.Context) error{&e.Tenant.Human: calls, &e.Approver: decisions} {
		for name, call := range set {
			if err := call(context.Background()); err == nil {
				t.Errorf("%s outside a unit of work", name)
			}
			apitest.EveryFailure(t, e.Tenant, *as, name, call)
		}
	}

	// A run that fails while building records the problem on its operation.
	broken := setup(t)
	op := broken.propose(t, "2026-04-15", "2026-04-15")
	pgtest.Exec(t, `DELETE FROM config.active_version WHERE tenant_id = $1`, broken.Tenant.ID)
	job := payments.Job{RunID: uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):]), ProposedBy: broken.Tenant.Human.ID, ProposerKind: "human"}
	apitest.EveryFailure(t, broken.Tenant, broken.Tenant.Human, "fail", func(ctx context.Context) error { return h.Process(ctx, job) })
	work(t)

	for name, call := range map[string]func(ctx context.Context) error{
		"list order": func(ctx context.Context) error {
			_, err := h.ListPaymentRuns(ctx, gen.ListPaymentRunsParams{OrderBy: gen.NewOptString("total")})
			return err
		},
		"payments page size": func(ctx context.Context) error {
			_, err := h.ListPayments(ctx, gen.ListPaymentsParams{ID: run, PageSize: gen.NewOptInt32(0)})
			return err
		},
		"payments of an unknown run": func(ctx context.Context) error {
			_, err := h.ListPayments(ctx, gen.ListPaymentsParams{ID: uuid.New()})
			return err
		},
		"exclusions page size": func(ctx context.Context) error {
			_, err := h.ListPaymentExclusions(ctx, gen.ListPaymentExclusionsParams{ID: run, PageSize: gen.NewOptInt32(0)})
			return err
		},
		"exclusions of an unknown run": func(ctx context.Context) error {
			_, err := h.ListPaymentExclusions(ctx, gen.ListPaymentExclusionsParams{ID: uuid.New()})
			return err
		},
		"files order": func(ctx context.Context) error {
			_, err := h.ListBankFiles(ctx, gen.ListBankFilesParams{OrderBy: gen.NewOptString("status")})
			return err
		},
		"unknown file": func(ctx context.Context) error {
			_, err := h.TransmitBankFile(ctx, gen.TransmitBankFileParams{ID: uuid.New(), IfMatch: "*"})
			return err
		},
		"propose unknown": func(ctx context.Context) error {
			_, err := h.ProposePaymentRun(ctx, &gen.PaymentRunPropose{LegalEntityID: uuid.New(), HouseBankID: e.bank}, gen.ProposePaymentRunParams{})
			return err
		},
		"release mismatch": func(ctx context.Context) error {
			_, err := h.ReleaseRun(ctx, run, func(int32) error { return errMismatch }, "")
			return err
		},
		"reject mismatch": func(ctx context.Context) error {
			_, err := payments.RejectRun(ctx, run, func(int32) error { return errMismatch }, "r")
			return err
		},
		"release unknown": func(ctx context.Context) error {
			_, err := h.ReleaseRun(ctx, uuid.New(), yes, "")
			return err
		},
		"reject unknown": func(ctx context.Context) error {
			_, err := payments.RejectRun(ctx, uuid.New(), yes, "r")
			return err
		},
	} {
		if err := apitest.InRequest(t, e.Tenant, e.Approver, call); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
