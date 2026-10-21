package payments_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// maple is company 3000, Maple Inc, a Canadian company in CAD (ADR-0051
// §3, T53a): company 2000's chart and settings, a CAD house bank MAIN and a
// USD house bank USD, both at a Canadian bank, the USD bank's clearing
// (102000), main (103000) and incoming (104000) accounts revalued at period
// end; payment method T ranks MAIN in CAD and USD in USD; its statements
// match by end-to-end id and post without approval.
func maple(doc *config.Config) {
	co := *doc.Company("2000")
	co.Code, co.Name, co.Currency, co.Country = "3000", "Maple Inc", "CAD", "CA"
	co.ChartOfAccounts = append(slices.Clone(co.ChartOfAccounts),
		config.Account{Number: "102000", Name: "Bank clearing - USD", Type: config.Asset, OpenItems: true, RevalueForeignCurrency: true},
		config.Account{Number: "103000", Name: "Bank - USD", Type: config.Asset, RevalueForeignCurrency: true},
		config.Account{Number: "104000", Name: "Bank incoming - USD", Type: config.Asset, OpenItems: true, RevalueForeignCurrency: true},
		config.Account{Number: "420000", Name: "Exchange gains", Type: config.Revenue},
		config.Account{Number: "630000", Name: "Exchange losses", Type: config.Expense})
	co.TaxCodes = slices.Clone(co.TaxCodes)
	co.AccountDetermination.FXGain, co.AccountDetermination.FXLoss = "420000", "630000"
	co.HouseBank = config.HouseBank{Name: "MAIN", BIC: "ROYCCAT2", InstitutionNumber: "003", TransitNumber: "00002", AccountNumber: "1000001"}
	co.HouseBanks = []config.HouseBank{{Name: "USD", BIC: "ROYCCAT2", InstitutionNumber: "003", TransitNumber: "00002", AccountNumber: "4000001",
		GLAccount: "102000", MainGLAccount: "103000", IncomingGLAccount: "104000", Currency: "USD"}}
	co.PaymentMethods = []config.PaymentMethod{{Code: "T", Description: "Bank transfer",
		Ranking: []config.BankRanking{{Currency: "CAD", HouseBanks: []string{"MAIN"}}, {Currency: "USD", HouseBanks: []string{"USD"}}}}}
	co.FXValuation = config.FXValuation{}
	for _, n := range []string{"102000", "103000", "104000"} {
		co.FXValuation.Accounts = append(co.FXValuation.Accounts, config.FXValuationAccount{Account: n, UnrealisedGain: "420000", UnrealisedLoss: "630000"})
	}
	co.BankStatements = config.BankStatements{MatchingRules: []config.MatchingRule{{Kind: config.MatchEndToEndID}}, ApprovalTiers: []config.ApprovalTier{{}}}
	doc.Companies = append(doc.Companies, co)
}

// mapleScenario is a tenant with Maple Inc configured, its legal entity
// and its USD house bank.
type mapleScenario struct {
	e       env
	le, usd uuid.UUID
}

