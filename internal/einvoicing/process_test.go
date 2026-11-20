package einvoicing_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/einvoicing/einvoicetest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
)

// The UAE company's participant at the provider: scheme 0235 and its
// TIN, the first ten digits of its FTA registration (sellerTRN), and a
// supplier's TRN (illustrative).
const (
	uaeParticipant = "0235:1002345678"
	supplierTRN    = "100555555500003"
)

// receiving is uaeSetup's tenant (5000 with its FTA registration from
// 2026-01-01) with an einvoice_provider connector to the stand-in naming
// 5000's participant, as the fetch's scene.
func receiving(t *testing.T) *scene {
	t.Helper()
	u := uaeSetup(t, &config.PostalAddress{Line1: "Gulf Tower", City: "Dubai", Region: "AE-DU", CountryCode: "AE"})
	s := &scene{f: u.f, srv: einvoicetest.NewServer(t, "tok"), store: u.store, pool: pgtest.Pool(t)}
	client, err := einvoicing.NewClient(egress.LoopbackForTests(s.srv.Roots))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	s.cfg = app.Config{Secrets: vaulttest.ByPath{"provider": "tok"}, Objects: s.store, Providers: client, Pages: pages, FieldKeys: apitest.FieldKeys()}
	s.e = app.EInvoicing(s.cfg)
	s.connector = uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, 'einvoice_provider', 'Gulf provider', jsonb_build_object('adapter', 'standin', 'base_url', $3::text, 'format', 'pint_ae',
		'participants', jsonb_build_object('5000', $4::text)), $5, 'active', $6, $6)`,
		s.f.Tenant.ID, s.connector, s.srv.URL, uaeParticipant, vaulttest.TenantRef(s.f.Tenant.ID, "provider"), s.f.Tenant.Human.ID)
	return s
}

// supplierInvoice is a supplier's PINT AE tax invoice to Maple Gulf
// Trading, issued on 2026-03-10: one standard-rated line of 1,000.00 AED
// with 50.00 VAT.
func supplierInvoice(number string) einvoicing.Document {
	party := func(name, trn, street string) einvoicing.Party {
		return einvoicing.Party{EndpointScheme: einvoicing.EndpointScheme, Endpoint: trn[:10], Name: name, TRN: trn,
			Address: einvoicing.Address{Line1: street, City: "Dubai", Subentity: "DXB", Country: "AE"}}
	}
	return einvoicing.Document{Kind: einvoicing.KindTaxInvoice, TypeCode: einvoicing.TypeTaxInvoice, Number: number,
		IssueDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), Currency: "AED", Customization: einvoicing.PINTAECustomization,
		Profile: einvoicing.PINTAEProfile, TransactionType: "00000000", OrderReference: "4500000001",
		Seller: party("Gulf Office Interiors", supplierTRN, "Sheikh Zayed Road"), Buyer: party("Maple Gulf Trading", sellerTRN, "Gulf Tower"),
		PaymentMeans: "30", Lines: []einvoicing.Line{{ID: "1", Quantity: "4", Unit: "C62", Net: 100000, Price: 25000, Name: "Desk",
			Description: "Oak desk", Category: "S", Percent: "5"}},
		Subtotals: []einvoicing.Subtotal{{Category: "S", Percent: "5", Taxable: 100000, Tax: 5000}}, Tax: 5000, LineTotal: 100000,
		TaxExclusive: 100000, TaxInclusive: 105000, Payable: 105000, Exponent: 2}
}

// processed is what the tenant holds of the connector's message once
// processed: status, problem code, what was read and whether it was
// processed by the fetch.
func (s *scene) processed(t *testing.T, message string, operation uuid.UUID) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT status || ' ' || coalesce(problem_code, '-') || ' ' || coalesce(document_number, '-') || ' ' || coalesce(document_kind, '-')
		|| ' ' || coalesce(issue_date::text, '-') || ' ' || coalesce(currency, '-') || ' ' || coalesce(gross_amount::text, '-') || ' '
		|| coalesce(validated_against, '-') || ' ' || (processed_fetch_operation_id IS NOT DISTINCT FROM $4)
		FROM einvoicing.einvoice WHERE tenant_id = $1 AND connector_id = $2 AND message_id = $3`,
		[]any{s.f.Tenant.ID, s.connector, message, operation}, &got)
	return got
}

