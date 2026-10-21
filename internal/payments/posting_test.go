package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// postRules are matchRules with approval tiers: a line of up to 50.00
// posts at once, a larger one waits for the controller's approval.
var postRules = strings.Replace(matchRules, `"matching_rules"`,
	`"approval_tiers": [{"up_to": "50.00"}, {"steps": [{"role": "controller", "approvals": 1}]}], "matching_rules"`, 1)

// postScenario is the matching scenario of a house bank whose statements
// post: its main bank account is 101000, its clearing account is
// open-item managed, and the tiers are postRules.
func postScenario(t *testing.T) matchScenario {
	t.Helper()
	e := matchSetupWith(t, "payments.statement.post")
	e.statementRules(t, postRules)
	e.bankPosts(t)
	return matchStatementOf(t, e)
}

// bankPosts gives house bank MAIN its main bank account 101000 and makes
// its clearing account open-item managed, as an activated configuration
// would (ADR-0047).
func (e env) bankPosts(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `UPDATE payments.house_bank SET main_gl_account_id = $2, version = version + 1 WHERE id = $1`, e.bank, e.Account(t, "2000", "101000"))
	pgtest.Exec(t, `UPDATE ledger.company_account ca SET is_open_item_managed = true, version = ca.version + 1 FROM payments.house_bank h
		WHERE h.id = $1 AND ca.legal_entity_id = h.legal_entity_id AND ca.account_id = h.gl_account_id`, e.bank)
}

// sc helpers: the path of a line's action, the statement's ETag, and a
// line's live match.
func (sc matchScenario) line(n int, action string) string {
	return fmt.Sprintf("/v1/bank-statements/%s/lines/%d:%s", sc.statement, n, action)
}

func (sc matchScenario) tag(t *testing.T) string {
	t.Helper()
	_, tag := sc.e.in(t).Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	return tag
}

func (sc matchScenario) live(t *testing.T, n int) map[string]any {
	t.Helper()
	ms := sc.e.in(t).matchesOf(t, sc.statement, n)
	return ms[len(ms)-1].(map[string]any)
}

// posting is what a posted line's journal entry booked, by account number.
func posting(t *testing.T, entry string) map[string]money.Minor {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT a.number, l.amount_tc FROM ledger.journal_line l
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id WHERE l.journal_entry_id = $1 ORDER BY l.line_no`, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]money.Minor{}
	for rows.Next() {
		var number string
		var amount money.Minor
		if err := rows.Scan(&number, &amount); err != nil {
			t.Fatal(err)
		}
		out[number] += amount
	}
	return out
}

// wantPosting checks what the entry booked on each account.
func wantPosting(t *testing.T, what, entry string, want map[string]money.Minor) {
	t.Helper()
	got := posting(t, entry)
	for account, amount := range want {
		if got[account] != amount {
			t.Errorf("%s posting on %s: got %d, want %d", what, account, got[account], amount)
		}
	}
}

// clearingBalance is what the payment's entry and the line's posting
// leave on the house bank's clearing account, and how many of their lines
// on it are cleared.
func (e env) clearingBalance(t *testing.T, payment, entry string) (money.Minor, int) {
	t.Helper()
	var balance money.Minor
	var cleared int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT coalesce(sum(l.amount_tc), 0), count(ci.line_no) FROM ledger.journal_line l
		JOIN payments.house_bank h ON h.tenant_id = l.tenant_id AND h.gl_account_id = l.account_id AND h.id = $3
		LEFT JOIN ledger.clearing_item ci ON ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no
		WHERE l.journal_entry_id IN ((SELECT journal_entry_id FROM payments.payment WHERE id = $1), $2)`, payment, entry, e.bank).Scan(&balance, &cleared); err != nil {
		t.Fatal(err)
	}
	return balance, cleared
}

