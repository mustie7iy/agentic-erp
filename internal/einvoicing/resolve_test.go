package einvoicing_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// resolving is an entering scene whose first fetch held GOI-2 and GOI-2B
// (the same bytes delivered twice, naming no order), GOI-3 (a seller the
// order's supplier does not hold the TRN of), GOI-6 (in dollars without
// its VAT in AED) and BAD (unreadable now, validated before), and whose
// clerk may resolve held e-invoices and read supplier invoices (ADR-0061
// §7, §11; T65f).
type resolving struct {
	*entering
	op    uuid.UUID
	bytes map[string][]byte
}

const otherTRN = "100666666600003"

func resolvingScene(t *testing.T) *resolving {
	t.Helper()
	s := &resolving{entering: enteringScene(t), op: uuid.Must(uuid.NewV7()), bytes: map[string][]byte{}}
	other := supplierInvoice("GOI-3")
	other.Seller.TRN, other.Seller.Endpoint = otherTRN, otherTRN[:10]
	dollars := supplierInvoice("GOI-6")
	dollars.Currency, dollars.TaxCurrency, dollars.AccountingCurrency = "USD", "AED", "AED"
	aed := money.Minor(18363)
	dollars.AccountingTax = &aed
	s.bytes["GOI-2"] = ordered(supplierInvoice("GOI-2"), "", "")
	s.bytes["GOI-2B"] = s.bytes["GOI-2"]
	s.bytes["GOI-3"] = ordered(other, "4500000001", "10")
	s.bytes["GOI-6"] = ordered(dollars, "4500000001", "10")
	s.bytes["BAD"] = []byte("not XML")
	for _, m := range []string{"GOI-2", "GOI-2B", "GOI-3", "GOI-6", "BAD"} {
		s.srv.Deliver(m, uaeParticipant, s.bytes[m])
	}
	s.recordAll(t, s.op)
	// BAD was validated before the rules that refuse it now (as
	// TestEnterValidatedBefore), so the fetch holds it with that refusal.
	pgtest.Exec(t, `UPDATE einvoicing.einvoice SET processed_at = now(), processed_fetch_operation_id = $2, validated_against = $3,
		document_number = message_id, document_kind = 'invoice', version = version + 1 WHERE tenant_id = $1 AND message_id = 'BAD'`,
		s.f.Tenant.ID, s.op, einvoicing.PINTAERelease)
	if got, err := s.e.ProcessEInvoices(context.Background(), s.connector, s.op, s.within()); err != nil || got.Held != 5 {
		t.Fatalf("the fetch: got %+v (%v), want five held", got, err)
	}
	s.f.Tenant.Grant(t, "einvoicing.einvoice.manage", s.f.Tenant.Human)
	s.f.Tenant.Grant(t, "payables.invoice.read", s.f.Tenant.Human)
	return s
}

// doc is the message's document as the clerk reads it, with its ETag.
func (s *resolving) doc(t *testing.T, message string) (map[string]any, string) {
	t.Helper()
	var id uuid.UUID
	pgtest.Scan(t, `SELECT id FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = $2`, []any{s.f.Tenant.ID, message}, &id)
	return s.f.Must(http.StatusOK, http.MethodGet, "/v1/e-invoices/"+id.String(), "", "")
}

// path is the path of the message's document's operation.
func (s *resolving) path(t *testing.T, message, action string) string {
	t.Helper()
	d, _ := s.doc(t, message)
	return "/v1/e-invoices/" + d["id"].(string) + ":" + action
}

// manual writes an invoice of the supplier entered by hand in the company,
// with the reference, in status (posted with its entry), and returns it.
func (s *resolving) manual(t *testing.T, le uuid.UUID, reference, status string) uuid.UUID {
	t.Helper()
	return s.manualOf(t, "invoice", le, reference, status)
}

// manualOf is manual for an invoice of the kind.
func (s *resolving) manualOf(t *testing.T, kind string, le uuid.UUID, reference, status string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
			posting_date, currency, gross_amount, tax_amount, status, source, journal_entry_year, journal_entry_id, created_by, updated_by, kind)
		SELECT $1, $2, $3, $4, $5, $6, '2026-03-10', CASE WHEN p THEN date '2026-03-10' END, 'AED', 105000, 5000, $7, 'manual',
			CASE WHEN p THEN 2026 END, CASE WHEN p THEN gen_random_uuid() END, $8, $8, $9 FROM (SELECT $7 = 'posted' AS p) x`,
		s.f.Tenant.ID, id, "M"+id.String()[24:], le, s.f.Supplier, reference, status, s.f.Tenant.Human.ID, kind)
	return id
}

// supplierHolds gives the order's supplier the TRN as a further tax
// identifier (a scheme of its own: one per scheme).
func (s *resolving) supplierHolds(t *testing.T, trn string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
		VALUES ($1, $2, 'AE_OTHER', $3, $4, '0003', $5, $5)`, s.f.Tenant.ID, s.f.Supplier, apitest.Seal(t, s.f.Tenant.ID, trn),
		apitest.Fingerprint(t, s.f.Tenant.ID, trn), s.f.Tenant.Human.ID)
}

