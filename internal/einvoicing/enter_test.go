package einvoicing_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// entering is a receiving scene whose company 5000 has a released order
// 4500000001 of the tenant's supplier, who holds the seller's TRN: item 10
// four desks at 250.00 AED with V5 (category S at 5%), item 20 two chairs
// at 100.00 AED without a tax code.
type entering struct {
	*scene
	le, order uuid.UUID
}

func enteringScene(t *testing.T) *entering {
	t.Helper()
	s := &entering{scene: receiving(t)}
	f := s.f
	pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = '5000'`, []any{f.Tenant.ID}, &s.le)
	s.order = uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, '4500000001', $3, $4, 'AED', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		f.Tenant.ID, s.order, s.le, f.Supplier, f.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit)
		VALUES ($1, $2, 10, 'Desk', 4, 'EA', 25000, 1, $3, 'V5', 25000, 1), ($1, $2, 20, 'Chair', 2, 'EA', 10000, 1, $3, NULL, 10000, 1)`,
		f.Tenant.ID, s.order, f.Account(t, "5000", "520000"))
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, s.order)
	pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
		VALUES ($1, $2, 'AE_TRN', $3, $4, '0003', $5, $5)`, f.Tenant.ID, f.Supplier, apitest.Seal(t, f.Tenant.ID, supplierTRN),
		apitest.Fingerprint(t, f.Tenant.ID, supplierTRN), f.Tenant.Human.ID)
	return s
}

// ordered is a document's XML naming the order (IBT-013) and its first
// line naming the order line (IBT-132); E2's renderer writes neither, as
// a sale names no purchase order of the buyer's.
func ordered(d einvoicing.Document, order, line string) []byte {
	x := string(einvoicing.RenderPINTAE(d))
	if order != "" {
		x = strings.Replace(x, "<cac:AccountingSupplierParty>", "<cac:OrderReference><cbc:ID>"+order+"</cbc:ID></cac:OrderReference><cac:AccountingSupplierParty>", 1)
	}
	if line != "" {
		x = strings.Replace(x, "<cac:Item>", "<cac:OrderLineReference><cbc:LineID>"+line+"</cbc:LineID></cac:OrderLineReference><cac:Item>", 1)
	}
	return []byte(x)
}

// entered is a message's document as entered: status, problem code, the
// invoice's source, status, supplier reference and items' codes, and
// whether this fetch entered it.
func (s *entering) entered(t *testing.T, message string, operation uuid.UUID) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT d.status || ' ' || coalesce(d.problem_code, '-') || ' ' || coalesce(i.source || ' ' || i.status || ' ' || i.supplier_reference
			|| ' ' || (SELECT string_agg(coalesce(it.tax_code, '-'), ',' ORDER BY it.item_no) FROM payables.supplier_invoice_item it
			WHERE it.supplier_invoice_id = i.id), '-') || ' ' || (d.entry_fetch_operation_id IS NOT DISTINCT FROM $4)
		FROM einvoicing.einvoice d LEFT JOIN payables.supplier_invoice i ON i.id = d.supplier_invoice_id
		WHERE d.tenant_id = $1 AND d.connector_id = $2 AND d.message_id = $3`, []any{s.f.Tenant.ID, s.connector, message, operation}, &got)
	return got
}

