package payments_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// matchRules are company 2000's bank statement rules in the tests: charges
// post by NCHG, and the four matching rules in their usual order, the
// amount rule within 0.50, 3 days and to the counterparty.
const matchRules = `{"posting_rules": [{"transaction_code": "NCHG", "description": "Bank charges", "account": "520000"},
	{"transaction_code": "PMNT-CCRD-CHRG", "description": "Card charges", "account": "520000"}],
	"matching_rules": [{"kind": "end_to_end_id"}, {"kind": "batch_id"}, {"kind": "transaction_code"},
	{"kind": "amount", "amount_tolerance": "0.50", "date_window_days": 3, "counterparty": true}]}`

// matchSetup is statementSetup with the permission to match and company
// 2000's rules active, and the supplier's bank account.
func matchSetup(t *testing.T) env {
	t.Helper()
	return matchSetupWith(t)
}

// matchSetupWith is matchSetup with more permissions.
func matchSetupWith(t *testing.T, more ...string) env {
	t.Helper()
	statementPerms := append(append([]string{"payments.statement.match"}, more...), statementPerms...)
	e, _ := statementSetupWith(t, statementPerms)
	e.statementRules(t, matchRules)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	return e
}

// statementRules activates the configuration with company 2000's bank
// statement settings replaced by rules.
func (e env) statementRules(t *testing.T, rules string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, jsonb_set(v.document, '{companies,1,bank_statements}', $2::jsonb), v.hash, 'rollback',
			v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		e.Tenant.ID, rules)
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
}

// paidRun is a released run's one payment and its bank file.
type paidRun struct{ payment, e2e, file, msg string }

