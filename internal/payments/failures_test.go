package payments_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// failureRuns are the runs and files TestDatabaseFailures calls the
// handlers on, one in each state a handler reads.
type failureRuns struct {
	file, toAck, toReject  uuid.UUID // a released run's bank file; two transmitted files
	relA, relB, relC, relD map[string]any
	relAID, relBID         uuid.UUID
	relCID, relDID         uuid.UUID
	payA, payC             uuid.UUID
	run                    uuid.UUID // a proposed run
	queued, gone           payments.Job
	vendorEntry            uuid.UUID // the journal entry of an invoice in no run
}

// TestDatabaseFailures calls every handler and context function outside a
// unit of work and with each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(&connector{})
	var s failureRuns
	if !t.Run("released and transmitted runs", func(t *testing.T) { s.released(t, e.in(t)) }) {
		t.FailNow()
	}
	if !t.Run("runs under release", func(t *testing.T) { s.underRelease(t, e.in(t), h) }) {
		t.FailNow()
	}
	if !t.Run("outside a unit of work", func(t *testing.T) {
		for name, err := range map[string]error{
			"post payment":   func() error { _, err := h.PostPayment(context.Background(), payments.Run{}, s.payC); return err }(),
			"finish release": func() error { _, err := h.FinishRelease(context.Background(), payments.Run{}); return err }(),
			"fail release": func() error {
				_, err := h.FailRelease(context.Background(), payments.Run{}, problem.Conflict("x", "y"))
				return err
			}(),
		} {
			t.Run(name, func(t *testing.T) {
				if err == nil {
					t.Error("outside a unit of work: got nil error, want an error")
				}
			})
		}
	}) {
		t.FailNow()
	}
	if !t.Run("proposed, queued and cancelled runs", func(t *testing.T) { s.proposed(t, e.in(t), h) }) {
		t.FailNow()
	}
	if !t.Run("every statement failing", func(t *testing.T) {
		calls, decisions := failureCalls(t, e, h, &s)
		for as, set := range map[*pgtest.Actor]map[string]func(ctx context.Context) error{&e.Tenant.Human: calls, &e.Approver: decisions} {
			for name, call := range set {
				t.Run(name, func(t *testing.T) {
					if err := call(context.Background()); err == nil {
						t.Error("outside a unit of work: got nil error, want an error")
					}
					apitest.EveryFailure(t, e.Tenant, *as, name, call)
				})
			}
		}
	}) {
		t.FailNow()
	}
	if !t.Run("a failing build records its problem", func(t *testing.T) {
		broken := setup(t)
		op := broken.propose(t, "2026-04-15", "2026-04-15")
		pgtest.Exec(t, `DELETE FROM config.active_version WHERE tenant_id = $1`, broken.Tenant.ID)
		job := payments.Job{RunID: uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):]), ProposedBy: broken.Tenant.Human.ID, ProposerKind: "human"}
		apitest.EveryFailure(t, broken.Tenant, broken.Tenant.Human, "fail", func(ctx context.Context) error { return h.Process(ctx, job) })
		work(t)
	}) {
		t.FailNow()
	}
	t.Run("refusals", func(t *testing.T) {
		for name, call := range refusalCalls(e, h, s.run) {
			t.Run(name, func(t *testing.T) {
				if err := apitest.InRequest(t, e.Tenant, e.Approver, call); err == nil {
					t.Error("got nil error, want a refusal")
				}
			})
		}
	})
}

// released makes a released run with its bank file, and two runs whose
// files are transmitted: one to acknowledge, one to reject.
func (s *failureRuns) released(t *testing.T, e env) {
	t.Helper()
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-1", "100.00", "2026-03-01")
	e.credit(t, e.Supplier, "10.00", "2026-03-01")
	released := e.run(t, "2026-04-15", "2026-04-15")
	e.release(t, released, "")
	files, _ := e.Must(200, "GET", "/v1/bank-files", "", "")
	s.file = uuidOf(id(files["items"].([]any)[0].(map[string]any)))
	transmitted := func(number, invoice string) uuid.UUID { // a released run of one invoice, its file transmitted
		sup := e.NewSupplier(t, number)
		e.bankAccount(t, sup, "DE89370400440532013000", "S", "2026-01-01", "", "create")
		e.invoice(t, sup, invoice, "7.00", "2026-03-01")
		r := e.run(t, "2026-04-15", "2026-04-15")
		e.release(t, r, "")
		fs, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(r), "", "")
		f := id(fs["items"].([]any)[0].(map[string]any))
		e.transmit(t, f, `"1"`)
		return uuidOf(f)
	}
	s.toAck, s.toReject = transmitted("BP90000004", "INV-4"), transmitted("BP90000005", "INV-5")
}

