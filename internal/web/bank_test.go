package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// statementS1 is house bank MAIN's first statement in the bank tests,
// opening at 1,000.00 on 16 April and closing at 957.25 on 17 April:
// charges of 10.50 the rules post at once (line 1), an incoming payment
// of 40.00 (2), a debit of 5.00 nothing explains (3), charges of 60.00 the
// agent proposes to post by the charges rule (4) and a debit of 7.25 it
// proposes to exclude, its reason planted (5).
var statementS1 = fixture.MT940("S1", "1", "1000,00", "260416", "957,25", "260417",
	":61:2604170417D10,50NCHGNONREF//B1\n",
	":61:2604170417C40,00NTRFNONREF//B2\n:86:/NAME/CUSTOMER 4711/REMI/AR-2026-118\n",
	":61:2604170417D5,00NMSCNONREF//B3\n",
	":61:2604170417D60,00NMSCNONREF//B4\n",
	":61:2604170417D7,25NMSCNONREF//B5\n")

// bankData is what bankStack's tenant holds.
type bankData struct {
	statement uuid.UUID // S1
	bank      uuid.UUID // house bank MAIN of company 2000
	de, us    uuid.UUID // companies 2000, MAIN •••• 3000's, and 1000
	objects   blobstore.Dir
	viewer    pgtest.Actor // may read statements and companies alone
	outsider  pgtest.Actor // may read nothing
	approver3 pgtest.Actor // a third person holding every role
}

// bankStack is the workspace in front of a configured tenant whose
// approver reconciles bank statements: house bank MAIN posting its lines
// (fixture.Reconciles), its opening balance in the ledger, statement S1
// imported with the agent's two proposals, a person who may only read
// statements and one who may not.
func bankStack(t *testing.T) (*stack, *bankData) {
	t.Helper()
	d := &bankData{objects: blobstore.Dir{Root: t.TempDir()}}
	s := newStack(t, stackOptions{permissions: fixture.StatementPermissions, objects: d.objects, seed: func(f fixture.Env) {
		f.Reconciles(t)
		f.OpeningBalance(t, 100000)
		d.bank = f.HouseBank(t)
		d.de, d.us = f.DE, f.US
		d.statement = f.ImportStatement(t, d.objects, statementS1)[0]
		f.Propose(t, d.statement, 4, `"posting_rule": "NCHG", "reason": "The bank booked 60.00 of charges for the quarter."`)
		reason, _ := json.Marshal(planted)
		f.Propose(t, d.statement, 5, `"exclude": true, "reason": `+string(reason))
		d.viewer = roleHolder(t, f, "viewer", "payments.statement.read", "tenancy.org.read")
		d.outsider = roleHolder(t, f, "outsider")
		d.approver3 = f.Person(t, "approver3")
	}})
	return s, d
}

// linesOfStatement are a statement's lines' statuses, from the database.
func linesOfStatement(t *testing.T, statement uuid.UUID) string {
	t.Helper()
	var out []string
	pgtest.Scan(t, `SELECT array_agg(status ORDER BY line_no) FROM payments.bank_statement_line WHERE statement_id = $1`, []any{statement}, &out)
	return strings.Join(out, " ")
}

// lineCard is the text of a statement's page's line card.
func lineCard(p page, line int) string {
	for _, li := range p.all("li", "line") {
		if attrOf(li, "id") == fmt.Sprintf("line-%d", line) {
			return text(li)
		}
	}
	p.t.Fatalf("no line %d in %s", line, p.mainText())
	return ""
}

// lineForm is the decision verb's form of the line, nil for none.
func lineForm(p page, statement uuid.UUID, line int, verb string) *html.Node {
	for _, f := range p.all("form", "") {
		if attrOf(f, "action") == statementPath(statement)+"/"+verb && field(f, "line") == fmt.Sprint(line) {
			return f
		}
	}
	return nil
}

// decideOn posts the line's verb form as the statement's page offers it,
// with the fields given besides its own.
func decideOn(b *browser, statement uuid.UUID, line int, verb string, fields url.Values) response {
	b.s.t.Helper()
	f := lineForm(b.get(statementPath(statement)).page(b.s.t), statement, line, verb)
	if f == nil {
		b.s.t.Fatalf("line %d: no %s form", line, verb)
	}
	v := url.Values{}
	for _, name := range []string{"csrf_token", "if_match", "idempotency_key", "line"} {
		v.Set(name, field(f, name))
	}
	for k, vs := range fields {
		v[k] = vs
	}
	return b.do(http.MethodPost, statementPath(statement)+"/"+verb, v)
}