// approve has the approver approve the match's pending posting request.
func (e env) approve(t *testing.T, approver pgtest.Actor, match string) {
	t.Helper()
	r := e.Pending(t, match)
	e.As(approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
}

// TestPostStatementLines: on import the lines the rules matched post, at
// once within the first tier and after the controller's approval above it;
// a posting clears the payment's line on the clearing account, a posting
// rule's posts to its account; a rejected posting returns the line to
// matched, and a person posts it again (ADR-0047, T49f).
func TestPostStatementLines(t *testing.T) {
	sc := postScenario(t)
	e, pays := sc.e, sc.pays
	want := []string{"pending_approval", "pending_approval", "posted", "proposed", "unmatched", "unmatched", "unmatched", "proposed", "proposed"}
	if got := e.lineStatuses(t, sc.statement); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("line statuses after import: got %v, want %v", got, want)
	}
	charges := sc.live(t, 3)
	wantPosting(t, "charges", charges["journal_entry_id"].(string), map[string]money.Minor{"101000": -1050, "520000": 1050})
	if charges["posting_date"] != "2026-04-17" {
		t.Errorf("charges posting date: got %v, want the booking date", charges["posting_date"])
	}
	first := sc.live(t, 1)
	e.approve(t, e.Approver, id(first))
	first = sc.live(t, 1)
	wantPosting(t, "payment", first["journal_entry_id"].(string), map[string]money.Minor{"100000": 10000, "101000": -10000})
	balance, cleared := e.clearingBalance(t, pays[0].payment, first["journal_entry_id"].(string))
	if balance != 0 {
		t.Errorf("clearing account balance of the payment and its posting: got %d, want 0", balance)
	}
	if cleared != 2 {
		t.Errorf("cleared lines on the clearing account: got %d, want 2", cleared)
	}
	batch := sc.live(t, 2)
	r := e.Pending(t, id(batch))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "Wrong batch."}`, fmt.Sprintf(`"%v"`, r["version"]))
	if got := e.lineStatuses(t, sc.statement)[1]; got != "matched" {
		t.Errorf("line 2 after its posting was rejected: got %s, want matched", got)
	}
	_, tag := e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	if got := e.lineStatuses(t, sc.statement)[1]; got != "matched" {
		t.Errorf("line 2 after a run of the rules: got %s, want matched, not submitted again", got)
	}
	m, next := e.Must(200, "POST", sc.line(2, "post"), `{"posting_date": "2026-04-30", "reason": "Booked on the month's last day."}`, tag)
	if m["posting_date"] != "2026-04-30" {
		t.Errorf("posting date: got %v, want 2026-04-30", m["posting_date"])
	}
	if m["posting_reason"] != "Booked on the month's last day." {
		t.Errorf("posting reason: got %v, want the person's", m["posting_reason"])
	}
	if m["approval_request_id"] == r["id"] {
		t.Errorf("approval request: got the rejected %v, want a new one", m["approval_request_id"])
	}
	if next == tag {
		t.Errorf("ETag after posting: got %s, want a new one", next)
	}
	e.approve(t, e.Approver2, id(batch))
	if got := e.lineStatuses(t, sc.statement)[:3]; strings.Join(got, ",") != "posted,posted,posted" {
		t.Errorf("lines 1-3: got %v, want posted", got)
	}
	if got := len(events(t, e.Tenant, "bank_statement_line_posted")); got != 3 {
		t.Errorf("posted events: got %d, want 3", got)
	}
	t.Run("again", func(t *testing.T) {
		before := sc.tag(t)
		if _, after := e.in(t).Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", before); after != before {
			t.Errorf("a run after the postings: got ETag %s, want %s unchanged", after, before)
		}
		e.in(t).Problem(409, "payments.statement_line_posting", "POST", sc.line(1, "post"), "", before)
	})
}

// TestPostStatementLineByHand: a person posts a line matched by hand; whoever
// matched it cannot approve the posting; what cannot be posted is refused
// (ADR-0047, T49f).
func TestPostStatementLineByHand(t *testing.T) {
	sc := postScenario(t)
	e, pays := sc.e, sc.pays
	_, tag := e.As(e.Approver).Must(201, "POST", sc.line(4, "match"), `{"payment_ids": ["`+pays[2].payment+`"], "reason": "Remittance names INV-3."}`, sc.tag(t))
	m, _ := e.Must(200, "POST", sc.line(4, "post"), "", tag)
	if m["approval_request_id"] == nil {
		t.Fatalf("approval request of a posting above the first tier: got none, want one (%v)", m)
	}
	if m["journal_entry_id"] != nil {
		t.Errorf("journal entry of a posting waiting for approval: got %v, want none", m["journal_entry_id"])
	}
	r := e.Pending(t, id(m))
	e.As(e.Approver).Problem(403, "workflow.segregation_of_duties", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	e.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
	e.approve(t, e.Approver2, id(m))
	if got := e.lineStatuses(t, sc.statement)[3]; got != "posted" {
		t.Errorf("line 4: got %s, want posted", got)
	}
	_, tag = e.Must(201, "POST", sc.line(8, "match"), `{"payment_ids": ["`+pays[3].payment+`"], "reason": "x"}`, sc.tag(t))
	_, tag = e.Must(201, "POST", sc.line(7, "match"), `{"posting_rule": "NCHG", "reason": "Charges reversed."}`, tag)
	stale := tag
	_, tag = e.Must(201, "POST", sc.line(6, "exclude"), `{"reason": "x"}`, tag)
	for name, c := range map[string]struct {
		status     int
		code, path string
		body       string
		ifMatch    string
		asTheAgent bool
	}{
		"a statement changed since it was read": {412, "request.version_mismatch", sc.line(7, "post"), "", stale, false},
		"a reason without a date":               {422, "request.validation_failed", sc.line(7, "post"), `{"reason": "x"}`, tag, false},
		"a date without a reason":               {422, "request.validation_failed", sc.line(7, "post"), `{"posting_date": "2026-05-04"}`, tag, false},
		"no such line":                          {404, "request.not_found", sc.line(99, "post"), "", tag, false},
		"an unmatched line":                     {409, "payments.statement_line_not_matched", sc.line(5, "post"), "", tag, false},
		"an excluded line":                      {409, "payments.statement_line_not_matched", sc.line(6, "post"), "", tag, false},
		"a posted line":                         {409, "payments.statement_line_posting", sc.line(3, "post"), "", tag, false},
		"payments not paying the line":          {409, "payments.statement_amount_differs", sc.line(8, "post"), "", tag, false},
		"an agent":                              {403, "identity.agent_not_allowed", sc.line(7, "post"), "", tag, true},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := e.in(t)
			if c.asTheAgent {
				c2 = c2.agent()
			}
			c2.Problem(c.status, c.code, "POST", c.path, c.body, c.ifMatch)
		})
	}
	m, _ = e.Must(200, "POST", sc.line(7, "post"), "", tag)
	wantPosting(t, "a reversed credit to charges", m["journal_entry_id"].(string), map[string]money.Minor{"101000": -1050, "520000": 1050})
}

// TestPostStatementLineRefused: a locked period refuses a posting (another
// date in an open period posts it), and refuses the approval of one
// submitted before; a bank in gap, without a main account, or whose
// clearing account is not open-item managed posts nothing, and a posting
// rule the company dropped refuses the line's posting (ADR-0047, T49f).
func TestPostStatementLineRefused(t *testing.T) {
	sc := postScenario(t)
	e := sc.e
	for _, status := range []string{"closed", "locked"} {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND period = 4`, e.Tenant.ID, e.DE, status)
	}
	r := e.Pending(t, id(sc.live(t, 1)))
	e.As(e.Approver).Problem(409, "ledger.period_not_open", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	e.Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":cancel", `{"comment": "April is locked."}`, fmt.Sprintf(`"%v"`, r["version"]))
	if got := e.lineStatuses(t, sc.statement)[0]; got != "matched" {
		t.Errorf("line 1 after its request was withdrawn: got %s, want matched", got)
	}
	e.Problem(409, "ledger.period_not_open", "POST", sc.line(1, "post"), "", sc.tag(t))
	m, _ := e.Must(200, "POST", sc.line(1, "post"), `{"posting_date": "2026-05-04", "reason": "April is locked."}`, sc.tag(t))
	e.approve(t, e.Approver, id(m))
	if got := e.lineStatuses(t, sc.statement)[0]; got != "posted" {
		t.Errorf("line 1 on another date: got %s, want posted", got)
	}
	_, tag := e.Must(201, "POST", sc.line(7, "match"), `{"posting_rule": "NCHG", "reason": "x"}`, sc.tag(t))
	t.Run("not open-item managed", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_open_item_managed = false, version = version + 1 WHERE legal_entity_id = $1`, e.DE)
		defer e.bankPosts(t)
		r := e.in(t).Pending(t, id(sc.live(t, 2)))
		e.in(t).As(e.Approver).Problem(422, "ledger.not_open_item_managed", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve",
			`{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
	})
	t.Run("a posting rule dropped", func(t *testing.T) {
		e.statementRules(t, `{"matching_rules": [{"kind": "end_to_end_id"}]}`)
		defer e.statementRules(t, postRules)
		e.in(t).Problem(409, "payments.statement_posting_rule_gone", "POST", sc.line(7, "post"), "", tag)
	})
	t.Run("no main bank account", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE payments.house_bank SET main_gl_account_id = NULL, version = version + 1 WHERE id = $1`, e.bank)
		defer e.bankPosts(t)
		e.in(t).Problem(409, "payments.statement_bank_not_posting", "POST", sc.line(7, "post"), "", tag)
	})
	gap := e.imported(t, "mt940", mtStatement("M3", "3", "1,00", "260420", "1,00", "260420"))[0].(map[string]any)
	if gap["status"] != "gap" {
		t.Fatalf("statement: got %v, want a gap", gap["status"])
	}
	e.Problem(409, "payments.statement_gap", "POST", sc.line(7, "post"), "", sc.tag(t))
}

// TestAutomaticPostingWaits: a line the rules matched while the bank could
// not post is posted by the next run of the rules, not by an agent's run
// (agents never post), and not while the bank has a statement in gap.
func TestAutomaticPostingWaits(t *testing.T) {
	e := matchSetupWith(t, "payments.statement.post")
	e.statementRules(t, postRules)
	sc := matchStatementOf(t, e)
	if got := e.lineStatuses(t, sc.statement)[:3]; strings.Join(got, ",") != "matched,matched,matched" {
		t.Fatalf("lines 1-3 of a bank without a main account: got %v, want matched", got)
	}
	e.bankPosts(t)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	tag := sc.tag(t)
	if _, after := e.agent().Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", tag); after != tag {
		t.Errorf("an agent's run: got ETag %s, want %s: nothing posted", after, tag)
	}
	pgtest.Exec(t, `UPDATE payments.bank_statement SET status = 'gap', previous_statement_id = id, version = version + 1 WHERE id = $1`, sc.statement)
	tag = sc.tag(t)
	if _, after := e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", tag); after != tag {
		t.Errorf("a run with a gap: got ETag %s, want %s: nothing posted", after, tag)
	}
	pgtest.Exec(t, `UPDATE payments.bank_statement SET status = 'imported', previous_statement_id = NULL, version = version + 1 WHERE id = $1`, sc.statement)
	e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	if got := e.lineStatuses(t, sc.statement)[:3]; strings.Join(got, ",") != "pending_approval,pending_approval,posted" {
		t.Errorf("lines 1-3 after a person's run: got %v, want posted or waiting for approval", got)
	}
}

// TestBankPostingLedgerRefusals: the ledger clears only open lines of the
// entries on the clearing account, which total the bank's line; a line
// cleared or reversed is not cleared again.
func TestBankPostingLedgerRefusals(t *testing.T) {
	sc := postScenario(t)
	e, pays := sc.e, sc.pays
	charges := sc.live(t, 3)
	var payEntry, chargesEntry ledger.ItemRef
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT p.journal_entry_year, p.journal_entry_id, m.journal_entry_year, m.journal_entry_id
		FROM payments.payment p, payments.statement_match m WHERE p.id = $1 AND m.id = $2`, pays[3].payment, id(charges)).
		Scan(&payEntry.FiscalYear, &payEntry.EntryID, &chargesEntry.FiscalYear, &chargesEntry.EntryID); err != nil {
		t.Fatal(err)
	}
	var clearing uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT gl_account_id FROM payments.house_bank WHERE id = $1`, e.bank).Scan(&clearing); err != nil {
		t.Fatal(err)
	}
	postings := ledger.Postings{LegalEntities: tenancy.Directory{}, Numbers: configuration.Numbers{}}
	post := func(amount money.Minor, entries ...ledger.ItemRef) error {
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := postings.PostBank(ctx, ledger.BankPosting{LegalEntityID: e.DE, DocumentType: "SA", PostingDate: date("2026-04-17"),
				Currency: "EUR", BankAccountID: e.Account(t, "2000", "101000"), Amount: amount, ClearingAccountID: clearing, Entries: entries,
				SourceContext: "payments", SourceDocumentID: uuid.New()})
			return err
		})
	}
	for name, c := range map[string]struct {
		amount  money.Minor
		entries []ledger.ItemRef
		code    string
	}{
		"an entry without a line on the account": {-1050, []ledger.ItemRef{chargesEntry}, "ledger.not_an_open_item"},
		"items that do not total the line":       {-9999, []ledger.ItemRef{payEntry}, "ledger.clearing_not_balanced"},
	} {
		t.Run(name, func(t *testing.T) { pgtest.WantErr(t, post(c.amount, c.entries...), c.code) })
	}
	if err := post(-10000, payEntry); err != nil {
		t.Fatal(err)
	}
	t.Run("cleared already", func(t *testing.T) { pgtest.WantErr(t, post(-10000, payEntry), "ledger.item_already_cleared") })
	manual, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-04-17", "posting_date": "2026-04-17",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "-50.00", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "50.00", "currency": "EUR"}}]}`, e.DE, clearing, e.Account(t, "2000", "101000")), "")
	_, jtag := e.Must(200, "GET", "/v1/journal-entries/"+id(manual), "", "")
	e.Must(200, "POST", "/v1/journal-entries/"+id(manual)+":reverse", "", jtag)
	reversed := ledger.ItemRef{FiscalYear: 2026, EntryID: uuidOf(id(manual))}
	t.Run("reversed", func(t *testing.T) { pgtest.WantErr(t, post(-5000, reversed), "ledger.entry_reversed") })
}