// recordAll records every message the stand-in holds for the fetch.
func (s *scene) recordAll(t *testing.T, operation uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pending, err := s.e.PendingEInvoices(ctx, s.connector, s.within())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pending {
		if _, err := s.e.RecordEInvoice(ctx, s.connector, operation, m, s.within()); err != nil {
			t.Fatalf("record %s: %v", m.ID, err)
		}
	}
}

// TestProcessEInvoices: a fetch's processing reads each document of the
// connector not processed yet as PINT AE (ADR-0061 §4, §5; T65c): a valid
// one stays received with what it says, the release validated against and
// an audit record but no event; a malformed one, one addressed to another
// TRN or electronic address and one listed for none of the companies are
// refused with their reasons, each audited and published; one issued
// before the company's FTA registration, or once the connector names no
// participant of the company, is left unprocessed for a later fetch. Run twice, the step changes and publishes nothing again
// and still counts the refusals as this fetch's; a later fetch finds
// nothing to process.
func TestProcessEInvoices(t *testing.T) {
	s := receiving(t)
	other := supplierInvoice("GOI-3")
	other.Buyer.TRN = "100234567899993" // the company's TIN, another TRN
	early := supplierInvoice("GOI-4")
	early.IssueDate = time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	elsewhere := supplierInvoice("GOI-5")
	elsewhere.Buyer.Endpoint = "1009999999"
	s.srv.Deliver("GOI-1", uaeParticipant, einvoicing.RenderPINTAE(supplierInvoice("GOI-1")))
	s.srv.Deliver("BAD", uaeParticipant, []byte(`<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"><ID>1</ID>`))
	s.srv.Deliver("GOI-2", "0235:9999999999", einvoicing.RenderPINTAE(supplierInvoice("GOI-2")))
	s.srv.Deliver("GOI-3", uaeParticipant, einvoicing.RenderPINTAE(other))
	s.srv.Deliver("GOI-4", uaeParticipant, einvoicing.RenderPINTAE(early))
	s.srv.Deliver("GOI-5", uaeParticipant, einvoicing.RenderPINTAE(elsewhere))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	ctx := context.Background()
	for run := range 2 {
		if refused, err := s.e.ProcessEInvoices(ctx, s.connector, op, s.within()); err != nil || refused != 4 {
			t.Fatalf("run %d: got %d refused (%v), want 4", run+1, refused, err)
		}
	}
	for message, want := range map[string]string{
		"GOI-1": "received - GOI-1 invoice 2026-03-10 AED 105000 " + einvoicing.PINTAERelease + " true",
		"BAD":   "refused einvoicing.unreadable - - - - - " + einvoicing.PINTAERelease + " true",
		"GOI-2": "refused einvoicing.not_addressed_to_company GOI-2 invoice 2026-03-10 AED 105000 " + einvoicing.PINTAERelease + " true",
		"GOI-3": "refused einvoicing.not_addressed_to_company GOI-3 invoice 2026-03-10 AED 105000 " + einvoicing.PINTAERelease + " true",
		"GOI-4": "received - - - - - - - false",
		"GOI-5": "refused einvoicing.not_addressed_to_company GOI-5 invoice 2026-03-10 AED 105000 " + einvoicing.PINTAERelease + " true",
	} {
		if got := s.processed(t, message, op); got != want {
			t.Errorf("%s: got %s, want %s", message, got, want)
		}
	}
	details := map[string]string{}
	for _, m := range []string{"GOI-2", "GOI-3", "GOI-5"} {
		var d string
		pgtest.Scan(t, `SELECT problem_detail FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = $2`, []any{s.f.Tenant.ID, m}, &d)
		details[m] = d
	}
	for message, want := range map[string]string{
		"GOI-2": "a participant of none of connector Gulf provider's companies", "GOI-3": "ending 9993, is not the number of company 5000's FTA registration",
		"GOI-5": "is 0235:1009999999, not company 5000's participant 0235:1002345678",
	} {
		if !strings.Contains(details[message], want) || strings.Contains(details[message], sellerTRN) {
			t.Errorf("%s's detail: got %q, want it naming %q and no TRN in clear", message, details[message], want)
		}
	}
	events := apitest.Published(t, s.f.Tenant, outbox.Event{Context: "einvoicing", Name: "einvoice_changed"})
	var refusals []string
	for _, e := range events {
		if e["status"] == einvoicing.StatusRefused {
			refusals = append(refusals, e["message_id"].(string)+" "+e["status_before"].(string)+" "+e["problem_code"].(string))
		}
	}
	if len(refusals) != 4 || refusals[0] != "BAD received einvoicing.unreadable" {
		t.Errorf("EInvoiceChanged of the refusals: got %v, want one per refused document, BAD's first", refusals)
	}
	var validatedAudits, refusedAudits int
	pgtest.Scan(t, `SELECT count(*) FILTER (WHERE action = 'einvoicing.einvoice.validated'), count(*) FILTER (WHERE action = 'einvoicing.einvoice.refused')
		FROM audit.audit_event WHERE tenant_id = $1 AND actor_id = $2`, []any{s.f.Tenant.ID, einvoicing.FetcherActor}, &validatedAudits, &refusedAudits)
	if validatedAudits != 1 || refusedAudits != 4 {
		t.Errorf("audit records: got %d validated and %d refused, want 1 and 4, once each", validatedAudits, refusedAudits)
	}
	t.Run("a gap in the company's setup leaves it for later", func(t *testing.T) {
		s.srv.Deliver("GOI-6", uaeParticipant, einvoicing.RenderPINTAE(supplierInvoice("GOI-6")))
		later := uuid.Must(uuid.NewV7())
		s.recordAll(t, later) // recorded for 5000, whose participant the connector then no longer names
		pgtest.Exec(t, `UPDATE integration.connector SET settings = jsonb_set(settings, '{participants}', '{"2000": "0235:2000000000"}'), version = version + 1
			WHERE id = $1`, s.connector)
		if _, err := s.e.ProcessEInvoices(ctx, s.connector, later, s.within()); err != nil {
			t.Fatal(err)
		}
		if got := s.processed(t, "GOI-6", later); got != "received - - - - - - - false" {
			t.Errorf("the company's participant gone: got %s, want it left unprocessed", got)
		}
	})
	if refused, err := s.e.ProcessEInvoices(ctx, s.connector, uuid.Must(uuid.NewV7()), s.within()); err != nil || refused != 0 {
		t.Errorf("a later fetch: got %d refused (%v), want none, nothing left to process", refused, err)
	}
}