// upload posts the statement upload form as the list offers it: the
// fields given (a nil value: the list's own) and the file, when not nil.
func upload(b *browser, fields map[string]string, file []byte) response {
	b.s.t.Helper()
	list := b.get("/bank").page(b.s.t)
	form := list.form(uploadPathOf)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	values := map[string]string{"csrf_token": field(form, "csrf_token"), "idempotency_key": field(form, "idempotency_key")}
	for k, v := range fields {
		values[k] = v
	}
	for k, v := range values {
		_ = w.WriteField(k, v) // into memory
	}
	if file != nil {
		part, _ := w.CreateFormFile("file", "statement.sta") // into memory
		_, _ = part.Write(file)
	}
	_ = w.Close()
	return b.raw(http.MethodPost, uploadPathOf, &body, w.FormDataContentType())
}

const uploadPathOf = "/bank/statements"

// raw sends a body of its own type, as the browser sends a form; header
// holds extra headers, as name, value pairs.
func (b *browser) raw(method, path string, body io.Reader, ctype string, header ...string) response {
	b.s.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, b.s.public+path, body)
	if err != nil {
		b.s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Origin", b.s.public)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		b.s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		b.s.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(data), url: req.URL}
}

// TestStatementList: the list shows the house banks with their links and
// the statements with their bank, masked account, company, balances and
// status; the status tabs and the bank filter narrow it; a gap is
// flagged; the upload form is offered to a person who may import.
func TestStatementList(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	r := b.get("/bank")
	status(t, "the list", r, http.StatusOK)
	p := r.page(t)
	main := p.mainText()
	holds(t, "the list", main, "MAIN •••• 3000 EUR Acme GmbH Its statements Reconciliation with the ledger",
		"Status Imported", "House bank MAIN •••• 3000", "Company Acme GmbH", "Statement number 1", "Opening balance EUR 1,000.00 2026-04-16",
		"Closing balance EUR 957.25 2026-04-17", "Lines 5", "Import a statement file")
	if strings.Contains(r.body, "DE89370400440532013000") {
		t.Error("the list shows the whole account")
	}
	checkInert(t, p)
	cards := p.all("li", "card")
	if len(cards) != 1 || attrOf(p.cardLink(cards[0]), "href") != statementPath(d.statement) {
		t.Fatalf("cards: got %d, want S1's", len(cards))
	}
	current := ""
	for _, a := range p.all("a", "navigation__link") {
		if attrOf(a, "aria-current") == "page" {
			current = attrOf(a, "href")
		}
	}
	if current != "/bank" {
		t.Errorf("navigation: the current entry is %q, want /bank", current)
	}
	for name, tc := range map[string]struct {
		query string
		want  int
	}{
		"reconciled":    {"status=reconciled", 0},
		"imported":      {"status=imported", 1},
		"the bank":      {"house_bank_id=" + d.bank.String(), 1},
		"another bank":  {"house_bank_id=" + uuid.NewString(), 0},
		"both, the tab": {"house_bank_id=" + d.bank.String() + "&status=gap", 0},
		"the company":   {"legal_entity_id=" + d.de.String(), 1},
		"another":       {"legal_entity_id=" + d.us.String(), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := len(b.get("/bank?"+tc.query).page(t).all("li", "card")); got != tc.want {
				t.Errorf("got %d statements, want %d", got, tc.want)
			}
		})
	}
	status(t, "a bank that is no id", b.get("/bank?house_bank_id=x"), http.StatusBadRequest)
	status(t, "a company that is no id", b.get("/bank?legal_entity_id=x"), http.StatusBadRequest)
	// A company chosen (T73i2): its house banks alone, chosen in the
	// filter, and kept by the status tabs.
	us := b.get("/bank?legal_entity_id=" + d.us.String()).page(t)
	if strings.Contains(us.mainText(), "MAIN •••• 3000") {
		t.Errorf("company 1000's statements: show company 2000's house bank: %s", us.mainText())
	}
	de := b.get("/bank?legal_entity_id=" + d.de.String()).page(t)
	selected := ""
	walk(de.doc, func(n *html.Node) {
		if n.Data == "option" && hasAttr(n, "selected") {
			selected += attrOf(n, "value")
		}
	})
	if selected != d.de.String() {
		t.Errorf("the company filter: chosen %q, want %s", selected, d.de)
	}
	tabs := map[string]bool{}
	for _, a := range de.all("a", "") {
		tabs[attrOf(a, "href")] = true
	}
	gapTab := "/bank?" + (url.Values{"legal_entity_id": {d.de.String()}, "status": {"gap"}}).Encode()
	if !tabs[gapTab] {
		t.Errorf("the gap tab: not %s in %v", gapTab, tabs)
	}
	fr := b.get("/bank", "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "1 000,00 EUR", "Relevés bancaires", "Importé")
	t.Run("a gap", func(t *testing.T) {
		r := upload(b, map[string]string{"house_bank_id": d.bank.String(), "format": "mt940"},
			[]byte(fixture.MT940("S3", "3", "900,00", "260419", "890,00", "260420", ":61:2604200420D10,00NCHGNONREF//G1\n")))
		status(t, "the gap's import", r, http.StatusSeeOther)
		gap := uuid.MustParse(strings.TrimPrefix(strings.TrimSuffix(r.location(), "?done=imported"), "/bank/statements/"))
		g := b.get("/bank?status=gap").page(t)
		cards := g.all("li", "card")
		if len(cards) != 1 || attrOf(g.cardLink(cards[0]), "href") != statementPath(gap) {
			t.Fatalf("the gap tab: got %d statements, want S3", len(cards))
		}
		holds(t, "the gap", text(cards[0]), "Gap before it", "Its opening balance is not the closing balance of the statement before it.")
		gp := b.get(statementPath(gap)).page(t)
		holds(t, "the gap's page", gp.mainText(), "A statement is missing before this one", "Previous statement Open the previous statement")
		holds(t, "the gap's line", lineCard(gp, 1), "Status Matched")
		if lineForm(gp, gap, 1, "post") != nil {
			t.Error("a statement in gap offers its matched line's posting")
		}
	})
}