func mapleSetup(t *testing.T) mapleScenario {
	t.Helper()
	store := blobstoretest.New(t)
	objects := store.Store()
	objects.Encryption = blobstore.Encryption{Algorithm: blobstore.SSEKMS}
	f := fixture.NewWith(t, []fixture.Company{{Code: "3000", Name: "Maple Inc", Country: "CA", Currency: "CAD"}}, maple,
		append(append(slices.Clone(perms), statementPerms...), "payments.statement.match", "payments.statement.post")...)
	e := setupIn(t, f, func(cfg *app.Config) { cfg.Objects, cfg.Retention = objects, time.Hour })
	e.objects = objects
	sc := mapleScenario{e: e}
	pgtest.Scan(t, `SELECT le.id, h.id FROM tenancy.legal_entity le JOIN payments.house_bank h ON h.legal_entity_id = le.id AND h.code = 'USD'
		WHERE le.tenant_id = $1 AND le.code = '3000'`, []any{e.Tenant.ID}, &sc.le, &sc.usd)
	// USD in CAD and in the group's EUR: 1.35 when the invoice is posted,
	// 1.37 on the run date, 1.38 on the statement's booking date, 1.40 at
	// the month's end; the revaluation's header converts CAD to EUR.
	for _, r := range []struct{ from, to, day, rate string }{
		{"USD", "CAD", "2026-01-01", "1.35"}, {"USD", "CAD", "2026-04-15", "1.37"}, {"USD", "CAD", "2026-04-16", "1.38"}, {"USD", "CAD", "2026-04-30", "1.40"},
		{"USD", "EUR", "2026-01-01", "0.92"}, {"USD", "EUR", "2026-04-15", "0.93"}, {"USD", "EUR", "2026-04-16", "0.94"}, {"USD", "EUR", "2026-04-30", "0.95"},
		{"CAD", "EUR", "2026-01-01", "0.68"},
	} {
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', $2, $3, $4, $5, 'BOC', $6)`, e.Tenant.ID, r.from, r.to, r.day, r.rate, e.Tenant.Human.ID)
	}
	return sc
}

// TestForeignCurrencyHouseBankEndToEnd: a Canadian company's USD account is
// a house bank (ADR-0051 §3, T53a). The configuration keeps its currency;
// a run in USD pays from it, its file debits the USD account in USD, the
// bank's USD statement is imported, matched and posted at the booking
// date's rate with the realised difference, and the statement reconciles
// against the ledger's USD balance of the bank account, which the
// month-end valuation then revalues.
func TestForeignCurrencyHouseBankEndToEnd(t *testing.T) {
	sc := mapleSetup(t)
	var (
		run, payment map[string]any
		statement    string
	)
	_ = t.Run("configuration", func(t *testing.T) { sc.configured(t) }) &&
		t.Run("payment run in USD", func(t *testing.T) { run, payment = sc.paymentRun(t) }) &&
		t.Run("bank file", func(t *testing.T) { sc.bankFile(t, run) }) &&
		t.Run("statement imported, matched and posted", func(t *testing.T) { statement = sc.statement(t, payment) }) &&
		t.Run("reconciled", func(t *testing.T) { sc.reconciled(t, statement) }) &&
		t.Run("revalued at the month's end", func(t *testing.T) { sc.revalued(t) })
}