// resolved is a document as a resolution leaves it: status, problem
// code, resolution, whether resolved_at is set, and the invoice it names.
func resolved(d map[string]any) string {
	return words(d["status"], d["problem_code"], d["resolution"], d["resolved_at"] != nil, d["supplier_invoice_id"])
}

// TestRetryEInvoice (T65f): a retry enters a held document again in the
// clerk's request, after its cause was corrected, and answers it entered;
// one whose cause stands stays held with this attempt's problem (payables'
// refusal, a document not in AED, the rules refusing its bytes now), 200
// either way; each attempt is audited and published as retried, the
// fetch's entry kept; a document not held, another ETag and an agent are
// refused.
func TestRetryEInvoice(t *testing.T) {
	s := resolvingScene(t)
	d, tag := s.doc(t, "GOI-2")
	got, after := s.f.Must(http.StatusOK, http.MethodPost, s.path(t, "GOI-2", "retry"), "", tag)
	check(t, "GOI-2 still without its order", resolved(got), "held payables.capture_needs_order retried true <nil>")
	check(t, "the fetch's entry kept", words(got["entry_fetch_operation_id"], got["entered_at"] == d["entered_at"]), words(s.op, true))
	if after == tag {
		t.Errorf("the ETag: got %s again, want it moved", after)
	}
	s.f.Problem(http.StatusPreconditionFailed, "request.version_mismatch", http.MethodPost, s.path(t, "GOI-2", "retry"), "", tag)
	for message, want := range map[string]string{
		"GOI-6": "held " + einvoicing.HeldNotInAED + " retried true <nil>",
		"BAD":   "held " + einvoicing.RefusedUnreadable + " retried true <nil>",
	} {
		_, tag := s.doc(t, message)
		got, _ := s.f.Must(http.StatusOK, http.MethodPost, s.path(t, message, "retry"), "", tag)
		check(t, message, resolved(got), want)
	}
	s.supplierHolds(t, otherTRN)
	_, tag = s.doc(t, "GOI-3")
	got, _ = s.f.Must(http.StatusOK, http.MethodPost, s.path(t, "GOI-3", "retry"), "", tag)
	check(t, "GOI-3 entered once its supplier holds the TRN", words(got["status"], got["problem_code"], got["resolution"], got["supplier_invoice_status"],
		got["supplier_reference"]), "entered <nil> retried parked GOI-3")
	check(t, "its invoice", s.entered(t, "GOI-3", s.op), "entered - einvoice parked GOI-3 V5 true")
	_, tag = s.doc(t, "GOI-3")
	s.f.Problem(http.StatusConflict, "einvoicing.einvoice_not_held", http.MethodPost, s.path(t, "GOI-3", "retry"), "", tag)
	s.f.Problem(http.StatusNotFound, "request.not_found", http.MethodPost, "/v1/e-invoices/"+uuid.NewString()+":retry", "", tag)
	var audits, events int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.retried' AND actor_id = $2`,
		[]any{s.f.Tenant.ID, s.f.Tenant.Human.ID}, &audits)
	pgtest.Scan(t, `SELECT count(*) FROM einvoicing.outbox WHERE tenant_id = $1 AND payload->>'resolution' = 'retried' AND payload->>'status_before' = 'held'`,
		[]any{s.f.Tenant.ID}, &events)
	check(t, "retries audited and published", words(audits, events), "4 4")
	if got := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "einvoice_invoice_once"}).Code; got != "einvoicing.invoice_already_linked" {
		t.Errorf("einvoice_invoice_once: got %s, want einvoicing.invoice_already_linked", got)
	}
	s.f.Tenant.AllCompanies(t, s.f.Tenant.Agent)
	agent := apitest.Client{T: t, API: s.f.API, Tenant: s.f.Tenant, As: s.f.Tenant.Agent}
	_, tag = s.doc(t, "GOI-2")
	for action, body := range map[string]string{"retry": "", "link": `{"supplier_invoice_id": "` + uuid.NewString() + `"}`,
		"dismiss": `{"reason": "An agent's"}`} {
		agent.Problem(http.StatusForbidden, "identity.agent_not_allowed", http.MethodPost, s.path(t, "GOI-2", action), body, tag)
	}
}