// TestEnterEInvoices: a fetch enters each valid tax invoice in AED as a
// supplier invoice of source einvoice by capture's rules (ADR-0061 §6,
// T65d): the order from IBT-013, its item by order line, its code the
// order item's where it has the line's category and rate, else the one
// that does, else the invoice on hold; every refusal of the entering holds
// the document with its code, a constraint the database raises
// (supplier_invoice_no_duplicates) among them, with nothing of it kept; a
// tax invoice not in AED is held; one stating a preceding invoice (IBG-03)
// is entered as any other; a credit note citing an invoice not
// posted is entered as a credit memo on hold naming it (T65e). Run
// twice, the step enters, holds and publishes nothing again and still
// counts what this fetch entered and held.
func TestEnterEInvoices(t *testing.T) {
	s := enteringScene(t)
	other := supplierInvoice("GOI-3")
	other.Seller.TRN, other.Seller.Endpoint = "100666666600003", "1006666666"
	chairs := supplierInvoice("GOI-4")
	chairs.Lines[0].Net, chairs.Lines[0].Quantity, chairs.Lines[0].Description = 20000, "2", "Chair"
	chairs.Subtotals[0].Taxable, chairs.Subtotals[0].Tax, chairs.Tax = 20000, 1000, 1000
	chairs.LineTotal, chairs.TaxExclusive, chairs.TaxInclusive, chairs.Payable = 20000, 20000, 21000, 21000
	again := supplierInvoice("GOI-1")
	again.Lines[0].Description = "Oak desk, delivered again"
	dollars := supplierInvoice("GOI-6")
	dollars.Currency, dollars.TaxCurrency, dollars.AccountingCurrency = "USD", "AED", "AED"
	aed := money.Minor(18363)
	dollars.AccountingTax = &aed
	credit := supplierInvoice("GOI-7")
	credit.Kind, credit.TypeCode, credit.Reference = einvoicing.KindTaxCreditNote, einvoicing.TypeTaxCreditNote, "GOI-1"
	credit.Reason = &einvoicing.Reason{Code: "VD", Text: "Damaged desk"}
	preceded := supplierInvoice("GOI-1")
	preceded.Reference = "GOI-0" // a preceding invoice (IBG-03), which an invoice may state: entered all the same
	s.srv.Deliver("GOI-1", uaeParticipant, ordered(preceded, "4500000001", "10"))
	s.srv.Deliver("GOI-2", uaeParticipant, ordered(supplierInvoice("GOI-2"), "", ""))
	s.srv.Deliver("GOI-3", uaeParticipant, ordered(other, "4500000001", "10"))
	s.srv.Deliver("GOI-4", uaeParticipant, ordered(chairs, "4500000001", "20"))
	s.srv.Deliver("GOI-5", uaeParticipant, ordered(again, "4500000001", "10"))
	s.srv.Deliver("GOI-6", uaeParticipant, ordered(dollars, "4500000001", "10"))
	s.srv.Deliver("GOI-7", uaeParticipant, ordered(credit, "4500000001", "10"))
	s.srv.Deliver("GOI-8", uaeParticipant, ordered(supplierInvoice("GOI-8"), "4500000001", "ten"))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	ctx := context.Background()
	for run := range 2 {
		if got, err := s.e.ProcessEInvoices(ctx, s.connector, op, s.within()); err != nil || got.Entered != 3 || got.Held != 5 || got.Refused != 0 {
			t.Fatalf("run %d: got %+v (%v), want 3 entered and 5 held", run+1, got, err)
		}
	}
	for message, want := range map[string]string{
		"GOI-1": "entered - einvoice parked GOI-1 V5 true",
		"GOI-2": "held payables.capture_needs_order - true",
		"GOI-3": "held payables.einvoice_seller_unknown - true",
		"GOI-4": "entered - einvoice on_hold GOI-4 - true",
		"GOI-5": "held payables.duplicate_invoice - true",
		"GOI-6": "held " + einvoicing.HeldNotInAED + " - true",
		"GOI-7": "entered - einvoice on_hold GOI-7 V5 true",
		"GOI-8": "held payables.unreadable_document - true",
	} {
		if got := s.entered(t, message, op); got != want {
			t.Errorf("%s: got %s, want %s", message, got, want)
		}
	}
	var invoices int
	var hold, detail string
	var ref bool
	pgtest.Scan(t, `SELECT count(*) FROM payables.supplier_invoice WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{s.f.Tenant.ID, s.le}, &invoices)
	if invoices != 3 {
		t.Errorf("supplier invoices: got %d, want 3, nothing of a held one kept", invoices)
	}
	pgtest.Scan(t, `SELECT hold_reason FROM payables.supplier_invoice WHERE tenant_id = $1 AND supplier_reference = 'GOI-4'`, []any{s.f.Tenant.ID}, &hold)
	if !strings.Contains(hold, "5% in VAT category S, which codes V5, VB5 charge") {
		t.Errorf("GOI-4's hold: got %q, want the codes of category S at 5%% to choose from", hold)
	}
	pgtest.Scan(t, `SELECT problem_detail FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'GOI-3'`, []any{s.f.Tenant.ID}, &detail)
	if !strings.Contains(detail, "ending 0003") || strings.Contains(detail, "100666666600003") {
		t.Errorf("GOI-3's detail: got %q, want the TRN's last four only", detail)
	}
	pgtest.Scan(t, `SELECT d.content_ref = i.capture_object_ref AND d.sha256 = i.capture_sha256 FROM einvoicing.einvoice d
		JOIN payables.supplier_invoice i ON i.id = d.supplier_invoice_id WHERE d.tenant_id = $1 AND d.message_id = 'GOI-1'`,
		[]any{s.f.Tenant.ID}, &ref)
	if !ref {
		t.Errorf("GOI-1's invoice's document: got %v, want the e-invoice's bytes", ref)
	}
	events := apitest.Published(t, s.f.Tenant, outbox.Event{Context: "einvoicing", Name: "einvoice_changed"})
	var outcomes []string
	for _, e := range events {
		if st := e["status"]; st == einvoicing.StatusEntered || st == einvoicing.StatusHeld {
			o := e["message_id"].(string) + " " + st.(string) + " " + e["status_before"].(string)
			if e["supplier_invoice_id"] != nil {
				o += " invoice"
			}
			outcomes = append(outcomes, o)
		}
	}
	if got := strings.Join(outcomes, ", "); got != "GOI-1 entered received invoice, GOI-2 held received, GOI-3 held received, GOI-4 entered received invoice, "+
		"GOI-5 held received, GOI-6 held received, GOI-7 entered received invoice, GOI-8 held received" {
		t.Errorf("EInvoiceChanged of the entering: got %s, want one per document entered or held", got)
	}
	captured := apitest.Published(t, s.f.Tenant, outbox.Event{Context: "payables", Name: "supplier_invoice_captured"})
	if len(captured) != 3 || captured[0]["source"] != "einvoice" {
		t.Errorf("SupplierInvoiceCaptured: got %v, want three of source einvoice", captured)
	}
	var enteredAudits, heldAudits int
	pgtest.Scan(t, `SELECT count(*) FILTER (WHERE action = 'einvoicing.einvoice.entered'), count(*) FILTER (WHERE action = 'einvoicing.einvoice.held')
		FROM audit.audit_event WHERE tenant_id = $1 AND actor_id = $2`, []any{s.f.Tenant.ID, einvoicing.FetcherActor}, &enteredAudits, &heldAudits)
	if enteredAudits != 3 || heldAudits != 5 {
		t.Errorf("audit records: got %d entered and %d held, want 3 and 5, once each", enteredAudits, heldAudits)
	}
	if got, err := s.e.ProcessEInvoices(ctx, s.connector, uuid.Must(uuid.NewV7()), s.within()); err != nil || got.Entered != 0 || got.Held != 0 {
		t.Errorf("a later fetch: got %+v (%v), want nothing entered or held", got, err)
	}
}

// TestEnterValidatedBefore: a tax invoice a fetch validated before T65d
// (processed, still received) is entered by the next fetch, which counts
// it, without validating it again; one whose bytes the rules refuse now is
// held with that refusal rather than refused after its validation.
func TestEnterValidatedBefore(t *testing.T) {
	s := enteringScene(t)
	s.srv.Deliver("GOI-1", uaeParticipant, ordered(supplierInvoice("GOI-1"), "4500000001", "10"))
	s.srv.Deliver("BAD", uaeParticipant, []byte("not XML"))
	first, next := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	s.recordAll(t, first)
	pgtest.Exec(t, `UPDATE einvoicing.einvoice SET processed_at = now(), processed_fetch_operation_id = $2, validated_against = $3,
		document_number = message_id, document_kind = 'invoice', version = version + 1 WHERE tenant_id = $1`, s.f.Tenant.ID, first, einvoicing.PINTAERelease)
	got, err := s.e.ProcessEInvoices(context.Background(), s.connector, next, s.within())
	if err != nil || got.Entered != 1 || got.Held != 1 {
		t.Fatalf("the next fetch: got %+v (%v), want GOI-1 entered and BAD held", got, err)
	}
	if got := s.entered(t, "GOI-1", next); got != "entered - einvoice parked GOI-1 V5 true" {
		t.Errorf("GOI-1: got %s, want it entered by the next fetch", got)
	}
	if got := s.entered(t, "BAD", next); got != "held "+einvoicing.RefusedUnreadable+" - true" {
		t.Errorf("BAD: got %s, want it held with the refusal", got)
	}
	var validated int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.validated'`, []any{s.f.Tenant.ID}, &validated)
	if validated != 0 {
		t.Errorf("validations audited: got %d, want none, the documents processed before", validated)
	}
}

