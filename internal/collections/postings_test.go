package collections_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// posting is charged's dunning posted (T76h): the charges to 480000
// Dunning charges and the interest to 481000 Interest on arrears, both
// revenue, with the number range a debit memo's entry is numbered from.
func posting(doc *config.Config) {
	charged(doc)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "480000", Name: "Dunning charges", Type: config.Revenue},
		config.Account{Number: "481000", Name: "Interest on arrears", Type: config.Revenue})
	de.AccountDetermination.DunningCharges, de.AccountDetermination.DunningInterest = "480000", "481000"
	for _, y := range fixture.Years() {
		doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_dd", FiscalYear: y, First: 100000, Last: 999999})
	}
}

// postingEnv is setupWith(posting) with the customers' data in company
// 2000, which a debit memo posts with: the receivables account 120000 and
// NT30.
func postingEnv(t *testing.T) env {
	t.Helper()
	e := setupWith(t, posting)
	for _, c := range []uuid.UUID{e.north, e.south, e.east} {
		e.companyData(t, c)
	}
	return e
}

// companyData sets the customer up in company 2000, as an approved
// partner change would (T74a).
func (e env) companyData(t *testing.T, customer uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, customer, e.DE, e.receivable, e.Tenant.Human.ID)
}

// memos are the tenant's debit memos as "customer number gross source
// status", by document number.
func (e env) memos(t *testing.T) string {
	t.Helper()
	var out *string
	pgtest.Scan(t, `SELECT string_agg(b.number || ' ' || i.gross_amount || ' ' || i.source_document_id || ' ' || i.status, ', ' ORDER BY i.document_number)
		FROM receivables.customer_invoice i JOIN partner.business_partner b ON b.tenant_id = i.tenant_id AND b.id = i.customer_id
		WHERE i.tenant_id = $1 AND i.kind = 'debit_memo'`, []any{e.Tenant.ID}, &out)
	if out == nil {
		return ""
	}
	return *out
}

// lines are the journal lines of a debit memo's entry as "account amount
// partner?", credits negative, by line.
func (e env) lines(t *testing.T, entry string) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_tc || CASE WHEN l.partner_id IS NULL THEN '' ELSE ' customer' END, ', ' ORDER BY l.line_no)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{e.Tenant.ID, entry}, &out)
	return out
}

// TestChargesPosted (T76h acceptance): confirming a run posts each notice
// charged anything as one customer debit memo naming the notice, an open
// item of the customer, the charge and the interest each on its account;
// the notice names its memo, the run counts them and DunningNoticeIssued
// carries it; a notice charged nothing posts none; a second confirmation
// posts nothing more.
func TestChargesPosted(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	// Southwind's notice is charged level 1's 5.00 and no interest; Northwind's is below the minimum.
	id, tag := e.start(t, keyDate)
	confirmed := e.confirm(t, id, tag)
	if got := confirmed["charge_document_count"]; got != float64(1) {
		t.Errorf("the run's memos: got %v, want 1", got)
	}
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	south := objects(t, page, "items")[0]
	doc := apitest.Object(t, south, "charge_document")
	var memo string
	pgtest.Scan(t, `SELECT concat_ws(' ', kind, status, document_number, invoice_date, gross_amount, tax_amount, source_document_id, source_document_number,
			posting_date, journal_entry_id) FROM receivables.customer_invoice WHERE tenant_id = $1 AND id = $2`,
		[]any{e.Tenant.ID, apitest.String(t, doc, "customer_invoice_id")}, &memo)
	if want := strings.Join([]string{"debit_memo posted", apitest.String(t, doc, "customer_invoice_number"), keyDate, "500 0", apitest.String(t, south, "id"),
		apitest.String(t, south, "id"), apitest.String(t, doc, "posting_date"), apitest.String(t, doc, "journal_entry_id")}, " "); memo != want {
		t.Errorf("the memo: got %s, want %s", memo, want)
	}
	if got := e.lines(t, apitest.String(t, doc, "journal_entry_id")); got != "120000 500 customer, 480000 -500" {
		t.Errorf("the memo's entry: got %q, want the customer debited and the charge account credited", got)
	}
	issued := e.issued(t)
	if len(issued) != 1 || issued[0]["customer_invoice_id"] != doc["customer_invoice_id"] || issued[0]["customer_invoice_number"] != doc["customer_invoice_number"] {
		t.Errorf("DunningNoticeIssued: got %v, want Southwind's naming memo %v", issued, doc["customer_invoice_number"])
	}
	e.Problem(409, "collections.invalid_status", "POST", "/v1/dunning-runs/"+id+":confirm", "", `"2"`)

	// A month later Southwind is at level 2, 15.00 and 3.59 of interest; Northwind at level 1, 5.00; Eastwind below the minimum.
	id, tag = e.start(t, "2026-07-30")
	if got := e.confirm(t, id, tag)["charge_document_count"]; got != float64(2) {
		t.Errorf("the second run's memos: got %v, want 2", got)
	}
	page, _ = e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	notices := objects(t, page, "items")
	if got := e.lines(t, apitest.String(t, apitest.Object(t, notices[0], "charge_document"), "journal_entry_id")); got != "120000 1859 customer, 480000 -1500, 481000 -359" {
		t.Errorf("Southwind's second memo: got %q, want the charge and the interest on their accounts", got)
	}
	want := "BP70000001 500 " + apitest.String(t, south, "id") + " posted, BP70000001 1859 " + apitest.String(t, notices[0], "id") +
		" posted, BP70000002 500 " + apitest.String(t, notices[1], "id") + " posted"
	if got := e.memos(t); got != want {
		t.Errorf("the debit memos: got %s, want %s", got, want)
	}
	// The first memo, dated 30 June on NT30, is an open item of Southwind's, due on 30 July: not overdue on the key date.
	if !strings.Contains(e.openItems(t), apitest.String(t, doc, "journal_entry_id")) {
		t.Error("the first memo: not among the open items, want the customer's open item")
	}
}

