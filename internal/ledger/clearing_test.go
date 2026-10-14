package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

type ref struct {
	entry map[string]any
	line  int
}

func clearing(le uuid.UUID, date string, refs ...ref) string {
	items := make([]string, len(refs))
	for i, r := range refs {
		items[i] = fmt.Sprintf(`{"fiscal_year": %v, "journal_entry_id": %q, "line_no": %d}`, r.entry["fiscal_year"], r.entry["id"], r.line)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": %q, "items": [%s]}`, le, date, strings.Join(items, ", "))
}

func TestClearing(t *testing.T) {
	e := setup(t)
	bank, expense, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "520000"), e.account(t, "2000", "211000")
	sup := e.supplier.String()
	invoice, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "100.00", ""}, line{ap, "-100.00", sup}), "")
	payment, payTag := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-20", "EUR", line{ap, "100.00", sup}, line{bank, "-100.00", ""}), "")
	other, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-25", "EUR", line{expense, "7.00", ""}, line{ap, "-7.00", e.vendor.String()}), "")

	items := func(q string) []string {
		page, _ := e.Must(200, "GET", "/v1/open-items?"+q, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			o := it.(map[string]any)
			out = append(out, o["amount"].(map[string]any)["amount"].(string)+"@"+o["posting_date"].(string))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			out = append(out, "next:"+tok)
		}
		return out
	}
	if got := strings.Join(items("partner_id="+sup), ","); got != "-100.00@2026-03-01,100.00@2026-03-20" {
		t.Fatalf("open items: %s", got)
	}
	if got := items("legal_entity_id=" + e.de.String() + "&account_id=" + ap + "&posting_date_to=2026-03-21&page_size=1"); len(got) != 2 {
		t.Fatalf("paged: %v", got)
	} else if rest := items("legal_entity_id=" + e.de.String() + "&account_id=" + ap + "&posting_date_to=2026-03-21&page_size=1&page_token=" +
		strings.TrimPrefix(got[1], "next:")); len(rest) != 1 {
		t.Fatalf("page 2: got %d items %v, want 1", len(rest), rest)
	} else if rest[0] != "100.00@2026-03-20" {
		t.Errorf("page 2: got %q, want 100.00@2026-03-20", rest[0])
	}

	refusals := map[string]struct {
		body   string
		status int
		code   string
	}{
		"duplicate":        {clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{invoice, 2}), 400, "request.invalid_body"},
		"unknown line":     {clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{invoice, 9}), 422, "ledger.not_an_open_item"},
		"expense line":     {clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{invoice, 1}), 422, "ledger.not_an_open_item"},
		"other company":    {clearing(e.us, "2026-03-31", ref{invoice, 2}, ref{payment, 1}), 422, "ledger.not_an_open_item"},
		"not balanced":     {clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{other, 2}), 422, "ledger.clearing_not_balanced"},
		"unknown entity":   {clearing(uuid.New(), "2026-03-31", ref{invoice, 2}, ref{payment, 1}), 422, "request.reference_not_found"},
		"no fiscal period": {clearing(e.de, "2031-03-31", ref{invoice, 2}, ref{payment, 1}), 422, "ledger.no_fiscal_period"},
	}
	for name, c := range refusals {
		t.Run(name, func(t *testing.T) {
			code, m, _ := e.in(t).Do("POST", "/v1/clearings", c.body, "")
			if code != c.status {
				t.Errorf("status: got %d, want %d (%v)", code, c.status, m)
			}
			if m["code"] != c.code {
				t.Errorf("code: got %v, want %s", m["code"], c.code)
			}
		})
	}

	c, tag := e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{payment, 1}), "")
	if got := len(c["items"].([]any)); got != 2 {
		t.Errorf("clearing items: got %d, want 2", got)
	}
	if c["legal_entity_id"] != e.de.String() {
		t.Errorf("clearing legal_entity_id: got %v, want %s", c["legal_entity_id"], e.de)
	}
	if len(tag) < 10 {
		t.Errorf("clearing tag: got %q, want at least 10 characters", tag)
	}
	ce, _ := e.Must(200, "GET", "/v1/journal-entries/"+c["clearing_entry_id"].(string), "", "")
	if ce["document_type"] != "AB" {
		t.Errorf("clearing entry document_type: got %v, want AB", ce["document_type"])
	}
	if ce["document_number"] != "100000" {
		t.Errorf("clearing entry document_number: got %v, want 100000", ce["document_number"])
	}
	if got := amounts(t, ce, "amount"); got != "100.00,-100.00" {
		t.Errorf("clearing entry amounts: got %s, want 100.00,-100.00", got)
	}
	if got := items("partner_id=" + sup); len(got) != 0 {
		t.Fatalf("still open: %v", got)
	}
	got, _ := e.Must(200, "GET", "/v1/clearings/"+c["id"].(string), "", "")
	if len(got["items"].([]any)) != 2 {
		t.Fatalf("get: %v", got)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/clearings/"+uuid.NewString(), "", "")
	cleared := events(t, e.ten, "items_cleared")
	if len(cleared) != 1 {
		t.Fatalf("items_cleared events: got %d %v, want 1", len(cleared), cleared)
	}
	if got := len(cleared[0]["items"].([]any)); got != 2 {
		t.Errorf("items_cleared event items: got %d, want 2", got)
	}
	e.Problem(409, "ledger.item_already_cleared", "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{payment, 1}), "")
	e.Problem(409, "ledger.entry_cleared", "POST", "/v1/journal-entries/"+payment["id"].(string)+":reverse", "", payTag)

	// Items of one clearing share their currency, and the clearing date must be open.
	e.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	dollars, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-02", "USD", line{expense, "7.00", ""}, line{ap, "7.00", e.vendor.String()},
		line{bank, "-14.00", ""}), "")
	e.Problem(422, "ledger.mixed_items", "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{other, 2}, ref{dollars, 2}), "")
	back, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-26", "EUR", line{ap, "7.00", e.vendor.String()}, line{bank, "-7.00", ""}), "")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND period = 4`, e.de)
	e.Problem(409, "ledger.period_not_open", "POST", "/v1/clearings", clearing(e.de, "2026-04-01", ref{other, 2}, ref{back, 1}), "")
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE account_id = $1`, ap)
	e.Problem(422, "ledger.account_not_postable", "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{other, 2}, ref{back, 1}), "")
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = false, version = version + 1 WHERE account_id = $1`, ap)
	e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{other, 2}, ref{back, 1}), "")

	list, _ := e.Must(200, "GET", "/v1/clearings?legal_entity_id="+e.de.String()+"&page_size=1", "", "")
	if got := len(list["items"].([]any)); got != 1 {
		t.Errorf("list items: got %d, want 1", got)
	}
	if list["next_page_token"] == nil {
		t.Fatalf("list: got no next_page_token, want one: %v", list)
	}
	rest, _ := e.Must(200, "GET", "/v1/clearings?legal_entity_id="+e.de.String()+"&page_size=1&page_token="+list["next_page_token"].(string), "", "")
	if got := len(rest["items"].([]any)); got != 1 {
		t.Errorf("rest items: got %d, want 1", got)
	}
	if rest["next_page_token"] != nil {
		t.Errorf("rest next_page_token: got %v, want none", rest["next_page_token"])
	}
	agent := e.Client
	agent.As = e.ten.Agent
	agent.Problem(404, "request.not_found", "GET", "/v1/clearings/"+c["id"].(string), "", "")
	if page, _ := agent.Must(200, "GET", "/v1/open-items", "", ""); len(page["items"].([]any)) != 0 {
		t.Fatalf("agent sees open items: %v", page)
	}
}