// configured: the house banks are in their currencies, shown by the API,
// and the USD bank's accounts are revalued.
func (sc mapleScenario) configured(t *testing.T) {
	t.Helper()
	e := sc.e.in(t)
	page, _ := e.Must(200, "GET", "/v1/house-banks?legal_entity_id="+sc.le.String(), "", "")
	got := map[string]string{}
	for _, b := range page["items"].([]any) {
		m := b.(map[string]any)
		got[m["code"].(string)] = m["currency"].(string) + " " + m["account_last4"].(string)
	}
	for code, want := range map[string]string{"MAIN": "CAD 0001", "USD": "USD 0001"} {
		if got[code] != want {
			t.Errorf("house bank %s: got %q, want %q", code, got[code], want)
		}
	}
	var revalued []string
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT a.number FROM ledger.company_account ca JOIN ledger.gl_account a ON a.id = ca.account_id
		WHERE ca.legal_entity_id = $1 AND ca.revalue_foreign_currency ORDER BY a.number`, sc.le)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		revalued = append(revalued, n)
	}
	if want := []string{"102000", "103000", "104000"}; !slices.Equal(revalued, want) {
		t.Errorf("revalued accounts: got %v, want %v", revalued, want)
	}
}

// paymentRun: an invoice of USD 1,000.00 posted at 1.35 is paid by a run
// in USD through the USD bank, which the ranking picks, released at 1.37:
// the clearing account is credited USD 1,000.00 (CAD 1,370.00) and the
// difference is a realised loss. A run for the USD bank in CAD is refused.
func (sc mapleScenario) paymentRun(t *testing.T) (run, payment map[string]any) {
	t.Helper()
	e := sc.e.in(t)
	pgtest.Exec(t, `UPDATE partner.supplier_company SET payment_methods = '{T}', version = version + 1 WHERE partner_id = $1 AND legal_entity_id = $2`,
		e.Supplier, sc.le)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "US-1",
		"invoice_date": "2026-03-02", "gross_amount": {"amount": "1000.00", "currency": "USD"}, "tax_amount": {"amount": "0.00", "currency": "USD"},
		"items": [{"account_id": %q, "description": "Parts", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "1000.00", "currency": "USD"}}]}`,
		sc.le, e.Supplier, e.Account(t, "3000", "520000")), "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-02"}`, `"1"`)
	e.Problem(422, "payments.currency_mismatch", "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q,
		"currency": "CAD", "run_date": "2026-04-15", "due_by": "2026-04-15"}`, sc.le, sc.usd), "")
	op, _ := e.Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "USD", "run_date": "2026-04-15",
		"due_by": "2026-04-15"}`, sc.le), "")
	work(t)
	run, _ = e.Must(200, "GET", op["resource"].(string), "", "")
	pays := e.payments(t, run)
	if len(pays) != 1 {
		t.Fatalf("payments: got %v, want 1", pays)
	}
	payment = pays[0].(map[string]any)
	if got := fmt.Sprint(payment["house_bank_id"], " ", payment["payment_method"], " ", payment["amount"]); got != sc.usd.String()+" T map[amount:1000.00 currency:USD]" {
		t.Errorf("payment: got %s, want the USD bank, method T, USD 1000.00", got)
	}
	e.release(t, run, "")
	payment = e.payments(t, run)[0].(map[string]any)
	want := map[string]money.Minor{"211000": 135000, "102000": -137000, "630000": 2000}
	if got := booked(t, payment["journal_entry_id"].(string), "amount_lc"); !maps.Equal(got, want) {
		t.Errorf("payment entry in CAD: got %v, want %v", got, want)
	}
	return run, payment
}

// bankFile: the run's one file is pain.001 in USD from the USD account,
// which it names with its currency.
func (sc mapleScenario) bankFile(t *testing.T, run map[string]any) {
	t.Helper()
	e := sc.e.in(t)
	files, _ := e.Must(200, "GET", "/v1/bank-files?payment_run_id="+id(run), "", "")
	items := files["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("bank files: got %v, want 1", items)
	}
	e.transmit(t, id(items[0].(map[string]any)), `"1"`)
	if len(e.connector.files) != 1 {
		t.Fatalf("files transmitted: got %d, want 1", len(e.connector.files))
	}
	xml := string(e.connector.files[0].Content)
	for _, want := range []string{`<InstdAmt Ccy="USD">1000.00</InstdAmt>`, "<Id>4000001</Id>", "<Ccy>USD</Ccy>", "<MmbId>000300002</MmbId>"} {
		if !strings.Contains(xml, want) {
			t.Errorf("file: got\n%s\nwant it to contain %s", xml, want)
		}
	}
}

// statement: the bank's USD statement, opening at USD 5,000.00 (posted to
// the bank account at 1.35 as the opening balance), is imported; its debit
// is matched to the payment by its end-to-end id and posted at once at the
// booking date's 1.38: the clearing line is cleared at its CAD 1,370.00,
// the bank account credited CAD 1,380.00, the difference a realised loss.
func (sc mapleScenario) statement(t *testing.T, payment map[string]any) string {
	t.Helper()
	e := sc.e.in(t)
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	text := "Opening balance"
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := post.Post(ctx, ledger.Posting{LegalEntityID: sc.le, DocumentType: "SA", DocumentDate: date("2026-04-01"), PostingDate: date("2026-04-01"),
			Currency: "USD", HeaderText: &text, SourceContext: "payments", SourceDocumentID: uuid.Must(uuid.NewV7()),
			Lines: []ledger.PostingLine{{AccountID: e.Account(t, "3000", "103000"), Amount: 500000}, {AccountID: e.Account(t, "3000", "300000"), Amount: -500000}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt><GrpHdr><MsgId>MSG-USD-1</MsgId></GrpHdr>
<Stmt><Id>USD-1</Id><ElctrncSeqNb>1</ElctrncSeqNb><Acct><Id><Othr><Id>4000001</Id></Othr></Id><Ccy>USD</Ccy></Acct>
<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">5000.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-04-16</Dt></Dt></Bal>
<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">4000.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-04-17</Dt></Dt></Bal>
<Ntry><Amt Ccy="USD">1000.00</Amt><CdtDbtInd>DBIT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>2026-04-17</Dt></BookgDt><ValDt><Dt>2026-04-17</Dt></ValDt>
<BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>ICDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd>
<NtryDtls><TxDtls><Refs><EndToEndId>` + payment["end_to_end_id"].(string) + `</EndToEndId></Refs></TxDtls></NtryDtls></Ntry>
</Stmt></BkToCstmrStmt></Document>`
	code, body := e.upload(t, sc.usd.String(), "camt.053", doc)
	if code != 201 {
		t.Fatalf("import: got %d %v, want 201", code, body)
	}
	s := body["statements"].([]any)[0].(map[string]any)
	if got := e.lineStatuses(t, id(s)); !slices.Equal(got, []string{"posted"}) {
		t.Fatalf("line statuses: got %v, want [posted]", got)
	}
	ms := e.matchesOf(t, id(s), 1)
	entry := ms[len(ms)-1].(map[string]any)["journal_entry_id"].(string)
	want := map[string]money.Minor{"102000": 137000, "103000": -138000, "630000": 1000}
	if got := booked(t, entry, "amount_lc"); !maps.Equal(got, want) {
		t.Errorf("statement posting in CAD: got %v, want %v", got, want)
	}
	var open int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_line l JOIN ledger.gl_account a ON a.id = l.account_id
		WHERE l.legal_entity_id = $1 AND a.number = '102000' AND NOT EXISTS (SELECT FROM ledger.clearing_item ci
			WHERE ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no)`,
		[]any{sc.le}, &open)
	if open != 0 {
		t.Errorf("open lines on the USD clearing account: got %d, want 0", open)
	}
	return id(s)
}

// reconciled: compared in USD, the ledger's bank account holds the
// statement's closing balance, so the statement is reconciled; in CAD it
// would differ by the exchange differences.
func (sc mapleScenario) reconciled(t *testing.T, statement string) {
	t.Helper()
	e := sc.e.in(t)
	s, _ := e.Must(200, "GET", "/v1/bank-statements/"+statement, "", "")
	if s["status"] != "reconciled" {
		t.Errorf("statement status: got %v, want reconciled", s["status"])
	}
	r, _ := e.Must(200, "GET", "/v1/house-banks/"+sc.usd.String()+"/reconciliation", "", "")
	if r["currency"] != "USD" {
		t.Errorf("report currency: got %v, want USD", r["currency"])
	}
	figures := field(r, "summary", "figures").(map[string]any)
	for name, want := range map[string]string{"closing_balance": "4000.00", "ledger_balance": "4000.00", "unexplained": "0.00", "in_transit": "0.00"} {
		if got := field(figures, name, "amount"); got != want {
			t.Errorf("figures %s: got %v, want %s", name, got, want)
		}
		if got := field(figures, name, "currency"); got != "USD" {
			t.Errorf("figures %s currency: got %v, want USD", name, got)
		}
	}
}

// revalued: at the month's end the bank account's USD 4,000.00, booked at
// CAD 5,370.00, is worth CAD 5,600.00 at 1.40: an unrealised gain of
// CAD 230.00; the cleared clearing account holds nothing to value.
func (sc mapleScenario) revalued(t *testing.T) {
	t.Helper()
	e := sc.e.in(t)
	p, _ := e.Must(200, "POST", "/v1/revaluation-runs/preview", fmt.Sprintf(`{"legal_entity_id": %q, "key_date": "2026-04-30"}`, sc.le), "")
	bank, clearing := e.Account(t, "3000", "103000").String(), e.Account(t, "3000", "102000").String()
	found := false
	for _, it := range p["items"].([]any) {
		m := it.(map[string]any)
		switch m["account_id"] {
		case bank:
			found = true
			got := fmt.Sprint(field(m, "amount", "amount"), " ", field(m, "amount", "currency"), " ", field(m, "difference_lc", "amount"))
			if want := "4000.00 USD 230.00"; got != want {
				t.Errorf("bank account item: got %s, want %s", got, want)
			}
		case clearing:
			t.Errorf("clearing account item: got %v, want none", m)
		}
	}
	if !found {
		t.Errorf("revaluation items: got %v, want the USD bank account's", p["items"])
	}
}

// TestHouseBankCurrency: a house bank is kept in its configured currency,
// or its company's, and a document that gives a bank another currency
// under the same name is refused, as another account is: a bank's
// statements and balances stay in one currency (ADR-0051 §3, T53a).
func TestHouseBankCurrency(t *testing.T) {
	e := setup(t)
	h := houseBanks()
	usd := further("DB")
	usd.Currency = "USD"
	doc := fixture.Document(t)
	doc.Companies[1].HouseBanks = []config.HouseBank{usd}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.Apply(ctx, doc) }); err != nil {
		t.Fatal(err)
	}
	got := banksOf(t, e.DE)
	for code, want := range map[string]string{"MAIN": "EUR", "DB": "USD"} {
		if got[code].currency != want {
			t.Errorf("house bank %s currency: got %s, want %s", code, got[code].currency, want)
		}
	}
	for name, c := range map[string]struct {
		mutate func(co *config.Company)
		want   string // the finding's message, "" none
	}{
		"the same currencies":          {func(*config.Company) {}, ""},
		"the company's currency named": {func(co *config.Company) { co.HouseBank.Currency = "EUR" }, ""},
		"MAIN in USD":                  {func(co *config.Company) { co.HouseBank.Currency = "USD" }, "house bank MAIN already has another currency, EUR; give the new bank account a new name"},
		"DB in the company's currency": {func(co *config.Company) { co.HouseBanks[0].Currency = "" }, "house bank DB already has another currency, USD; give the new bank account a new name"},
		"DB with another account and currency": {func(co *config.Company) {
			co.HouseBanks[0].IBAN, co.HouseBanks[0].Currency = "DE89370400440532013000", ""
		}, "house bank DB already has other bank details; give the new bank account a new name"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := fixture.Document(t)
			doc.Companies[1].HouseBanks = []config.HouseBank{usd}
			c.mutate(&doc.Companies[1])
			var findings []config.Finding
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				findings, err = h.Check(ctx, nil, doc)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var got string
			if len(findings) > 0 {
				got = findings[0].Message
			}
			if len(findings) > 1 || got != c.want {
				t.Errorf("findings: got %v, want %q", findings, c.want)
			}
		})
	}
	// Applied again in the company's currency (a document Check refused),
	// the bank keeps its own.
	doc = fixture.Document(t)
	doc.Companies[1].HouseBanks = []config.HouseBank{further("DB")}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.Apply(ctx, doc) }); err != nil {
		t.Fatal(err)
	}
	if got := banksOf(t, e.DE)["DB"].currency; got != "USD" {
		t.Errorf("house bank DB currency after a document in EUR: got %s, want USD", got)
	}
}