// TestProcessedEInvoiceListed: the list and the document carry what
// processing read and decided (ADR-0061 §15).
func TestProcessedEInvoiceListed(t *testing.T) {
	s := receiving(t)
	s.srv.Deliver("GOI-1", uaeParticipant, einvoicing.RenderPINTAE(supplierInvoice("GOI-1")))
	s.srv.Deliver("BAD", uaeParticipant, []byte("not XML"))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	if _, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err != nil {
		t.Fatal(err)
	}
	var page *gen.EInvoicePage
	if err := apitest.InRequest(t, s.f.Tenant, s.f.Tenant.Human, func(ctx context.Context) error {
		res, err := s.e.ListEInvoices(ctx, gen.ListEInvoicesParams{ConnectorID: gen.NewOptUUID(s.connector)})
		page, _ = res.(*gen.EInvoicePage)
		return err
	}); err != nil || page == nil || len(page.Items) != 2 {
		t.Fatalf("the list: got %v %v, want two documents", page, err)
	}
	byID := map[string]gen.EInvoice{}
	for _, d := range page.Items {
		byID[d.MessageID] = d
	}
	good, bad := byID["GOI-1"], byID["BAD"]
	if got, want := strings.Join([]string{good.Status, good.DocumentNumber.Value, good.DocumentKind.Value, good.IssueDate.Value.Format(time.DateOnly),
		good.GrossAmount.Value.Amount, good.GrossAmount.Value.Currency, good.Specification.Value, good.ValidatedAgainst.Value,
		good.ProcessedFetchOperationID.Value.String()}, " "),
		"received GOI-1 invoice 2026-03-10 1050.00 AED "+einvoicing.PINTAECustomization+" "+einvoicing.PINTAERelease+" "+op.String(); got != want {
		t.Errorf("the valid document: got %s, want %s", got, want)
	}
	if good.ProblemCode.Set || !good.ProcessedAt.Set {
		t.Errorf("the valid document's problem and processing: got %v %v, want no problem, processed", good.ProblemCode, good.ProcessedAt)
	}
	if bad.Status != einvoicing.StatusRefused || bad.ProblemCode.Value != einvoicing.RefusedUnreadable || !bad.ProblemDetail.Set ||
		bad.DocumentNumber.Set || bad.GrossAmount.Set {
		t.Errorf("the refused document: got %s %v %v, want refused unreadable with its detail and nothing read", bad.Status, bad.ProblemCode,
			bad.DocumentNumber)
	}
}