// TestPostingToANewSupplier goes through both contexts: a supplier requested
// through the partner API cannot be posted to until its create change is
// applied, and is blocked again by a partner block.
func TestPostingToANewSupplier(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT tenant_id, gen_random_uuid(), id, p, current_date - 1, created_by FROM identity.role, unnest(ARRAY['partner.supplier.create', 'partner.supplier.change', 'partner.supplier.read']) p
		WHERE tenant_id = $1 AND code = 'all'`, e.ten.ID)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'vendor_master_approver', 'Approver', 'active', $2, $2)`, e.ten.ID, e.ten.Human.ID)
	bank, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "211000")
	bp, _ := e.Must(201, "POST", "/v1/business-partners", fmt.Sprintf(`{"kind": "organization", "name": "Neu GmbH", "country_code": "DE", "roles": ["supplier"],
		"supplier_companies": [{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": "NT30"}], "reason": "New supplier"}`, e.de, ap), "")
	body := entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{ap, "-1.00", bp["id"].(string)})
	e.Problem(422, "ledger.partner_not_postable", "POST", "/v1/journal-entries", body, "")
	changes, _ := e.Must(200, "GET", "/v1/partner-changes?partner_id="+bp["id"].(string), "", "")
	change := uuid.MustParse(changes["items"].([]any)[0].(map[string]any)["id"].(string))
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := partner.ApplyChange(ctx, change)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.Must(201, "POST", "/v1/journal-entries", body, "")
	_, tag := e.Must(200, "GET", "/v1/business-partners/"+bp["id"].(string), "", "")
	e.Must(200, "POST", "/v1/business-partners/"+bp["id"].(string)+":block", `{"reason": "Fraud suspected"}`, tag)
	e.Problem(422, "ledger.partner_not_postable", "POST", "/v1/journal-entries", body, "")
}