// TestEnterEInvoicesFailures: payables failing on the machinery fails the
// step and leaves the document validated and received, which a later
// fetch enters; every statement of processing and entering failing in
// turn fails the step, and the run after enters the document once; the
// later fetch's documents list what it entered and held, though another
// fetch recorded them.
func TestEnterEInvoicesFailures(t *testing.T) {
	s := enteringScene(t)
	ctx := context.Background()
	s.srv.Deliver("GOI-1", uaeParticipant, ordered(supplierInvoice("GOI-1"), "4500000001", "10"))
	s.srv.Deliver("GOI-2", uaeParticipant, ordered(supplierInvoice("GOI-2"), "", ""))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	e := app.EInvoicing(s.cfg)
	e.Payables = failingPayables{e.Payables}
	if _, err := e.ProcessEInvoices(ctx, s.connector, op, s.within()); err == nil || !strings.Contains(err.Error(), "payables down") {
		t.Fatalf("payables failing: got %v, want its error", err)
	}
	var state string
	pgtest.Scan(t, `SELECT string_agg(message_id || ' ' || status || ' ' || (processed_at IS NOT NULL), ', ' ORDER BY message_id) FROM einvoicing.einvoice
		WHERE tenant_id = $1`, []any{s.f.Tenant.ID}, &state)
	if state != "GOI-1 received false, GOI-2 received false" {
		t.Errorf("after payables failed: got %s, want both left as they were", state)
	}
	later := uuid.Must(uuid.NewV7()) // a later fetch enters what this one recorded
	process := func(in einvoicing.Within) error {
		_, err := s.e.ProcessEInvoices(ctx, s.connector, later, in)
		return err
	}
	everyFailure(t, s.scene, process)
	if got := s.entered(t, "GOI-1", later) + ", " + s.entered(t, "GOI-2", later); got != "entered - einvoice parked GOI-1 V5 true, held payables.capture_needs_order - true" {
		t.Errorf("after the failures: got %s, want GOI-1 entered and GOI-2 held by the later fetch", got)
	}
	var res gen.ListEInvoicesRes
	if err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) (err error) {
		res, err = s.e.ListEInvoices(ctx, gen.ListEInvoicesParams{FetchOperationID: gen.NewOptUUID(later)})
		return err
	}); err != nil {
		t.Fatalf("the later fetch's documents: got %v, want its page", err)
	}
	page, ok := res.(*gen.EInvoicePage)
	if !ok {
		t.Fatalf("the later fetch's documents: got %T, want a page", res)
	}
	if len(page.Items) != 2 {
		t.Errorf("the later fetch's documents: got %d, want 2, GOI-1 and GOI-2, which it entered and held", len(page.Items))
	}
	var invoices int
	pgtest.Scan(t, `SELECT count(*) FROM payables.supplier_invoice WHERE tenant_id = $1 AND source = 'einvoice'`, []any{s.f.Tenant.ID}, &invoices)
	if invoices != 1 {
		t.Errorf("invoices entered: got %d, want 1", invoices)
	}
}