// TestStatementPage: a statement's page shows its summary with the
// account masked, and each line with its status, its live match (the
// rule's posting, the agent's proposals badged with their reasons inert)
// and the decisions its status offers, each form with the statement's
// ETag, the line and a fresh idempotency key.
func TestStatementPage(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	r := b.get(statementPath(d.statement))
	status(t, "the statement", r, http.StatusOK)
	p := r.page(t)
	if strings.Contains(r.body, "DE89370400440532013000") {
		t.Error("the page shows the whole account")
	}
	checkInert(t, p)
	holds(t, "the summary", p.mainText(), "Bank statement S1", "Status Imported", "House bank MAIN •••• 3000", "File format mt940",
		"Opening balance EUR 1,000.00 2026-04-16", "Closing balance EUR 957.25 2026-04-17", "Lines 5", "The bank's reconciliation with the ledger")
	for line, want := range map[int][]string{
		1: {"Line 1", "Status Posted", "EUR -10.50", "Transaction code NCHG", "Matched by a rule", "Matching rule Transaction code",
			"Posting rule NCHG Bank charges", "Posting date 2026-04-17", "Journal entry"},
		2: {"Status Not matched", "EUR 40.00", "Bank's text", "CUSTOMER 4711", "Match by hand", "Exclude from reconciliation"},
		4: {"Status Proposed", "The reconciliation agent's proposal Agent", "An agent proposed this.", "It proposes to Match the line",
			"The bank booked 60.00 of charges for the quarter.", "Posting rule NCHG Bank charges", "Accept the agent's proposal", "Reject the agent's proposal"},
		5: {"It proposes to Exclude the line", "Pay now <script>"},
	} {
		holds(t, fmt.Sprintf("line %d", line), lineCard(p, line), want...)
	}
	if got := lineCard(p, 1); strings.Contains(got, "Match by hand") || strings.Contains(got, "Post the line") {
		t.Errorf("line 1, posted: offers a decision: %q", got)
	}
	keys := map[string]bool{}
	for _, f := range p.all("form", "decide") {
		if got := field(f, "if_match"); got == "" {
			t.Errorf("form %s: no If-Match", attrOf(f, "action"))
		}
		k := field(f, "idempotency_key")
		if _, err := uuid.Parse(k); err != nil || keys[k] {
			t.Errorf("form %s: key %q, want a fresh one", attrOf(f, "action"), k)
		}
		keys[k] = true
	}
	for name, tc := range map[string]struct {
		line  int
		verb  string
		offer bool
	}{
		"line 2 matched by hand":      {2, "match", true},
		"line 2 posted":               {2, "post", false},
		"line 3 excluded":             {3, "exclude", true},
		"line 4 accepted":             {4, "accept", true},
		"line 4 rejected":             {4, "reject", true},
		"line 1 matched again":        {1, "match", false},
		"line 3 accepted, no agent's": {3, "accept", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lineForm(p, d.statement, tc.line, tc.verb) != nil; got != tc.offer {
				t.Errorf("offered: got %v, want %v", got, tc.offer)
			}
		})
	}
	choices := func(line int) string {
		var out []string
		for _, in := range p.all("input", "radio__input") {
			if strings.HasPrefix(attrOf(in, "id"), fmt.Sprintf("line-%d-", line)) {
				out = append(out, attrOf(in, "value"))
			}
		}
		return strings.Join(out, " ")
	}
	if got := choices(2); got != "rule:NCHG incoming" {
		t.Errorf("line 2's choices, a credit: got %q", got)
	}
	if got := choices(3); got != "rule:NCHG" {
		t.Errorf("line 3's choices, a debit: got %q", got)
	}
	status(t, "no statement", b.get(statementPath(uuid.New())), http.StatusNotFound)
	status(t, "no id", b.get("/bank/statements/x"), http.StatusNotFound)
	fr := b.get(statementPath(d.statement), "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "Relevé bancaire", "-10,50 EUR", "Non rapprochée", "Proposition de l'agent de rapprochement")
}