// TestReversedItemsAreNotCleared refuses to clear, settle or pay the lines
// of a reversed entry or of its reversal: the reversal cancelled them.
func TestReversedItemsAreNotCleared(t *testing.T) {
	e := setup(t)
	bank, expense, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "520000"), e.account(t, "2000", "211000")
	invoice, tag := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "100.00", ""}, line{ap, "-100.00", e.supplier.String()}), "")
	reversal, _ := e.Must(200, "POST", "/v1/journal-entries/"+invoice["id"].(string)+":reverse", "", tag)
	e.Problem(409, "ledger.entry_reversed", "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{reversal, 2}), "")
	// What is not an open item at all, or not one of this legal entity, is refused as such first.
	e.Problem(422, "ledger.not_an_open_item", "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{invoice, 1}, ref{reversal, 1}), "")
	e.Problem(422, "ledger.not_an_open_item", "POST", "/v1/clearings", clearing(e.us, "2026-03-31", ref{invoice, 2}, ref{reversal, 2}), "")

	item := func(m map[string]any) ledger.ItemRef {
		return ledger.ItemRef{FiscalYear: int16(m["fiscal_year"].(float64)), EntryID: uuid.MustParse(m["id"].(string)), LineNo: 2}
	}
	text := "Payment"
	settle := func(items ...ledger.ItemRef) ledger.Settlement {
		return ledger.Settlement{LegalEntityID: e.de, DocumentType: "SA", PostingDate: date("2026-03-31"), Currency: "EUR",
			BankAccountID: uuid.MustParse(bank), HeaderText: &text, SourceContext: "payments", SourceDocumentID: uuid.New(), Items: items}
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		expense := ledger.ItemRef{FiscalYear: 2026, EntryID: item(invoice).EntryID, LineNo: 1}
		for name, c := range map[string]struct {
			call func() error
			code string
		}{
			"settle the original": {func() error { _, err := postings().Settle(ctx, settle(item(invoice))); return err }, "ledger.entry_reversed"},
			"settle the reversal": {func() error { _, err := postings().Settle(ctx, settle(item(reversal))); return err }, "ledger.entry_reversed"},
			"settle both": {func() error { _, err := postings().Settle(ctx, settle(item(invoice), item(reversal))); return err },
				"ledger.entry_reversed"},
			"clear by migration": {func() error {
				_, err := postings().Clear(ctx, e.de, date("2026-03-31"), item(reversal), []ledger.ItemRef{item(invoice), item(reversal)})
				return err
			}, "ledger.entry_reversed"},
			"settle an expense line": {func() error { _, err := postings().Settle(ctx, settle(expense)); return err }, "ledger.not_an_open_item"},
			"clear an expense line by migration": {func() error {
				_, err := postings().Clear(ctx, e.de, date("2026-03-31"), item(reversal), []ledger.ItemRef{expense})
				return err
			}, "ledger.not_an_open_item"},
		} {
			t.Run(name, func(t *testing.T) {
				if got := problem.From(c.call()).Code; got != c.code {
					t.Errorf("code: got %s, want %s", got, c.code)
				}
			})
		}
		items, err := ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-12-31"))
		if err != nil {
			return err
		}
		if len(items) != 0 {
			t.Errorf("payable items: got %+v, want none", items)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestReversalClearsThePair clears the open items of a reversed entry
// against the matching lines of its reversal, in the reversal's
// transaction, with the reversal as the clearing document: neither entry
// stays an open item, and the clearing balances.
func TestReversalClearsThePair(t *testing.T) {
	e := setup(t)
	expense, ap := e.account(t, "2000", "520000"), e.account(t, "2000", "211000")
	sup := e.supplier.String()
	invoice, tag := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "100.00", ""}, line{ap, "-60.00", sup},
		line{ap, "-40.00", sup}), "")
	reversal, _ := e.Must(200, "POST", "/v1/journal-entries/"+invoice["id"].(string)+":reverse", `{"posting_date": "2026-03-10"}`, tag)

	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+sup, "", "")
	if got := apitest.List(t, page, "items"); len(got) != 0 {
		t.Errorf("open items after the reversal: got %v, want none", got)
	}
	clearings, _ := e.Must(200, "GET", "/v1/clearings?legal_entity_id="+e.de.String(), "", "")
	list := apitest.List(t, clearings, "items")
	if len(list) != 1 {
		t.Fatalf("clearings: got %d %v, want 1", len(list), list)
	}
	c := list[0].(map[string]any)
	if c["clearing_entry_id"] != reversal["id"] {
		t.Errorf("clearing entry: got %v, want the reversal %v", c["clearing_entry_id"], reversal["id"])
	}
	if c["clearing_date"] != "2026-03-10" {
		t.Errorf("clearing date: got %v, want the reversal's posting date 2026-03-10", c["clearing_date"])
	}
	if got := len(apitest.List(t, c, "items")); got != 2 {
		t.Errorf("clearing items: got %d, want the original's 2 supplier lines", got)
	}
	var lines int
	var tc, lc, gc int64
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*), coalesce(sum(l.amount_tc), 0), coalesce(sum(l.amount_lc), 0),
			coalesce(sum(l.amount_gc), 0)
		FROM ledger.clearing_item i JOIN ledger.journal_line l ON l.tenant_id = i.tenant_id AND l.fiscal_year = i.fiscal_year
			AND l.journal_entry_id = i.journal_entry_id AND l.line_no = i.line_no
		WHERE i.tenant_id = $1 AND i.clearing_id = $2`, e.ten.ID, uuid.MustParse(c["id"].(string))).Scan(&lines, &tc, &lc, &gc); err != nil {
		t.Fatal(err)
	}
	if lines != 4 {
		t.Errorf("cleared lines: got %d, want 4 (two of the original, two of the reversal)", lines)
	}
	if tc != 0 || lc != 0 || gc != 0 {
		t.Errorf("cleared lines sum: got %d/%d/%d, want 0/0/0", tc, lc, gc)
	}
	cleared := events(t, e.ten, "items_cleared")
	if len(cleared) != 1 {
		t.Fatalf("items_cleared events: got %d %v, want 1", len(cleared), cleared)
	}
	if cleared[0]["clearing_id"] != c["id"] {
		t.Errorf("items_cleared clearing_id: got %v, want %v", cleared[0]["clearing_id"], c["id"])
	}
	// The reversal comes before what it caused.
	var order []string
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT event_type FROM ledger.outbox WHERE tenant_id = $1
		AND (event_type LIKE '%.journal_entry_reversed.%' OR event_type LIKE '%.items_cleared.%') ORDER BY id`, e.ten.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		order = append(order, typ)
	}
	if want := []string{"com.example.erp.ledger.journal_entry_reversed.v1", "com.example.erp.ledger.items_cleared.v1"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("event order: got %v, want %v", order, want)
	}
	var audited int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'ledger.clearing.create' AND object_id = $2`, e.ten.ID, uuid.MustParse(c["id"].(string))).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Errorf("audit records of the clearing: got %d, want 1", audited)
	}
	// Nothing clears the pair again.
	e.Problem(409, "ledger.entry_reversed", "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{reversal, 2}), "")
}