// TestProcessEInvoicesFailures: a store that fails or holds other bytes
// fails the step, which a later run completes; a connector refused at use
// refuses it; every statement failing fails it and the step run again
// processes every document once (ADR-0061 §4).
func TestProcessEInvoicesFailures(t *testing.T) {
	s := receiving(t)
	ctx := context.Background()
	s.srv.Deliver("GOI-1", uaeParticipant, einvoicing.RenderPINTAE(supplierInvoice("GOI-1")))
	s.srv.Deliver("GOI-2", "", einvoicing.RenderPINTAE(supplierInvoice("GOI-2")))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	t.Run("the store failing", func(t *testing.T) {
		e := app.EInvoicing(s.cfg)
		e.Objects = failingGet{s.store}
		if _, err := e.ProcessEInvoices(ctx, s.connector, op, s.within()); err == nil || !strings.Contains(err.Error(), "store down") {
			t.Errorf("got %v, want the store's error", err)
		}
	})
	t.Run("the store holding other bytes or none", func(t *testing.T) {
		for name, store := range map[string]blobstore.Store{"other bytes": otherBytes{Store: s.store}, "none": blobstore.Dir{Root: t.TempDir()}} {
			e := app.EInvoicing(s.cfg)
			e.Objects = store
			_, err := e.ProcessEInvoices(ctx, s.connector, op, s.within())
			if p := problem.From(err); p.Code != "einvoicing.document_changed" || strings.Count(p.Detail, "-") < 8 {
				t.Errorf("%s: got %v, want einvoicing.document_changed naming both documents", name, err)
			}
		}
	})
	t.Run("a disabled connector", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE integration.connector SET status = 'disabled', version = version + 1 WHERE id = $1`, s.connector)
		defer pgtest.Exec(t, `UPDATE integration.connector SET status = 'active', version = version + 1 WHERE id = $1`, s.connector)
		if _, err := s.e.ProcessEInvoices(ctx, s.connector, op, s.within()); problem.From(err).Code != "integration.connector_disabled" {
			t.Errorf("got %v, want integration.connector_disabled", err)
		}
	})
	t.Run("failing statements", func(t *testing.T) {
		process := func(in einvoicing.Within) error {
			_, err := s.e.ProcessEInvoices(ctx, s.connector, op, in)
			return err
		}
		everyFailure(t, s, process) // until every document is processed
		everyFailure(t, s, process) // processed: nothing to process, the refusals counted
		if got := s.processed(t, "GOI-1", op) + ", " + s.processed(t, "GOI-2", op); !strings.HasPrefix(got, "received - GOI-1") ||
			!strings.Contains(got, "refused einvoicing.not_addressed_to_company GOI-2") {
			t.Errorf("after the failures: got %s, want GOI-1 valid and GOI-2 refused", got)
		}
	})
}

// TestProcessEInvoicesRace: a document another fetch processed between
// this fetch's listing and its lock is skipped, not processed twice
// (ADR-0008: read FOR UPDATE where still unprocessed).
func TestProcessEInvoicesRace(t *testing.T) {
	s := receiving(t)
	ctx := context.Background()
	s.srv.Deliver("BAD", uaeParticipant, []byte("not XML"))
	first, second := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	s.recordAll(t, first)
	e := app.EInvoicing(s.cfg)
	e.Objects = &racing{Store: s.store, before: func() {
		if refused, err := s.e.ProcessEInvoices(ctx, s.connector, second, s.within()); err != nil || refused != 1 {
			t.Errorf("the other fetch: got %d refused (%v), want 1", refused, err)
		}
	}}
	if refused, err := e.ProcessEInvoices(ctx, s.connector, first, s.within()); err != nil || refused != 0 {
		t.Errorf("this fetch: got %d refused (%v), want none, the other fetch's", refused, err)
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.refused'`, []any{s.f.Tenant.ID}, &audits)
	if audits != 1 {
		t.Errorf("refusals audited: got %d, want 1", audits)
	}
}