// TestLineDecisions: the reconciler's job on S1. An incoming payment is
// matched by hand and posted, a debit excluded with its reason, the
// agent's exclusion rejected and its line posted by the charges rule, and
// its proposal for line 4 accepted; a second person posts line 4, which
// waits for its approval in the inbox, where the person who accepted it
// is refused as approver; once approved by another, the statement is
// reconciled and decided for good.
func TestLineDecisions(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	done := func(what string, r response, notice string, line int) {
		t.Helper()
		want := statementPath(d.statement) + "?done=" + notice + fmt.Sprintf("&line=%d", line)
		if r.status != http.StatusSeeOther || r.location() != want {
			t.Fatalf("%s: got %d to %q, want 303 to %q: %s", what, r.status, r.location(), want, r.body)
		}
	}
	done("line 2 matched", decideOn(b, d.statement, 2, "match", url.Values{"target": {"incoming"}, "comment": {"Customer 4711 paid AR-2026-118."}}), "matched", 2)
	holds(t, "line 2 matched", b.get(statementPath(d.statement)+"?done=matched&line=2").page(t).mainText(), "The line is matched. Go to line 2",
		"Matched by hand", "Customer 4711 paid AR-2026-118.", "An incoming payment, to the bank's incoming payments clearing account")
	done("line 2 posted", decideOn(b, d.statement, 2, "post", nil), "posted", 2)
	done("line 3 excluded", decideOn(b, d.statement, 3, "exclude", url.Values{"comment": {"A correction the bank reverses tomorrow."}}), "excluded", 3)
	done("line 5 rejected", decideOn(b, d.statement, 5, "reject", url.Values{"comment": {"Not ours to exclude: the bank's charge."}}), "rejected", 5)
	done("line 5 matched", decideOn(b, d.statement, 5, "match", url.Values{"target": {"rule:NCHG"}, "comment": {"Charges."}}), "matched", 5)
	done("line 5 posted", decideOn(b, d.statement, 5, "post", url.Values{"date": {"2026-04-18"}, "comment": {"Booked a day late."}}), "posted", 5)
	done("line 4 accepted", decideOn(b, d.statement, 4, "accept", url.Values{"comment": {"Checked against the charges schedule."}}), "accepted", 4)
	if got := linesOfStatement(t, d.statement); got != "posted posted excluded matched posted" {
		t.Fatalf("lines: got %q", got)
	}
	p := b.get(statementPath(d.statement)).page(t)
	if lineForm(p, d.statement, 3, "match") == nil || lineForm(p, d.statement, 3, "exclude") != nil {
		t.Error("line 3, excluded: want a match by hand offered, and no exclusion again")
	}
	holds(t, "line 5", lineCard(p, 5), "Agent's last proposal: Rejected", "Posting date 2026-04-18", "Why on that date Booked a day late.")
	holds(t, "line 4", lineCard(p, 4), "Agent's last proposal: Accepted", "Matched by hand", "Post the line")
	other := s.as(s.f.Approver2)
	done("line 4 posted by another", decideOn(other, d.statement, 4, "post", nil), "pending", 4)
	waiting := other.get(statementPath(d.statement)).page(t)
	holds(t, "line 4 waiting", lineCard(waiting, 4), "Status Waiting for approval", "whoever matched the line by hand may not approve it",
		"Open the approval request")
	var request uuid.UUID
	for _, a := range waiting.all("a", "button") {
		if id, ok := strings.CutPrefix(attrOf(a, "href"), "/approvals/"); ok {
			request = uuid.MustParse(id)
		}
	}
	refused := b.do(http.MethodPost, requestPath(request)+"/approve", values(b.get(requestPath(request)).page(t).form(requestPath(request)+"/approve"), "ok"))
	status(t, "the matcher approves", refused, http.StatusForbidden)
	holds(t, "the matcher approves", refused.page(t).mainText(), "Bank statement clearing",
		"You took part in this work, so segregation of duties keeps you from approving it.")
	third := s.as(d.approver3)
	form := third.get(requestPath(request)).page(t).form(requestPath(request) + "/approve")
	if r := third.do(http.MethodPost, requestPath(request)+"/approve", values(form, "Charges schedule checked")); r.status != http.StatusSeeOther {
		t.Fatalf("the posting's approval: got %d %s", r.status, r.page(t).mainText())
	}
	if got := linesOfStatement(t, d.statement); got != "posted posted excluded posted posted" {
		t.Fatalf("lines after the approval: got %q", got)
	}
	final := b.get(statementPath(d.statement)).page(t).mainText()
	holds(t, "reconciled", final, "Status Reconciled", "Reconciled on", "Reconciled by", "The statement is reconciled: its lines are decided for good.")
	if strings.Contains(final, "Match by hand") || strings.Contains(final, "Exclude from reconciliation") {
		t.Error("a reconciled statement offers decisions")
	}
}