// TestChargesNotPosted: a company that names no dunning account posts
// nothing, whatever its notices charge (T76d's behaviour); a company the
// configuration lacks posts nothing either, and neither does a notice
// charged nothing in a company that posts.
func TestChargesNotPosted(t *testing.T) {
	e := setupWith(t, charged)
	e.story(t)
	id, tag := e.start(t, keyDate)
	if got := e.confirm(t, id, tag)["charge_document_count"]; got != float64(0) {
		t.Errorf("memos without the accounts: got %v, want 0", got)
	}
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	if _, ok := objects(t, page, "items")[0]["charge_document"]; ok {
		t.Error("the notice: got a charge document, want none")
	}
	f := postingEnv(t)
	f.story(t)
	id, tag = f.start(t, keyDate)
	h := endpoints(t)
	h.Settings = noCompany{}
	if err := f.inRequest(t, func(ctx context.Context) error {
		_, err := h.ConfirmDunningRun(ctx, gen.ConfirmDunningRunParams{ID: uuid.MustParse(id), IfMatch: tag})
		return err
	}); err != nil {
		t.Fatalf("a company the configuration lacks: got %v, want the run confirmed", err)
	}
	free := setupWith(t, func(doc *config.Config) {
		posting(doc)
		doc.Company("2000").Dunning.Levels[0].Charge = ""
	})
	free.story(t)
	free.companyData(t, free.south)
	id, tag = free.start(t, keyDate)
	if got := free.confirm(t, id, tag)["charge_document_count"]; got != float64(0) {
		t.Errorf("memos of a notice charged nothing: got %v, want 0", got)
	}
	if got := e.memos(t) + f.memos(t) + free.memos(t); got != "" {
		t.Errorf("debit memos: got %s, want none", got)
	}
}

// withoutInterest is the configuration with company 2000's interest
// account removed and no level charging interest: as it reads after a
// change made since a run was proposed.
type withoutInterest struct{ collections.Settings }

func (s withoutInterest) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, v, err := s.Settings.Document(ctx)
	if err != nil {
		return nil, 0, err
	}
	changed := *doc
	changed.Companies = append([]config.Company(nil), doc.Companies...)
	changed.Company("2000").AccountDetermination.DunningInterest = ""
	return &changed, v, nil
}

// TestChargeRefusals: what receivables refuses refuses the confirmation
// with its code, the customer's number leading the detail and no field of
// the memo's, and nothing is recorded or posted; so is interest whose
// account the company no longer names.
func TestChargeRefusals(t *testing.T) {
	t.Run("a customer without data in the company", func(t *testing.T) {
		e := setupWith(t, posting)
		e.story(t)
		id, tag := e.start(t, keyDate)
		p := e.Problem(422, "receivables.customer_not_usable", "POST", "/v1/dunning-runs/"+id+":confirm", "", tag)
		if detail, _ := p["detail"].(string); !strings.HasPrefix(detail, "The dunning charge of customer BP70000001 cannot be posted: ") {
			t.Errorf("detail: got %q, want it led by the customer", detail)
		}
		if _, ok := p["errors"]; ok {
			t.Errorf("field errors: got %v, want none", p["errors"])
		}
		run, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id, "", "")
		if got := fields(t, run, "status", "charge_document_count") + " " + e.dunned(t) + e.memos(t); got != "status proposed, charge_document_count 0 " {
			t.Errorf("after the refusal: got %q, want the run proposed and nothing recorded or posted", got)
		}
	})
	t.Run("interest whose account is no longer named", func(t *testing.T) {
		e := postingEnv(t)
		e.story(t)
		first, firstTag := e.start(t, keyDate)
		e.confirm(t, first, firstTag)
		id, tag := e.start(t, "2026-07-30")
		h := endpoints(t)
		h.Settings = withoutInterest{h.Settings}
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ConfirmDunningRun(ctx, gen.ConfirmDunningRunParams{ID: uuid.MustParse(id), IfMatch: tag})
			return err
		})
		if p := problem.From(err); p.Code != "collections.no_charge_account" || p.Status != 422 ||
			!strings.Contains(p.Detail, "customer BP70000001 charges interest on overdue items to 2026-07-30") {
			t.Errorf("got %d %s %q, want 422 collections.no_charge_account naming Southwind's interest", p.Status, p.Code, p.Detail)
		}
	})
}

// TestChargeFailures: confirming a run that posts, and reading its
// notices with their memos, with each database statement failing in
// turn; and with the configuration's read failing.
func TestChargeFailures(t *testing.T) {
	e := postingEnv(t)
	e.story(t)
	id, _ := e.start(t, keyDate)
	h := endpoints(t)
	confirm := func(ctx context.Context) error {
		_, err := h.ConfirmDunningRun(ctx, gen.ConfirmDunningRunParams{ID: uuid.MustParse(id), IfMatch: `"1"`})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "confirm and post", confirm)
	if err := e.inRequest(t, confirm); err != nil {
		t.Fatalf("the confirmation: got %v, want it posted", err)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "notices with memos", func(ctx context.Context) error {
		_, err := h.ListDunningNotices(ctx, gen.ListDunningNoticesParams{ID: uuid.MustParse(id)})
		return err
	})
}