// failingPayables fails every entering on the machinery.
type failingPayables struct{ einvoicing.Payables }

func (failingPayables) Enter(context.Context, einvoicing.Entry) (einvoicing.Entered, error) {
	return einvoicing.Entered{}, errors.New("payables down")
}

// TestEnteredEInvoiceListed: the list and the document name the invoice an
// e-invoice was entered as, its supplier and when and by which fetch, and
// filter by the supplier (ADR-0061 §15).
func TestEnteredEInvoiceListed(t *testing.T) {
	s := enteringScene(t)
	s.srv.Deliver("GOI-1", uaeParticipant, ordered(supplierInvoice("GOI-1"), "4500000001", "10"))
	s.srv.Deliver("GOI-2", uaeParticipant, ordered(supplierInvoice("GOI-2"), "", ""))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	if _, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err != nil {
		t.Fatal(err)
	}
	list := func(params gen.ListEInvoicesParams) []gen.EInvoice {
		var page *gen.EInvoicePage
		if err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
			res, err := s.e.ListEInvoices(ctx, params)
			page, _ = res.(*gen.EInvoicePage)
			return err
		}); err != nil || page == nil {
			t.Fatalf("the list: got %v %v", page, err)
		}
		return page.Items
	}
	items := list(gen.ListEInvoicesParams{SupplierID: gen.NewOptUUID(s.f.Supplier)})
	if len(items) != 1 {
		t.Fatalf("the supplier's: got %d, want GOI-1 alone", len(items))
	}
	d := items[0]
	if got, want := strings.Join([]string{d.MessageID, d.Status, d.SupplierReference.Value, d.SupplierInvoiceStatus.Value, d.SupplierName.Value,
		d.SupplierID.Value.String(), d.EntryFetchOperationID.Value.String()}, " "),
		"GOI-1 entered GOI-1 parked BP90000001 "+s.f.Supplier.String()+" "+op.String(); got != want {
		t.Errorf("the entered document: got %s, want %s", got, want)
	}
	if !d.SupplierInvoiceID.Set || d.SupplierInvoiceNumber.Value == "" || !d.EnteredAt.Set {
		t.Errorf("its invoice: got %v %v %v, want its id, number and when", d.SupplierInvoiceID, d.SupplierInvoiceNumber, d.EnteredAt)
	}
	apitest.EveryFailure(t, s.f.Tenant, s.f.Tenant.Human, "list the entered", func(ctx context.Context) error {
		_, err := s.e.ListEInvoices(ctx, gen.ListEInvoicesParams{SupplierID: gen.NewOptUUID(s.f.Supplier)})
		return err
	})
	held := list(gen.ListEInvoicesParams{Status: gen.NewOptString(einvoicing.StatusHeld)})
	if len(held) != 1 || held[0].SupplierInvoiceID.Set || held[0].SupplierName.Set || !held[0].EnteredAt.Set {
		t.Errorf("the held one: got %+v, want GOI-2 with no invoice and when it was held", held)
	}
}