// TestLineRefusals: what is wrong with a decision is said before the API
// is called, with what was typed kept and the form unfolded; the API's
// refusals are explained by their code; a stale form is refused with its
// text kept; a resubmitted form replays the first answer.
func TestLineRefusals(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	decideOn(b, d.statement, 2, "match", url.Values{"target": {"incoming"}, "comment": {"Customer 4711"}})
	for name, tc := range map[string]struct {
		line   int
		verb   string
		fields url.Values
		want   string
	}{
		"a match without a choice":      {2, "match", url.Values{"comment": {"Paid"}}, "Choose what the line clears."},
		"a match to no such choice":     {2, "match", url.Values{"target": {"payments:x"}, "comment": {"Paid"}}, "Choose what the line clears."},
		"a match without a reason":      {2, "match", url.Values{"target": {"incoming"}}, "Enter the reason."},
		"an exclusion without reason":   {3, "exclude", url.Values{"comment": {" "}}, "Enter the reason."},
		"a rejection without reason":    {5, "reject", nil, "Enter the reason."},
		"a reason 2,001 long":           {4, "accept", url.Values{"comment": {strings.Repeat("x", 2001)}}, "A comment has at most 2,000 characters."},
		"a posting date that is none":   {2, "post", url.Values{"date": {"18.04.2026"}, "comment": {"Late"}}, "Enter a date as year-month-day"},
		"a posting date, no reason":     {2, "post", url.Values{"date": {"2026-04-18"}}, "Enter why the line posts on another date."},
		"a reason without a date":       {2, "post", url.Values{"comment": {"Late"}}, "A reason goes with another posting date"},
		"a posting of a line unmatched": {3, "post", nil, "Only a matched line is posted. Match it first."},
		"a match to another's rule":     {3, "match", url.Values{"target": {"rule:NONE"}, "comment": {"x"}}, "The line may not clear what you chose"},
	} {
		t.Run(name, func(t *testing.T) {
			// The forms are those of a line that takes them; a refused
			// one is sent as the page's form of another verb would be.
			f := lineForm(b.get(statementPath(d.statement)).page(t), d.statement, tc.line, tc.verb)
			if f == nil {
				f = lineForm(b.get(statementPath(d.statement)).page(t), d.statement, 3, "exclude")
			}
			v := url.Values{"line": {fmt.Sprint(tc.line)}}
			for _, n := range []string{"csrf_token", "if_match", "idempotency_key"} {
				v.Set(n, field(f, n))
			}
			for k, vs := range tc.fields {
				v[k] = vs
			}
			r := b.do(http.MethodPost, statementPath(d.statement)+"/"+tc.verb, v)
			if r.status < 400 {
				t.Fatalf("got %d, want a refusal", r.status)
			}
			holds(t, "the page", r.page(t).mainText(), tc.want)
		})
	}
	if got := linesOfStatement(t, d.statement); got != "posted matched unmatched proposed proposed" {
		t.Errorf("lines after the refusals: got %q", got)
	}
	t.Run("what was typed is kept, its form open", func(t *testing.T) {
		r := decideOn(b, d.statement, 3, "match", url.Values{"comment": {"Customer 4711"}})
		p := r.page(t)
		f := lineForm(p, d.statement, 3, "match")
		if f == nil || !hasAttr(f.Parent, "open") || !strings.Contains(text(f), "Customer 4711") {
			t.Errorf("the match form: not open with the reason typed")
		}
	})
	t.Run("a stale form", func(t *testing.T) {
		stale := lineForm(b.get(statementPath(d.statement)).page(t), d.statement, 3, "exclude")
		decideOn(b, d.statement, 5, "reject", url.Values{"comment": {"Moves the statement on."}})
		v := url.Values{"line": {"3"}, "comment": {"Kept, though refused"}}
		for _, n := range []string{"csrf_token", "if_match", "idempotency_key"} {
			v.Set(n, field(stale, n))
		}
		r := b.do(http.MethodPost, statementPath(d.statement)+"/exclude", v)
		status(t, "stale", r, http.StatusPreconditionFailed)
		holds(t, "stale", r.page(t).mainText(), "The statement changed since you opened it", "Kept, though refused", "request.version_mismatch")
	})
	t.Run("a resubmitted form", func(t *testing.T) {
		f := lineForm(b.get(statementPath(d.statement)).page(t), d.statement, 3, "exclude")
		v := url.Values{"line": {"3"}, "comment": {"Bank correction."}}
		for _, n := range []string{"csrf_token", "if_match", "idempotency_key"} {
			v.Set(n, field(f, n))
		}
		first, again := b.do(http.MethodPost, statementPath(d.statement)+"/exclude", v), b.do(http.MethodPost, statementPath(d.statement)+"/exclude", v)
		if first.status != http.StatusSeeOther || again.location() != first.location() {
			t.Errorf("got %d to %q, then %d to %q, want the first answer twice", first.status, first.location(), again.status, again.location())
		}
	})
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		for name, v := range map[string]url.Values{
			"no key":  {"csrf_token": {token}, "if_match": {`"1"`}, "line": {"2"}},
			"no line": {"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}},
			"line 0":  {"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}, "line": {"0"}},
		} {
			status(t, name, b.do(http.MethodPost, statementPath(d.statement)+"/exclude", v), http.StatusBadRequest)
		}
		status(t, "no CSRF token", b.do(http.MethodPost, statementPath(d.statement)+"/exclude", url.Values{"line": {"2"}}), http.StatusForbidden)
		status(t, "no statement id", b.do(http.MethodPost, "/bank/statements/x/exclude", url.Values{"csrf_token": {token}}), http.StatusNotFound)
	})
}