// paidOne releases a run on the date paying one invoice of 100.00.
func (e env) paidOne(t *testing.T, invoice, runDate string) paidRun {
	t.Helper()
	e.invoice(t, e.Supplier, invoice, "100.00", "2026-03-01")
	run := e.run(t, runDate, runDate)
	e.release(t, run, "")
	var r paidRun
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT p.id::text, p.end_to_end_id, f.id::text, f.message_id
		FROM payments.payment p JOIN payments.bank_file f ON f.tenant_id = p.tenant_id AND f.payment_run_id = p.payment_run_id
		WHERE p.payment_run_id = $1`, id(run)).Scan(&r.payment, &r.e2e, &r.file, &r.msg); err != nil {
		t.Fatal(err)
	}
	return r
}

// lineStatuses are the statement's lines' statuses by line number.
func (e env) lineStatuses(t *testing.T, statement string) []string {
	t.Helper()
	s, _ := e.Must(200, "GET", "/v1/bank-statements/"+statement, "", "")
	var out []string
	for _, l := range s["lines"].([]any) {
		out = append(out, l.(map[string]any)["status"].(string))
	}
	return out
}

// matchesOf are the statement's matches, of one line when lineNo is not 0.
func (e env) matchesOf(t *testing.T, statement string, lineNo int) []any {
	t.Helper()
	q := ""
	if lineNo > 0 {
		q = fmt.Sprintf("?line_no=%d", lineNo)
	}
	list, _ := e.Must(200, "GET", "/v1/bank-statements/"+statement+"/matches"+q, "", "")
	return list["items"].([]any)
}

// matchScenario is four payments of 100.00 released on 15-18 April and a
// statement of their bank whose lines each rule takes up.
type matchScenario struct {
	e         env
	pays      []paidRun
	statement string
}

func matchStatementSetup(t *testing.T) matchScenario {
	t.Helper()
	return matchStatementOf(t, matchSetup(t))
}

// matchStatementOf is the scenario's payments and statement in e.
func matchStatementOf(t *testing.T, e env) matchScenario {
	t.Helper()
	var pays []paidRun
	for i := range 4 {
		pays = append(pays, e.paidOne(t, fmt.Sprintf("INV-%d", i+1), fmt.Sprintf("2026-04-%d", 15+i)))
	}
	file := mtStatement("M1", "1", "1000,00", "260416", "429,70", "260417",
		":61:2604170417D100,00NTRFNONREF//B1\n:86:/EREF/"+pays[0].e2e+"\n",                              // 1: end-to-end id
		":61:2604170417D100,00NTRFNONREF//B2\n:86:/PREF/"+pays[1].msg+"-1\n",                            // 2: the batch
		":61:2604170417D10,50NCHGNONREF//B3\n",                                                          // 3: charges
		":61:2604170417D100,00NTRFNONREF//B4\n:86:/NAME/supplier\n",                                     // 4: two payments of the amount, to the supplier
		":61:2604170417D100,30NTRFNONREF//B5\n:86:/EREF/NONE/PREF/NONE-1/IBAN/GB33BUKB20201555555555\n", // 5: no such payment or file; of the amount, to someone else
		":61:2604170417C50,00NTRFNONREF//B6\n",                                                          // 6: incoming
		":61:2604170417RC10,50NCHGNONREF//B7\n",                                                         // 7: a reversal
		":61:2604170417D99,00NTRFNONREF//B8\n:86:/EREF/"+pays[3].e2e+"\n",                               // 8: a payment that does not pay the line
		":61:2604170417D100,00NTRFNONREF//B9\n:86:/EREF/"+pays[0].e2e+"\n",                              // 9: a payment line 1 holds
	)
	s := e.imported(t, "mt940", file)[0].(map[string]any)
	return matchScenario{e: e, pays: pays, statement: id(s)}
}

// TestMatchStatementLines: on import each line is matched by the first rule
// that finds candidates, or left open for a person, or unmatched; matching
// again changes nothing (ADR-0047, T49e).
func TestMatchStatementLines(t *testing.T) {
	sc := matchStatementSetup(t)
	e, pays := sc.e, sc.pays
	want := []string{"matched", "matched", "matched", "proposed", "unmatched", "unmatched", "unmatched", "proposed", "proposed"}
	check := func(t *testing.T) {
		t.Helper()
		if got := e.in(t).lineStatuses(t, sc.statement); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("line statuses: got %v, want %v", got, want)
		}
		if got := len(e.in(t).matchesOf(t, sc.statement, 0)); got != 6 {
			t.Errorf("matches: got %d, want 6", got)
		}
		if got := len(events(t, e.Tenant, "bank_statement_line_matched")); got != 6 {
			t.Errorf("matched events: got %d, want 6", got)
		}
	}
	check(t)
	for line, c := range map[int]struct {
		rule, status string
		targets      []string
	}{
		1: {"end_to_end_id", "matched", []string{"1:" + pays[0].payment}},
		2: {"batch_id", "matched", []string{"1:" + pays[1].payment + ":" + pays[1].file}},
		3: {"transaction_code", "matched", []string{"1:NCHG"}},
		4: {"amount", "open", []string{"1:" + pays[2].payment, "2:" + pays[3].payment}},
		8: {"end_to_end_id", "open", []string{"1:" + pays[3].payment}},
		9: {"end_to_end_id", "open", []string{"1:" + pays[0].payment}},
	} {
		t.Run(fmt.Sprintf("line %d", line), func(t *testing.T) {
			m := e.in(t).matchesOf(t, sc.statement, line)[0].(map[string]any)
			if m["kind"] != "rule" || m["rule"] != c.rule || m["status"] != c.status {
				t.Errorf("match: got %v %v %v, want rule %s %s", m["kind"], m["rule"], m["status"], c.rule, c.status)
			}
			if got := targetsOf(m); strings.Join(got, " ") != strings.Join(c.targets, " ") {
				t.Errorf("targets: got %v, want %v", got, c.targets)
			}
		})
	}
	t.Run("again", func(t *testing.T) {
		_, tag := e.in(t).Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
		s, after := e.in(t).Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", tag)
		wantBankCodes(t, "bank statement", s)
		if len(s["lines"].([]any)) != 9 || after != tag {
			t.Errorf("statement: got %v %s, want its 9 lines and still %s", s, after, tag)
		}
		check(t)
	})
}

// targetsOf are a match's targets as candidate:payment[:file] or
// candidate:posting rule.
func targetsOf(m map[string]any) []string {
	var out []string
	for _, x := range m["targets"].([]any) {
		t := x.(map[string]any)
		s := fmt.Sprint(t["candidate"])
		for _, k := range []string{"payment_id", "bank_file_id", "posting_rule"} {
			if v, ok := t[k]; ok {
				s += ":" + v.(string)
			}
		}
		out = append(out, s)
	}
	return out
}

// TestMatchBatchOfTwoFiles: a booking naming two bank files leaves both as
// candidates, each with its payments.
func TestMatchBatchOfTwoFiles(t *testing.T) {
	e := matchSetup(t)
	a, b := e.paidOne(t, "INV-A", "2026-04-15"), e.paidOne(t, "INV-B", "2026-04-16")
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt><GrpHdr><MsgId>MSG-B</MsgId></GrpHdr>
<Stmt><Id>B1</Id><ElctrncSeqNb>1</ElctrncSeqNb><Acct><Id><IBAN>DE89370400440532013000</IBAN></Id><Ccy>EUR</Ccy></Acct>
<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="EUR">500.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-04-17</Dt></Dt></Bal>
<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="EUR">300.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-04-17</Dt></Dt></Bal>
<Ntry><Amt Ccy="EUR">200.00</Amt><CdtDbtInd>DBIT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>2026-04-17</Dt></BookgDt><ValDt><Dt>2026-04-17</Dt></ValDt>
<BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>ICDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd><NtryDtls>
<TxDtls><Refs><PmtInfId>` + a.msg + `-1</PmtInfId></Refs><Amt Ccy="EUR">100.00</Amt><CdtDbtInd>DBIT</CdtDbtInd></TxDtls>
<TxDtls><Refs><MsgId>` + b.msg + `</MsgId></Refs><Amt Ccy="EUR">100.00</Amt><CdtDbtInd>DBIT</CdtDbtInd></TxDtls>
</NtryDtls></Ntry></Stmt></BkToCstmrStmt></Document>`
	s := e.imported(t, "camt.053", doc)[0].(map[string]any)
	m := e.matchesOf(t, id(s), 1)[0].(map[string]any)
	first, second := "1:"+a.payment+":"+a.file, "2:"+b.payment+":"+b.file
	if a.file > b.file {
		first, second = "1:"+b.payment+":"+b.file, "2:"+a.payment+":"+a.file
	}
	if got := strings.Join(targetsOf(m), " "); m["rule"] != "batch_id" || m["status"] != "open" || got != first+" "+second {
		t.Errorf("match: got %v %v %s, want batch_id open %s %s", m["rule"], m["status"], got, first, second)
	}
}