// TestBankClearingTwice: the approval's callback posts a line once; a
// second call for the same request posts nothing and fails (run twice).
func TestBankClearingTwice(t *testing.T) {
	sc := postScenario(t)
	e := sc.e
	m := sc.live(t, 1)
	r := e.Pending(t, id(m))
	clearings := payments.BankClearings{Endpoints: endpoints(nil)}
	approved := func() error {
		return apitest.Committed(t, e.Tenant, e.Approver, func(ctx context.Context) error {
			return clearings.Approved(ctx, uuidOf(id(m)), uuidOf(r["id"].(string)))
		})
	}
	if err := approved(); err != nil {
		t.Fatal(err)
	}
	pgtest.WantErr(t, approved(), "does not wait for request")
	var entries int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM ledger.journal_entry WHERE source_document_id = $1`, id(m)).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Errorf("entries of the line: got %d, want 1", entries)
	}
}

// TestPostingDatabaseFailures: every statement of posting fails in turn,
// and none runs outside a unit of work.
func TestPostingDatabaseFailures(t *testing.T) {
	sc := postScenario(t)
	e, pays := sc.e, sc.pays
	h := endpoints(nil)
	h.Objects = e.objects
	st := uuidOf(sc.statement)
	e.Must(201, "POST", sc.line(7, "match"), `{"posting_rule": "NCHG", "reason": "x"}`, sc.tag(t))
	e.Must(201, "POST", sc.line(4, "match"), `{"payment_ids": ["`+pays[2].payment+`"], "reason": "x"}`, sc.tag(t))
	first, second := sc.live(t, 1), sc.live(t, 2)
	request := uuidOf(e.Pending(t, id(first))["id"].(string))
	clearings := payments.BankClearings{Endpoints: h}
	file := mtStatement("M2", "2", "429,70", "260418", "419,20", "260418", ":61:2604180418D10,50NCHGNONREF//C1\n")
	for name, fn := range map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := h.PostLine(ctx, st, 7, anyVersion, payments.LinePosting{})
			return err
		},
		"submit": func(ctx context.Context) error {
			_, err := h.PostLine(ctx, st, 4, anyVersion, payments.LinePosting{})
			return err
		},
		"import posted": func(ctx context.Context) error {
			_, _, _, err := h.ImportStatements(ctx, e.bank, "mt940", []byte(file))
			return err
		},
		"approved": func(ctx context.Context) error { return clearings.Approved(ctx, uuidOf(id(first)), request) },
		"rejected": func(ctx context.Context) error {
			return clearings.Rejected(ctx, uuidOf(id(first)), request, "x")
		},
		"barred": func(ctx context.Context) error {
			_, err := clearings.Barred(ctx, uuidOf(id(second)), e.Approver.ID)
			return err
		},
		"approve": func(ctx context.Context) error {
			decider := approvals.Decider{Roles: identity.Directory{}, Handlers: map[string]approvals.Handler{approvals.BankClearing: clearings}}
			_, err := decider.Approve(ctx, request, anyVersion, "ok")
			return err
		},
		"handler": func(ctx context.Context) error {
			_, err := h.PostBankStatementLine(ctx, gen.OptStatementLinePostInput{}, gen.PostBankStatementLineParams{ID: st, LineNo: 7, IfMatch: "*"})
			return err
		},
	} {
		apitest.EveryFailure(t, e.Tenant, e.Approver, name, fn)
	}
	for name, err := range map[string]error{
		"post": func() error {
			_, err := h.PostLine(context.Background(), st, 7, anyVersion, payments.LinePosting{})
			return err
		}(),
		"approved": clearings.Approved(context.Background(), uuidOf(id(first)), uuid.New()),
		"rejected": clearings.Rejected(context.Background(), uuidOf(id(first)), uuid.New(), "x"),
		"barred": func() error {
			_, err := clearings.Barred(context.Background(), uuidOf(id(first)), uuid.New())
			return err
		}(),
		"ledger": func() error {
			_, err := ledger.Postings{}.PostBank(context.Background(), ledger.BankPosting{})
			return err
		}(),
		"postable": ledger.Postings{}.Postable(context.Background(), e.DE, date("2026-04-17")),
	} {
		t.Run(name, func(t *testing.T) { pgtest.WantErr(t, err, "") })
	}
}

// TestAutomaticPostingRefusedRollsBack: a run whose postings the ledger
// refuses (April locked) leaves the lines matched, keeps nothing of the
// approval request it opened for one (no request, no event, no audit
// entry) and audits each refusal.
func TestAutomaticPostingRefusedRollsBack(t *testing.T) {
	e := matchSetupWith(t, "payments.statement.post")
	e.statementRules(t, postRules)
	sc := matchStatementOf(t, e)
	e.bankPosts(t)
	for _, status := range []string{"closed", "locked"} {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND period = 4`, e.Tenant.ID, e.DE, status)
	}
	h := endpoints(nil)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "a refused automatic posting", func(ctx context.Context) error {
		_, err := h.MatchStatement(ctx, uuidOf(sc.statement), anyVersion)
		return err
	})
	e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	if got := e.lineStatuses(t, sc.statement)[:3]; strings.Join(got, ",") != "matched,matched,matched" {
		t.Errorf("lines 1-3 in a locked April: got %v, want matched", got)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pgtest.Admin(t).QueryRow(context.Background(), q, append([]any{e.Tenant.ID}, args...)...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	first := id(sc.live(t, 1))
	if got := count(`SELECT count(*) FROM workflow.approval_request WHERE tenant_id = $1 AND object_id = $2`, first); got != 0 {
		t.Errorf("approval requests of line 1: got %d, want none", got)
	}
	if got := len(apitest.Published(t, e.Tenant, outbox.Event{Context: "workflow", Name: "approval_requested"})); got != 0 {
		t.Errorf("approval_requested events: got %d, want none", got)
	}
	if got := count(`SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'workflow.approval_request.open'`); got != 0 {
		t.Errorf("audited request openings: got %d, want none", got)
	}
	if got := count(`SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'payments.statement_match.post' AND outcome = 'refused'
		AND detail->>'code' = 'ledger.period_not_open'`); got != 3 {
		t.Errorf("audited refusals in the locked period: got %d, want 3", got)
	}
}

// TestAutomaticPostingNeedsThePermission: a person without
// payments.statement.post imports and runs the rules, and nothing posts.
func TestAutomaticPostingNeedsThePermission(t *testing.T) {
	e := matchSetup(t)
	e.statementRules(t, postRules)
	e.bankPosts(t)
	sc := matchStatementOf(t, e)
	if got := e.lineStatuses(t, sc.statement)[:3]; strings.Join(got, ",") != "matched,matched,matched" {
		t.Errorf("lines 1-3 imported by a person who may not post: got %v, want matched", got)
	}
}