// TestStatementUpload: a bank's file is imported for a house bank and
// the person goes to its statement; the same file again shows what it
// imported; what is wrong with the form is said with the bank and format
// kept, and the API's refusals explained by their code.
func TestStatementUpload(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	s2 := []byte(fixture.MT940("S2", "2", "957,25", "260417", "947,25", "260420", ":61:2604200420D10,00NCHGNONREF//C1\n"))
	fields := map[string]string{"house_bank_id": d.bank.String(), "format": "mt940"}
	r := upload(b, fields, s2)
	if r.status != http.StatusSeeOther || !strings.HasSuffix(r.location(), "?done=imported") {
		t.Fatalf("import: got %d to %q", r.status, r.location())
	}
	holds(t, "the new statement", b.get(r.location()).page(t).mainText(), "The file is imported.", "Bank statement S2", "Status Imported")
	again := upload(b, fields, s2)
	if got, want := again.location(), strings.TrimSuffix(r.location(), "?done=imported")+"?done=reimported"; got != want {
		t.Errorf("the same file again: got %d to %q, want %q", again.status, got, want)
	}
	holds(t, "the same file again", b.get(again.location()).page(t).mainText(), "This file was imported before.")
	for name, tc := range map[string]struct {
		fields map[string]string
		file   []byte
		status int
		want   []string
	}{
		"no bank":   {map[string]string{"format": "mt940"}, s2, http.StatusUnprocessableEntity, []string{"Choose the house bank the statements are of."}},
		"no format": {map[string]string{"house_bank_id": d.bank.String(), "format": "csv"}, s2, http.StatusUnprocessableEntity, []string{"Choose the file's format."}},
		"no file":   {fields, nil, http.StatusUnprocessableEntity, []string{"Choose the bank's file."}},
		"empty":     {fields, []byte{}, http.StatusUnprocessableEntity, []string{"Choose the bank's file."}},
		"unreadable": {fields, []byte("not a statement"), http.StatusUnprocessableEntity,
			[]string{"The file was not imported", "The file could not be read in the format you chose.", "payments.statement_unreadable",
				"Choose the file again before you send the form."}},
		"another file of S2": {fields, []byte(fixture.MT940("S2", "2", "957,25", "260417", "957,25", "260420")), http.StatusConflict,
			[]string{"A statement in this file was imported before from another file"}},
		"too large": {fields, bytes.Repeat([]byte("x"), maxUploadBody), http.StatusRequestEntityTooLarge, []string{"The file is larger than 4 MB."}},
		"a little too large": {fields, bytes.Repeat([]byte("x"), maxStatementFile+1), http.StatusUnprocessableEntity,
			[]string{"The file is larger than 4 MB."}},
	} {
		t.Run(name, func(t *testing.T) {
			r := upload(b, tc.fields, tc.file)
			status(t, "the upload", r, tc.status)
			holds(t, "the upload", r.page(t).mainText(), tc.want...)
		})
	}
	t.Run("the bank and format kept", func(t *testing.T) {
		p := upload(b, fields, []byte("not a statement")).page(t)
		form := p.form(uploadPathOf)
		kept := map[string]string{}
		walk(form, func(n *html.Node) {
			if n.Data == "option" && hasAttr(n, "selected") {
				kept[attrOf(n, "value")] = "selected"
			}
		})
		if kept[d.bank.String()] == "" || kept["mt940"] == "" {
			t.Errorf("selected: got %v, want the bank and mt940", kept)
		}
	})
	t.Run("a form that is not the page's", func(t *testing.T) {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		_ = w.WriteField("csrf_token", b.csrf()) // into memory
		_ = w.Close()
		status(t, "no key", b.raw(http.MethodPost, uploadPathOf, &body, w.FormDataContentType()), http.StatusBadRequest)
		status(t, "no token", b.raw(http.MethodPost, uploadPathOf, strings.NewReader("x"), "multipart/form-data; boundary=b"), http.StatusForbidden)
	})
}