// underRelease makes four runs under release: A has its payment posted and
// its release failed (to be resumed), B is releasing with nothing posted
// yet, C has its payment still to post, D has its payment posted and only
// the bank file left.
func (s *failureRuns) underRelease(t *testing.T, e env, h *payments.Endpoints) {
	t.Helper()
	e.invoice(t, e.Supplier, "INV-RA", "9.00", "2026-03-01")
	s.relA = e.run(t, "2026-04-21", "2026-04-21")
	e.decide(t, 202, s.relA, "release", "")
	s.payA = uuidOf(id(e.payments(t, s.relA)[0].(map[string]any)))
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, uuidOf(id(s.relA)))
		if err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, s.payA); err != nil {
			return err
		}
		if _, err := h.PostPayment(ctx, r, uuid.New()); err == nil || !strings.Contains(err.Error(), "has no payment") {
			return fmt.Errorf("unknown payment: %w", err)
		}
		_, err = h.FailRelease(ctx, r, problem.Conflict("payments.bank_file_invalid", "test"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(s.relA), "", ""); got["status"] != "releasing" {
		t.Fatalf("run A after a failed release with a posting: %v", got)
	}
	sB := e.NewSupplier(t, "BP90000006")
	e.bankAccount(t, sB, "DE89370400440532013000", "B", "2026-01-01", "", "create")
	e.invoice(t, sB, "INV-RB", "4.00", "2026-03-01")
	s.relB = e.run(t, "2026-04-22", "2026-04-22")
	e.decide(t, 202, s.relB, "release", "")
	sC := e.NewSupplier(t, "BP90000007")
	e.bankAccount(t, sC, "DE89370400440532013000", "C", "2026-01-01", "", "create")
	e.invoice(t, sC, "INV-RC", "6.00", "2026-03-01")
	s.relC = e.run(t, "2026-04-23", "2026-04-23") // releasing, its payment still to post
	e.decide(t, 202, s.relC, "release", "")
	s.payC = uuidOf(id(e.payments(t, s.relC)[0].(map[string]any)))
	s.relCID = uuidOf(id(s.relC))
	sD := e.NewSupplier(t, "BP90000008")
	e.bankAccount(t, sD, "DE89370400440532013000", "D", "2026-01-01", "", "create")
	e.invoice(t, sD, "INV-RD", "8.00", "2026-03-01")
	s.relD = e.run(t, "2026-04-24", "2026-04-24")
	e.decide(t, 202, s.relD, "release", "")
	var payD uuid.UUID
	s.relDID, payD = uuidOf(id(s.relD)), uuidOf(id(e.payments(t, s.relD)[0].(map[string]any)))
	if err := apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		r, err := payments.GetRun(ctx, s.relDID)
		if err != nil {
			return err
		}
		_, err = h.PostPayment(ctx, r, payD)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// proposed makes a proposed run, a queued job and a cancelled one, and an
// invoice in no run; a proposed run has no releaser and B cannot finish
// before its payment posts.
func (s *failureRuns) proposed(t *testing.T, e env, h *payments.Endpoints) {
	t.Helper()
	e.invoice(t, e.Vendor, "INV-2", "50.00", "2026-03-01")
	e.invoice(t, e.NewSupplier(t, "BP90000003"), "INV-3", "5.00", "2026-03-01")
	proposed := e.run(t, "2026-04-16", "2026-04-16")
	s.run = uuidOf(id(proposed))
	e.Must(202, "POST", "/v1/payment-runs:propose", `{"legal_entity_id": "`+e.DE.String()+`", "house_bank_id": "`+e.bank.String()+`",
		"run_date": "2026-04-17", "due_by": "2026-04-17"}`, "")
	cancelled := e.propose(t, "2026-04-18", "2026-04-18")
	e.Must(200, "POST", "/v1/operations/"+id(cancelled)+":cancel", "", `"1"`)
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx)
		j, _, err := h.NextJob(ctx, tx)
		s.queued = j
		return err
	}); err != nil {
		t.Fatalf("next job: %v", err)
	}
	if s.queued.Cancelled {
		t.Fatalf("next job: got a cancelled job, want a live one: %+v", s.queued)
	}
	s.gone = payments.Job{RunID: uuidOf(cancelled["resource"].(string)[len("/v1/payment-runs/"):]), Cancelled: true, ProposedBy: e.Tenant.Human.ID,
		ProposerKind: "human"}
	inv, _ := e.Must(200, "GET", "/v1/supplier-invoices?supplier_id="+e.Vendor.String(), "", "")
	s.vendorEntry = uuidOf(inv["items"].([]any)[0].(map[string]any)["journal_entry_id"].(string))
	if err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx)
		if _, _, err := h.Releaser(ctx, tx, s.run); err == nil || !strings.Contains(err.Error(), "not releasing") {
			return fmt.Errorf("releaser of a proposed run: %w", err)
		}
		r, err := payments.GetRun(ctx, uuidOf(id(s.relB)))
		if err != nil {
			return err
		}
		if _, err := h.FinishRelease(ctx, r); err == nil || !strings.Contains(err.Error(), "not posted yet") {
			return fmt.Errorf("finish before posting: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.relAID, s.relBID = uuidOf(id(s.relA)), uuidOf(id(s.relB))
}

func yes(int32) error { return nil }

// failureCalls are the calls TestDatabaseFailures fails statement by
// statement: as the requester, and the decisions as the approver.
func failureCalls(t *testing.T, e env, h *payments.Endpoints, s *failureRuns) (calls, decisions map[string]func(ctx context.Context) error) {
	t.Helper()
	calls = map[string]func(ctx context.Context) error{
		"propose": func(ctx context.Context) error {
			_, err := h.ProposePaymentRun(ctx, &gen.PaymentRunPropose{LegalEntityID: e.DE, HouseBankID: gen.NewOptUUID(e.bank), RunDate: date("2026-04-20"),
				DueBy: date("2026-04-20")}, gen.ProposePaymentRunParams{})
			return err
		},
		"build": func(ctx context.Context) error { return h.Process(ctx, s.queued) },
		"fail cancelled": func(ctx context.Context) error {
			return h.Process(ctx, s.gone)
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetPaymentRun(ctx, gen.GetPaymentRunParams{ID: s.run})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListPaymentRuns(ctx, gen.ListPaymentRunsParams{})
			return err
		},
		"payments": func(ctx context.Context) error {
			_, err := h.ListPayments(ctx, gen.ListPaymentsParams{ID: s.run})
			return err
		},
		"exclusions": func(ctx context.Context) error {
			_, err := h.ListPaymentExclusions(ctx, gen.ListPaymentExclusionsParams{ID: s.run})
			return err
		},
		"bank file": func(ctx context.Context) error {
			_, err := h.GetBankFile(ctx, gen.GetBankFileParams{ID: s.file})
			return err
		},
		"bank files": func(ctx context.Context) error {
			_, err := h.ListBankFiles(ctx, gen.ListBankFilesParams{})
			return err
		},
		"releaser": func(ctx context.Context) error {
			tx, ok := reqctx.TxFrom(ctx)
			if !ok {
				return errors.New("outside a unit of work")
			}
			_, _, err := h.Releaser(ctx, tx, s.relBID)
			return err
		},
		"payments to post": func(ctx context.Context) error {
			tx, ok := reqctx.TxFrom(ctx)
			if !ok {
				return errors.New("outside a unit of work")
			}
			_, _, err := payments.PaymentsToPost(ctx, tx, s.relBID)
			return err
		},
	}
	for name, call := range releaseCalls(h, s) {
		calls[name] = call
	}
	for name, call := range fileAndPortCalls(t, e, h, s) {
		calls[name] = call
	}
	decisions = map[string]func(ctx context.Context) error{
		"release": func(ctx context.Context) error {
			_, err := h.ReleasePaymentRun(ctx, gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString("ok")}),
				gen.ReleasePaymentRunParams{ID: s.run, IfMatch: "*"})
			return err
		},
		"release resume": func(ctx context.Context) error {
			_, err := h.StartRelease(ctx, s.relAID, yes, "again")
			return err
		},
		"reject": func(ctx context.Context) error {
			_, err := h.RejectPaymentRun(ctx, &gen.ReasonInput{Reason: "r"}, gen.RejectPaymentRunParams{ID: s.run, IfMatch: "*"})
			return err
		},
	}
	return calls, decisions
}