// TestEnterEInvoiceRetryable: payables answering a problem that asks to
// be retried (a lock timeout: 409 request.in_progress with a Retry-After)
// fails the step rather than holding the document, which stays received
// for the next fetch (review round 1).
func TestEnterEInvoiceRetryable(t *testing.T) {
	s := enteringScene(t)
	s.srv.Deliver("GOI-1", uaeParticipant, ordered(supplierInvoice("GOI-1"), "4500000001", "10"))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	e := app.EInvoicing(s.cfg)
	e.Payables = lockedPayables{e.Payables}
	if _, err := e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err == nil {
		t.Fatal("a lock timeout: got no error, want the step to fail")
	}
	if got := s.entered(t, "GOI-1", op); got != "received - - false" {
		t.Errorf("after the lock timeout: got %s, want it left received", got)
	}
}

// lockedPayables answers every entering with a lock timeout.
type lockedPayables struct{ einvoicing.Payables }

func (lockedPayables) Enter(context.Context, einvoicing.Entry) (einvoicing.Entered, error) {
	return einvoicing.Entered{}, &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
}

// TestEnterEInvoiceRolledBackAudit: a rule that holds the new invoice
// audits that hold inside the savepoint; when the entering is then
// refused (a duplicate of an invoice entered by hand), the hold's audit
// entry goes with the rollback, so no record names an invoice that never
// existed (review round 1).
func TestEnterEInvoiceRolledBackAudit(t *testing.T) {
	s := enteringScene(t)
	row := func(gross string) map[string]any {
		return map[string]any{"legal_entity_id": "le", "supplier_id": "s", "supplier_reference": "r", "invoice_date": "2026-03-10",
			"gross_amount": map[string]any{"amount": gross, "currency": "AED"}, "tax_amount": map[string]any{"amount": "0.00", "currency": "AED"}}
	}
	fixture.ActivateRules(t, s.f.Tenant.ID, "supplier_invoice", map[string]rules.Rule{"gross_limit": {Expression: `row.gross_amount.minor <= 10000`,
		Message: "Invoices above 100.00 need a contract.", Cases: []rules.Case{{Row: row("100.00"), Valid: true}, {Row: row("100.01"), Valid: false}}}})
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
		currency, gross_amount, tax_amount, status, source, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'BYHAND-1', $2, $3, 'GOI-1', '2026-03-10', 'AED', 105000, 5000, 'parked', 'manual', $4, $4)`,
		s.f.Tenant.ID, s.le, s.f.Supplier, s.f.Tenant.Human.ID)
	s.srv.Deliver("GOI-1", uaeParticipant, ordered(supplierInvoice("GOI-1"), "4500000001", "10"))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	if _, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err != nil {
		t.Fatal(err)
	}
	if got := s.entered(t, "GOI-1", op); got != "held payables.duplicate_invoice - true" {
		t.Errorf("GOI-1: got %s, want held as a duplicate", got)
	}
	if got := fixture.RuleAudits(t, s.f.Tenant.ID, "payables.supplier_invoice.create"); len(got) != 0 {
		t.Errorf("rule audits: got %+v, want none for the invoice rolled back", got)
	}
}