// TestReconciliationReport: a house bank's report shows the period's and
// each statement's closing balance against the ledger's, what explains
// the difference and that nothing is left unexplained, the lines' counts
// and the lines with their matches; the period and a line status narrow
// it; a date that is no date is said so.
func TestReconciliationReport(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	path := reportPath(d.bank)
	r := b.get(path)
	status(t, "the report", r, http.StatusOK)
	p := r.page(t)
	checkInert(t, p)
	holds(t, "the report", p.mainText(), "Reconciliation with the ledger MAIN •••• 3000", "1 statement closes in the period.",
		"Opening balance of the period EUR 1,000.00 2026-04-16", "On 2026-04-17", "Bank: closing balance EUR 957.25",
		"Ledger: balance of the bank's account EUR 989.50", "Explained by lines excluded EUR 0.00", "Explained by lines not posted yet EUR -32.25",
		"Explained by lines posted after the date EUR 0.00", "Unexplained Nothing unexplained", "Not matched 2 Proposed 2 Matched 0")
	rows := p.all("tr", "")
	if len(rows) != 6 { // the heading and five lines
		t.Errorf("rows: got %d, want the heading and five lines", len(rows))
	}
	holds(t, "line 1", text(rows[1]), "1 2026-04-17 EUR -10.50 NCHG Posted Matched by a rule Transaction code 2026-04-17")
	cards := p.all("li", "card")
	if len(cards) != 1 || attrOf(p.cardLink(cards[0]), "href") != statementPath(d.statement) {
		t.Fatalf("statements: got %d, want S1 linked", len(cards))
	}
	if got := len(b.get(path+"?line_status=unmatched").page(t).all("tr", "")); got != 3 {
		t.Errorf("the unmatched lines: got %d rows, want the heading and two", got)
	}
	holds(t, "after the period", b.get(path+"?closing_date_from=2026-04-18").page(t).mainText(), "0 statements close in the period.",
		"No statement closes in the period.")
	bad := b.get(path + "?closing_date_to=17.04.2026")
	status(t, "a date that is no date", bad, http.StatusUnprocessableEntity)
	holds(t, "a date that is no date", bad.page(t).mainText(), "Enter a date as year-month-day")
	refused := b.get(path + "?line_status=lost")
	status(t, "a status there is not", refused, http.StatusUnprocessableEntity)
	holds(t, "a status there is not", refused.page(t).mainText(), "The reconciliation could not be shown", "Something in the form is not valid.")
	status(t, "no such bank", b.get(reportPath(uuid.New())), http.StatusNotFound)
	status(t, "no id", b.get("/bank/house-banks/x/reconciliation"), http.StatusNotFound)
	fr := b.get(path, "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "Rapprochement avec le grand livre", "-32,25 EUR", "Rien d'inexpliqué")
}

// TestBankAsOthers: a person who may only read statements sees the list,
// a statement and its report with nothing to decide or import; a person
// who may read none is told why, and the bank is not in their
// navigation.
func TestBankAsOthers(t *testing.T) {
	s, d := bankStack(t)
	viewer := s.as(d.viewer)
	list := viewer.get("/bank")
	status(t, "the viewer's list", list, http.StatusOK)
	if strings.Contains(list.body, "Import a statement file") {
		t.Error("the viewer is offered the upload")
	}
	st := viewer.get(statementPath(d.statement)).page(t)
	holds(t, "the viewer's statement", st.mainText(), "You may read this statement, but not decide its lines.")
	if forms := st.all("form", "decide"); len(forms) != 0 {
		t.Errorf("the viewer is offered %d decisions", len(forms))
	}
	status(t, "the viewer's report", viewer.get(reportPath(d.bank)), http.StatusOK)
	outsider := s.as(d.outsider)
	r := outsider.get("/bank")
	status(t, "the outsider's list", r, http.StatusForbidden)
	holds(t, "the outsider's list", r.page(t).mainText(), "The statements could not be shown", "You do not have the permission this needs.")
	for _, a := range r.page(t).all("a", "navigation__link") {
		if attrOf(a, "href") == "/bank" {
			t.Error("the outsider's navigation offers the bank")
		}
	}
	page := outsider.get(statementPath(d.statement))
	status(t, "the outsider's statement", page, http.StatusForbidden)
	holds(t, "the outsider's statement", page.page(t).mainText(), "You do not have the permission this needs.")
	report := outsider.get(reportPath(d.bank))
	status(t, "the outsider's report", report, http.StatusForbidden)
	holds(t, "the outsider's report", report.page(t).mainText(), "The reconciliation could not be shown")
}