// releaseCalls are the release steps, each on the run in the state it needs.
func releaseCalls(h *payments.Endpoints, s *failureRuns) map[string]func(ctx context.Context) error {
	return map[string]func(ctx context.Context) error{
		"post payment": func(ctx context.Context) error {
			r, err := payments.GetRun(ctx, s.relCID)
			if err != nil {
				return err
			}
			_, err = h.PostPayment(ctx, r, s.payC)
			return err
		},
		"post payment again": func(ctx context.Context) error {
			r, err := payments.GetRun(ctx, s.relAID)
			if err != nil {
				return err
			}
			_, err = h.PostPayment(ctx, r, s.payA)
			return err
		},
		"finish release": func(ctx context.Context) error {
			r, err := payments.GetRun(ctx, s.relDID)
			if err != nil {
				return err
			}
			_, err = h.FinishRelease(ctx, r)
			return err
		},
		"fail release": func(ctx context.Context) error {
			r, err := payments.GetRun(ctx, s.relBID)
			if err != nil {
				return err
			}
			_, err = h.FailRelease(ctx, r, problem.Unprocessable("payments.bank_file_invalid", "test"))
			return err
		},
	}
}

// fileAndPortCalls are the bank file handlers and the ports other contexts
// call.
func fileAndPortCalls(t *testing.T, e env, h *payments.Endpoints, s *failureRuns) map[string]func(ctx context.Context) error {
	t.Helper()
	return map[string]func(ctx context.Context) error{
		"transmit": func(ctx context.Context) error {
			_, err := h.TransmitBankFile(ctx, gen.TransmitBankFileParams{ID: s.file, IfMatch: "*"})
			return err
		},
		"report accepted": func(ctx context.Context) error {
			_, err := h.ReportBankFileStatus(ctx, &gen.BankStatusReport{Outcome: "accepted"}, gen.ReportBankFileStatusParams{ID: s.toAck, IfMatch: "*"})
			return err
		},
		"report rejected": func(ctx context.Context) error {
			_, err := h.ReportBankFileStatus(ctx, &gen.BankStatusReport{Outcome: "rejected", Reference: gen.NewOptString("pain.002-0002"),
				Reason: gen.NewOptString("AC01 Incorrect account number")}, gen.ReportBankFileStatusParams{ID: s.toReject, IfMatch: "*"})
			return err
		},
		"in run": func(ctx context.Context) error {
			_, _, err := payments.Reservations{}.RunOf(ctx, e.DE, 2026, s.vendorEntry)
			return err
		},
		"check house banks": func(ctx context.Context) error {
			_, err := houseBanks().Check(ctx, nil, fixture.Document(t))
			return err
		},
		"apply house banks":     func(ctx context.Context) error { return houseBanks().Apply(ctx, fixture.Document(t)) },
		"apply payment methods": func(ctx context.Context) error { return paymentMethods().Apply(ctx, fixture.Document(t)) },
		"is payment method": func(ctx context.Context) error {
			_, err := paymentMethods().IsPaymentMethod(ctx, e.DE, "T")
			return err
		},
		"payees": func(ctx context.Context) error {
			_, err := partner.Payees{Keys: apitest.FieldKeys()}.Payees(ctx, e.DE, []uuid.UUID{e.Supplier}, date("2026-04-15"))
			return err
		},
		"bank details": func(ctx context.Context) error {
			_, err := partner.Payees{Keys: apitest.FieldKeys()}.BankDetails(ctx, []uuid.UUID{uuid.New()})
			return err
		},
	}
}

// refusalCalls are calls on the proposed run, or on nothing, that the
// handlers refuse.
func refusalCalls(e env, h *payments.Endpoints, run uuid.UUID) map[string]func(ctx context.Context) error {
	return map[string]func(ctx context.Context) error{
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
			_, err := h.ProposePaymentRun(ctx, &gen.PaymentRunPropose{LegalEntityID: uuid.New(), HouseBankID: gen.NewOptUUID(e.bank)}, gen.ProposePaymentRunParams{})
			return err
		},
		"release mismatch": func(ctx context.Context) error {
			_, err := h.StartRelease(ctx, run, func(int32) error { return errMismatch }, "")
			return err
		},
		"reject mismatch": func(ctx context.Context) error {
			_, err := h.RejectRun(ctx, run, func(int32) error { return errMismatch }, "r")
			return err
		},
		"release unknown": func(ctx context.Context) error {
			_, err := h.StartRelease(ctx, uuid.New(), yes, "")
			return err
		},
		"reject unknown": func(ctx context.Context) error {
			_, err := h.RejectRun(ctx, uuid.New(), yes, "r")
			return err
		},
	}
}