// TestMatchLinesByHand: a person matches a line to payments or a posting
// rule, or excludes it, replacing its match; what may not be matched is
// refused (ADR-0047, T49e).
func TestMatchLinesByHand(t *testing.T) {
	sc := matchStatementSetup(t)
	e, pays, line := sc.e, sc.pays, func(n int, action string) string {
		return fmt.Sprintf("/v1/bank-statements/%s/lines/%d:%s", sc.statement, n, action)
	}
	tag := func() string { _, tag := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", ""); return tag }
	stale := tag()
	m, next := e.Must(201, "POST", line(4, "match"), `{"payment_ids": ["`+pays[2].payment+`"], "reason": "Remittance names INV-3."}`, stale)
	if got := tag(); next != got || next == stale {
		t.Errorf("ETag after a decision: got %s, want the statement's new one %s", next, got)
	}
	if m["kind"] != "manual" || m["status"] != "matched" || strings.Join(targetsOf(m), "") != "1:"+pays[2].payment {
		t.Errorf("match by hand: got %v", m)
	}
	if got := e.matchesOf(t, sc.statement, 4); len(got) != 2 || got[0].(map[string]any)["status"] != "rejected" {
		t.Errorf("line 4's matches: got %v, want the rule's rejected and the person's", got)
	}
	x, next := e.Must(201, "POST", line(6, "exclude"), `{"reason": "Interest from another bank's correction."}`, next) // the ETag returned is the next If-Match
	if x["status"] != "excluded" || len(x["targets"].([]any)) != 0 {
		t.Errorf("exclude: got %v", x)
	}
	e.Must(201, "POST", line(7, "match"), `{"posting_rule": "NCHG", "reason": "Charges reversed."}`, next)
	want := []string{"matched", "matched", "matched", "matched", "unmatched", "excluded", "matched", "proposed", "proposed"}
	if got := e.lineStatuses(t, sc.statement); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("line statuses: got %v, want %v", got, want)
	}
	if got := len(events(t, e.Tenant, "bank_statement_line_excluded")); got != 1 {
		t.Errorf("excluded events: got %d, want 1", got)
	}
	pgtest.Exec(t, `UPDATE payments.bank_statement_line SET status = 'pending_approval', version = version + 1 WHERE statement_id = $1 AND line_no = 3`, sc.statement)
	for name, c := range map[string]struct {
		status     int
		code, path string
		body       string
		asTheAgent bool
	}{
		"a statement changed since it was read": {412, "request.version_mismatch", line(5, "exclude"), `{"reason": "x"}`, false},
		"a payment matched to another line":     {422, "payments.statement_match_target", line(5, "match"), `{"payment_ids": ["` + pays[0].payment + `"], "reason": "x"}`, false},
		"no target":                             {422, "request.validation_failed", line(5, "match"), `{"reason": "x"}`, false},
		"another company's posting rule":        {422, "payments.statement_match_target", line(5, "match"), `{"posting_rule": "INTR", "reason": "x"}`, false},
		"no such line":                          {404, "request.not_found", line(99, "exclude"), `{"reason": "x"}`, false},
		"a line on its way to the ledger":       {409, "payments.statement_line_posting", line(3, "exclude"), `{"reason": "x"}`, false},
		"an agent":                              {403, "identity.agent_not_allowed", line(5, "exclude"), `{"reason": "x"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := e.in(t)
			if c.asTheAgent {
				c2 = c2.agent()
			}
			ifMatch := tag()
			if c.status == 412 {
				ifMatch = stale
			}
			c2.Problem(c.status, c.code, "POST", c.path, c.body, ifMatch)
		})
	}
}

// anyVersion passes any version, as If-Match: * does.
func anyVersion(int32) error { return nil }

// TestMatchOpenLinesAgain: running the rules again takes up the lines a
// rule left open against the candidates there are now, and leaves matched
// and excluded lines; a second run changes nothing (T49e).
func TestMatchOpenLinesAgain(t *testing.T) {
	sc := matchStatementSetup(t)
	e, pays, line := sc.e, sc.pays, func(n int, action string) string {
		return fmt.Sprintf("/v1/bank-statements/%s/lines/%d:%s", sc.statement, n, action)
	}
	_, next := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	_, next = e.Must(201, "POST", line(5, "match"), `{"payment_ids": ["`+pays[2].payment+`"], "reason": "x"}`, next)
	_, next = e.Must(201, "POST", line(8, "match"), `{"payment_ids": ["`+pays[3].payment+`"], "reason": "x"}`, next)
	h := endpoints(nil)
	h.Objects = e.objects
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "an open line taken up again", func(ctx context.Context) error {
		_, err := h.MatchStatement(ctx, uuidOf(sc.statement), anyVersion)
		return err
	})
	_, next = e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", next)
	if got := e.lineStatuses(t, sc.statement)[3]; got != "unmatched" {
		t.Errorf("line 4, its candidates taken: got %s, want unmatched again", got)
	}
	_, next = e.Must(201, "POST", line(4, "exclude"), `{"reason": "x"}`, next)
	_, next = e.Must(201, "POST", line(1, "exclude"), `{"reason": "Booked twice by the bank; line 9 stands."}`, next)
	_, after := e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", next)
	want := []string{"excluded", "matched", "matched", "excluded", "matched", "unmatched", "unmatched", "matched", "matched"}
	if got := e.lineStatuses(t, sc.statement); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("line statuses: got %v, want %v", got, want)
	}
	for n, statuses := range map[int]string{4: "rejected excluded", 9: "rejected matched", 1: "rejected excluded", 2: "matched"} {
		var got []string
		for _, m := range e.matchesOf(t, sc.statement, n) {
			got = append(got, m.(map[string]any)["status"].(string))
		}
		if strings.Join(got, " ") != statuses {
			t.Errorf("line %d's matches: got %v, want %s", n, got, statuses)
		}
	}
	if m := e.matchesOf(t, sc.statement, 9)[1].(map[string]any); m["kind"] != "rule" || strings.Join(targetsOf(m), "") != "1:"+pays[0].payment {
		t.Errorf("line 9: got %v, want the rule's match of the payment line 1 no longer holds", m)
	}
	count := len(e.matchesOf(t, sc.statement, 0))
	t.Run("twice", func(t *testing.T) {
		_, again := e.in(t).Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", after)
		if got := len(e.in(t).matchesOf(t, sc.statement, 0)); again != after || got != count {
			t.Errorf("second run: got ETag %s and %d matches, want %s and %d", again, got, after, count)
		}
	})
}

// TestMatchToAFixedPoint: a line matched after an open one takes a payment
// the open one counted, and the open one is taken up again in the same
// run, so a second run changes nothing.
func TestMatchToAFixedPoint(t *testing.T) {
	e := matchSetup(t)
	a, b := e.paidOne(t, "INV-A", "2026-04-15"), e.paidOne(t, "INV-B", "2026-04-16")
	s := e.imported(t, "mt940", mtStatement("P1", "1", "500,00", "260416", "300,00", "260417",
		":61:2604170417D100,00NTRFNONREF//B1\n:86:/NAME/supplier\n",                       // 1: both payments by amount, until line 2 takes one
		":61:2604170417D100,00NTRFNONREF//B2\n:86:/EREF/"+a.e2e+"\n"))[0].(map[string]any) // 2: the first by its end-to-end id
	if got := e.lineStatuses(t, id(s)); strings.Join(got, ",") != "matched,matched" {
		t.Errorf("line statuses: got %v, want both matched", got)
	}
	ms := e.matchesOf(t, id(s), 1)
	if len(ms) != 2 || strings.Join(targetsOf(ms[1].(map[string]any)), "") != "1:"+b.payment {
		t.Errorf("line 1's matches: got %v, want its open match rejected and the second payment matched", ms)
	}
	_, tag := e.Must(200, "GET", "/v1/bank-statements/"+id(s), "", "")
	if _, again := e.Must(200, "POST", "/v1/bank-statements/"+id(s)+":match", "", tag); again != tag {
		t.Errorf("second run: got ETag %s, want %s unchanged", again, tag)
	}
}

// TestMatchingDatabaseFailures: every statement of matching fails in turn,
// and none runs outside a unit of work.
func TestMatchingDatabaseFailures(t *testing.T) {
	sc := matchStatementSetup(t)
	e, pays := sc.e, sc.pays
	h := endpoints(nil)
	h.Objects = e.objects
	st := uuidOf(sc.statement)
	file := mtStatement("M2", "2", "429,70", "260418", "419,20", "260418", ":61:2604180418D10,50NCHGNONREF//C1\n")
	for name, fn := range map[string]func(ctx context.Context) error{
		"import": func(ctx context.Context) error {
			_, _, _, err := h.ImportStatements(ctx, e.bank, "mt940", []byte(file))
			return err
		},
		"match": func(ctx context.Context) error { _, err := h.MatchStatement(ctx, st, anyVersion); return err },
		"by hand": func(ctx context.Context) error {
			_, err := h.MatchLine(ctx, st, 8, anyVersion, payments.ManualMatch{PaymentIDs: []uuid.UUID{uuidOf(pays[3].payment)}, Reason: "x"})
			return err
		},
		"posting rule": func(ctx context.Context) error {
			_, err := h.MatchLine(ctx, st, 5, anyVersion, payments.ManualMatch{PostingRule: "NCHG", Reason: "x"})
			return err
		},
		"exclude": func(ctx context.Context) error { _, err := h.ExcludeLine(ctx, st, 4, anyVersion, "x"); return err },
		"matches": func(ctx context.Context) error {
			_, err := h.ListBankStatementMatches(ctx, gen.ListBankStatementMatchesParams{ID: st})
			return err
		},
		"run handler": func(ctx context.Context) error {
			_, err := h.MatchBankStatement(ctx, gen.MatchBankStatementParams{ID: st, IfMatch: "*"})
			return err
		},
		"line handler": func(ctx context.Context) error {
			_, err := h.MatchBankStatementLine(ctx, &gen.StatementLineMatchInput{PostingRule: gen.NewOptString("NCHG"), Reason: "x"},
				gen.MatchBankStatementLineParams{ID: st, LineNo: 5, IfMatch: "*"})
			return err
		},
		"exclude handler": func(ctx context.Context) error {
			_, err := h.ExcludeBankStatementLine(ctx, &gen.ReasonInput{Reason: "x"}, gen.ExcludeBankStatementLineParams{ID: st, LineNo: 5, IfMatch: "*"})
			return err
		},
	} {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
	}
	for name, err := range map[string]error{
		"match": func() error { _, err := h.MatchStatement(context.Background(), st, anyVersion); return err }(),
		"by hand": func() error {
			_, err := h.MatchLine(context.Background(), st, 5, anyVersion, payments.ManualMatch{PostingRule: "NCHG"})
			return err
		}(),
		"matches": func() error { _, err := payments.StatementMatches(context.Background(), st, 0); return err }(),
	} {
		t.Run(name, func(t *testing.T) { pgtest.WantErr(t, err, "") })
	}
}