// TestLinkEInvoice (T65f): a held document is linked to the invoice the
// clerk entered by hand, posted already: entered, naming it and its
// supplier, the invoice's document the e-invoice's bytes, audited and
// published; a second document is refused the invoice (at most one names
// it), bytes that are another invoice's document already are refused, and
// an invoice not entered by hand, rejected or cancelled, of another
// company, supplier, kind, date, currency, gross or tax amount or
// reference, or with a document of its own, is a mismatch naming each
// difference.
func TestLinkEInvoice(t *testing.T) {
	s := resolvingScene(t)
	posted := s.manual(t, s.le, "GOI-2", "posted")
	_, tag := s.doc(t, "GOI-2")
	got, _ := s.f.Must(http.StatusOK, http.MethodPost, s.path(t, "GOI-2", "link"), fmt.Sprintf(`{"supplier_invoice_id": %q}`, posted), tag)
	check(t, "GOI-2 linked", resolved(got), "entered <nil> linked true "+posted.String())
	check(t, "its invoice named", words(got["supplier_id"], got["supplier_reference"], got["supplier_invoice_status"], got["supplier_name"] != ""),
		words(s.f.Supplier, "GOI-2", "posted", true))
	r := s.f.API.Do(apitest.Req{Method: http.MethodGet, Path: "/v1/supplier-invoices/" + posted.String() + "/document", Tenant: s.f.Tenant.ID,
		As: s.f.Tenant.Human})
	if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), s.bytes["GOI-2"]) {
		t.Errorf("the invoice's document: got %d %q, want the e-invoice's bytes", r.Code, r.Body)
	}
	var audits string
	pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY action) FROM audit.audit_event WHERE tenant_id = $1 AND object_id IN ($2, $3)
		AND action IN ('einvoicing.einvoice.linked', 'payables.supplier_invoice.document_attached')`, []any{s.f.Tenant.ID, uuid.MustParse(got["id"].(string)),
		posted}, &audits)
	check(t, "audited", audits, "einvoicing.einvoice.linked payables.supplier_invoice.document_attached")

	link := func(message string, invoice uuid.UUID) map[string]any {
		_, tag := s.doc(t, message)
		_, m, _ := s.f.Do(http.MethodPost, s.path(t, message, "link"), fmt.Sprintf(`{"supplier_invoice_id": %q}`, invoice), tag)
		return m
	}
	check(t, "a second document to the invoice", link("GOI-2B", posted)["code"], "einvoicing.invoice_already_linked")
	// The first invoice reversed frees its reference (supplier_invoice_no_duplicates) for one entered again by hand, which
	// has no document of its own but is offered the bytes the first holds.
	pgtest.Exec(t, `UPDATE payables.supplier_invoice SET status = 'reversed', reversal_entry_year = 2026, reversal_entry_id = gen_random_uuid(),
		version = version + 1 WHERE id = $1`, posted)
	again := s.manual(t, s.le, "GOI-2", "parked")
	check(t, "its bytes another invoice's document", link("GOI-2B", again)["code"], "payables.already_captured")
	// Each case changes one figure of the invoice the e-invoice states otherwise, the others as it states them.
	for _, c := range []struct{ name, set, want string }{
		{"another gross amount", "gross_amount = 105001", "its gross amount 1050.01 AED is not the e-invoice's total with VAT 1050.00 AED (IBT-112)"},
		{"another tax amount", "tax_amount = 4000", "its tax amount 40.00 AED is not the e-invoice's VAT total 50.00 AED (IBT-110)"},
		{"another invoice date", "invoice_date = '2026-03-11'", "its invoice date 2026-03-11 is not the e-invoice's issue date 2026-03-10 (IBT-002)"},
		{"rejected", "status = 'rejected'", "it is rejected"},
		{"cancelled", "status = 'cancelled'", "it is cancelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pgtest.Exec(t, `UPDATE payables.supplier_invoice SET gross_amount = 105000, tax_amount = 5000, invoice_date = '2026-03-10',
				status = 'parked', version = version + 1 WHERE id = $1`, again)
			pgtest.Exec(t, `UPDATE payables.supplier_invoice SET `+c.set+`, version = version + 1 WHERE id = $1`, again)
			m := link("GOI-2B", again)
			check(t, "the refusal", words(m["code"], len(apitest.List(t, m, "errors"))), "einvoicing.link_mismatch 1")
			if !strings.Contains(m["detail"].(string), c.want) {
				t.Errorf("the detail %q: want it to name %q", m["detail"], c.want)
			}
		})
	}
	check(t, "no such invoice", link("GOI-2B", uuid.New())["code"], "request.reference_not_found")
	// The kind is fixed once entered: a credit memo of its own, once the loop's last case cancelled the invoice above.
	memo := link("GOI-2B", s.manualOf(t, "credit_memo", s.le, "GOI-2", "parked"))
	check(t, "another kind", words(memo["code"], len(apitest.List(t, memo, "errors"))), "einvoicing.link_mismatch 1")
	if want := "its kind credit_memo is not invoice, the kind the e-invoice's invoice (IBT-003) is entered as"; !strings.Contains(memo["detail"].(string), want) {
		t.Errorf("the detail %q: want it to name %q", memo["detail"], want)
	}
	parked := s.manual(t, s.le, "X-1", "parked")
	captured := s.manual(t, s.le, "GOI-6", "cancelled")
	pgtest.Exec(t, `UPDATE payables.supplier_invoice SET source = 'capture', capture_sha256 = sha256('captured'::bytea), version = version + 1
		WHERE id = $1`, captured)
	for name, c := range map[string]struct {
		message string
		invoice uuid.UUID
		want    []string
	}{
		"another reference":         {"GOI-6", parked, []string{"currency AED is not the e-invoice's USD (IBT-005)", "supplier reference X-1"}},
		"another supplier":          {"GOI-3", s.manual(t, s.le, "GOI-3", "parked"), []string{"TRN (IBT-031) ending " + otherTRN[11:]}},
		"another company":           {"GOI-6", s.manual(t, s.f.DE, "GOI-6", "parked"), []string{"another company's", "currency AED"}},
		"the seller unreadable now": {"BAD", parked, []string{"cannot be read now (" + einvoicing.RefusedUnreadable + ")", "supplier reference X-1"}},
		"a captured one": {"GOI-6", captured, []string{"not entered by hand (source capture)", "it is cancelled", "currency AED",
			"a document of its own"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := link(c.message, c.invoice)
			if m["code"] != "einvoicing.link_mismatch" {
				t.Fatalf("got %v, want einvoicing.link_mismatch", m)
			}
			errs := apitest.List(t, m, "errors")
			check(t, "a field error for each", len(errs), len(c.want))
			for _, w := range c.want {
				if !strings.Contains(m["detail"].(string), w) {
					t.Errorf("the detail %q: want it to name %q", m["detail"], w)
				}
			}
		})
	}
	if got, _ := s.doc(t, "GOI-6"); got["status"] != "held" || got["resolution"] != nil {
		t.Errorf("GOI-6 after the refusals: got %v %v, want still held, unresolved", got["status"], got["resolution"])
	}
}

// TestDismissEInvoice (T65f): a held document is dismissed with a reason,
// final, keeping the problem it was held with; the reason is kept on the
// document and neither in the audit record nor in the event; a blank
// reason and a document not held are refused.
func TestDismissEInvoice(t *testing.T) {
	s := resolvingScene(t)
	_, tag := s.doc(t, "GOI-2B")
	s.f.Problem(http.StatusUnprocessableEntity, "request.validation_failed", http.MethodPost, s.path(t, "GOI-2B", "dismiss"), `{"reason": "  "}`, tag)
	const reason = "The supplier sent GOI-2 twice."
	got, _ := s.f.Must(http.StatusOK, http.MethodPost, s.path(t, "GOI-2B", "dismiss"), `{"reason": "`+reason+`"}`, tag)
	check(t, "dismissed", words(resolved(got), got["dismissal_reason"]), "dismissed payables.capture_needs_order dismissed true <nil> "+reason)
	_, tag = s.doc(t, "GOI-2B")
	s.f.Problem(http.StatusConflict, "einvoicing.einvoice_not_held", http.MethodPost, s.path(t, "GOI-2B", "dismiss"), `{"reason": "Again"}`, tag)
	var leaked int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND detail::text LIKE '%twice%')
		+ (SELECT count(*) FROM einvoicing.outbox WHERE tenant_id = $1 AND payload::text LIKE '%twice%')`, []any{s.f.Tenant.ID}, &leaked)
	check(t, "the reason in an audit record or event", leaked, 0)
	page, _ := s.f.Must(http.StatusOK, http.MethodGet, "/v1/e-invoices?status=dismissed", "", "")
	check(t, "listed dismissed", messages(apitest.List(t, page, "items")), "GOI-2B")
}