// racing is a store that runs before once, ahead of its first read.
type racing struct {
	blobstore.Store
	before func()
}

func (r *racing) Get(ctx context.Context, key string) ([]byte, string, error) {
	if r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.Store.Get(ctx, key)
}

// TestProcessEInvoicesGoesOn: a document whose stored bytes are gone does
// not stop the others; the step then fails naming it, with a problem no
// retry repairs (einvoicing.document_changed).
func TestProcessEInvoicesGoesOn(t *testing.T) {
	s := receiving(t)
	s.srv.Deliver("LOST", uaeParticipant, []byte("lost bytes"))
	s.srv.Deliver("GOI-1", uaeParticipant, einvoicing.RenderPINTAE(supplierInvoice("GOI-1")))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	e := app.EInvoicing(s.cfg)
	e.Objects = otherBytes{Store: s.store, only: "lost bytes"}
	_, err := e.ProcessEInvoices(context.Background(), s.connector, op, s.within())
	if problem.From(err).Code != "einvoicing.document_changed" {
		t.Errorf("got %v, want einvoicing.document_changed", err)
	}
	if got := s.processed(t, "GOI-1", op); !strings.HasPrefix(got, "received - GOI-1") || !strings.HasSuffix(got, " true") {
		t.Errorf("the other document: got %s, want it processed", got)
	}
	if got := s.processed(t, "LOST", op); got != "received - - - - - - - false" {
		t.Errorf("the lost document: got %s, want it left unprocessed", got)
	}
}

// failingGet is a store whose reads fail.
type failingGet struct{ blobstore.Store }

func (failingGet) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("store down")
}

// otherBytes is a store whose documents have other bytes than those kept:
// every one, or only the one of the bytes only.
type otherBytes struct {
	blobstore.Store
	only string
}

func (o otherBytes) Get(ctx context.Context, key string) ([]byte, string, error) {
	b, ct, err := o.Store.Get(ctx, key)
	if o.only != "" && string(b) != o.only {
		return b, ct, err
	}
	return append(b, ' '), ct, err
}