// TestResolveEInvoiceStore (T65f): a retry or a link reads the document's
// bytes before it locks the row: a server without object storage is
// unavailable, so is a store that fails, and bytes gone or other than
// those received are refused as the download refuses them.
func TestResolveEInvoiceStore(t *testing.T) {
	s := resolvingScene(t)
	d, tag := s.doc(t, "GOI-6")
	id := uuid.MustParse(d["id"].(string))
	call := func(e *einvoicing.Endpoints) error {
		return apitest.Committed(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := e.RetryEInvoice(ctx, gen.RetryEInvoiceParams{ID: id, IfMatch: tag})
			if err == nil {
				_, err = e.LinkEInvoice(ctx, &gen.EInvoiceLink{SupplierInvoiceID: uuid.New()}, gen.LinkEInvoiceParams{ID: id, IfMatch: tag})
			}
			return err
		})
	}
	failing := app.EInvoicing(s.cfg)
	failing.Objects = failingGets{}
	kept := filepath.Join(s.store.Root, filepath.FromSlash(blobstore.Key(s.f.Tenant.ID, blobstore.KindDocuments, hash(s.bytes["GOI-6"]))))
	for _, c := range []struct {
		name, code string
		e          *einvoicing.Endpoints
		before     func() error
	}{
		{"no object storage", "platform.unavailable", app.EInvoicing(app.Config{Pages: s.cfg.Pages}), nil},
		{"the store failing", "platform.unavailable", failing, nil},
		{"other bytes", "einvoicing.document_changed", s.e, func() error { return os.WriteFile(kept, []byte("<Invoice/>"), 0o600) }},
		{"bytes gone", "einvoicing.document_changed", s.e, func() error { return os.Remove(kept) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.before != nil {
				if err := c.before(); err != nil {
					t.Fatal(err)
				}
			}
			if err := call(c.e); problem.From(err).Code != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
}

// TestResolveEInvoiceFailures (T65f): every statement of a retry, a link
// and a dismissal failing in turn fails the request; payables failing on
// the machinery fails a retry rather than holding the document again;
// outside a unit of work each fails.
func TestResolveEInvoiceFailures(t *testing.T) {
	s := resolvingScene(t)
	s.supplierHolds(t, otherTRN)
	posted := s.manual(t, s.le, "GOI-2", "posted")
	ids := map[string]uuid.UUID{}
	tags := map[string]string{}
	for _, m := range []string{"GOI-2", "GOI-3", "GOI-6"} {
		d, tag := s.doc(t, m)
		ids[m], tags[m] = uuid.MustParse(d["id"].(string)), tag
	}
	calls := map[string]func(ctx context.Context) error{
		"retry": func(ctx context.Context) error {
			_, err := s.e.RetryEInvoice(ctx, gen.RetryEInvoiceParams{ID: ids["GOI-3"], IfMatch: tags["GOI-3"]})
			return err
		},
		"link": func(ctx context.Context) error {
			_, err := s.e.LinkEInvoice(ctx, &gen.EInvoiceLink{SupplierInvoiceID: posted}, gen.LinkEInvoiceParams{ID: ids["GOI-2"], IfMatch: tags["GOI-2"]})
			return err
		},
		"dismiss": func(ctx context.Context) error {
			_, err := s.e.DismissEInvoice(ctx, &gen.EInvoiceDismissal{Reason: "Sent in error"}, gen.DismissEInvoiceParams{ID: ids["GOI-6"], IfMatch: tags["GOI-6"]})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, name, call)
		})
	}
	t.Run("payables failing", func(t *testing.T) {
		e := app.EInvoicing(s.cfg)
		e.Payables = failingPayables{e.Payables}
		err := apitest.Committed(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			_, err := e.RetryEInvoice(ctx, gen.RetryEInvoiceParams{ID: ids["GOI-3"], IfMatch: tags["GOI-3"]})
			return err
		})
		if err == nil || problem.From(err).Status < 500 {
			t.Errorf("got %v, want the machinery's failure", err)
		}
		if got, _ := s.doc(t, "GOI-3"); got["status"] != "held" || got["resolution"] != nil {
			t.Errorf("GOI-3: got %v %v, want still held, unresolved", got["status"], got["resolution"])
		}
	})
}
