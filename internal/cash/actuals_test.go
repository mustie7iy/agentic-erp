package cash_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// actualPerms are what the actuals' people hold beside cash.forecast.read:
// the read permissions of a line's statement, payments and suppliers.
var actualPerms = []string{"payments.statement.read", "payments.run.read", "partner.supplier.read"}

// withActuals is the drill-down's document (withDrill: rent, capital
// expenditure, taxes by the planning group TAX_AUTH, supplier payments and
// customer receipts the defaults) with bank charges, the item of Maple's
// posting rule NCHG.
func withActuals(doc *config.Config) {
	withDrill(doc)
	doc.Cash.LiquidityItems = append(doc.Cash.LiquidityItems,
		config.LiquidityItem{Code: "BANK_CHARGES", Name: "Bank charges", Direction: config.DirectionBoth, Parent: "OPERATING"})
	doc.Cash.Derivation.PostingRules = []config.PostingRuleDerivation{{Rule: "NCHG", Item: "BANK_CHARGES"}}
	co := doc.Company("3000")
	co.BankStatements.PostingRules = []config.StatementPostingRule{{TransactionCode: "NCHG", Description: "Charges", Account: "520000"}}
}

// actualsEnv is Maple Inc read on Friday 17 April 2026 with the
// scenario's statements: ids by name.
type actualsEnv struct {
	forecastEnv
	cadLines, usdLines uuid.UUID // the statements
	docs               map[string]uuid.UUID
}

// target is what a line's match clears: payments, a posting rule, or the
// incoming payments clearing account.
type target struct {
	payments []uuid.UUID
	rule     string
	incoming bool
}

// actualLine writes a statement line in a status with a match in a status
// ("" for none): an excluded match a person's, any other a rule's.
func (e actualsEnv) actualLine(t *testing.T, statement uuid.UUID, no int, day string, amount int64, status, match string, to target) {
	t.Helper()
	e.exec(t, `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code, bank_reference,
			owner_reference, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5, 'NCHG', 'B', 'O', 'T', $6, $7, $7)`, e.Tenant.ID, statement, no, amount, day, status, e.Tenant.Human.ID)
	if match == "" {
		return
	}
	id := uuid.Must(uuid.NewV7())
	kind, rule := "rule", any("amount")
	if match == "excluded" {
		kind, rule = "manual", nil
	}
	e.exec(t, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'why', $7, $8, $8)`, e.Tenant.ID, id, statement, no, kind, rule, match, e.Tenant.Human.ID)
	for i, p := range to.payments {
		e.exec(t, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, payment_id, created_by) VALUES ($1, $2, 1, $3, $4, $5)`,
			e.Tenant.ID, id, i+1, p, e.Tenant.Human.ID)
	}
	if to.rule != "" || to.incoming {
		var rule any
		if to.rule != "" {
			rule = to.rule
		}
		e.exec(t, `INSERT INTO payments.statement_match_target (tenant_id, match_id, candidate, target_no, posting_rule, incoming, created_by)
			VALUES ($1, $2, 1, 1, $3, $4, $5)`, e.Tenant.ID, id, rule, to.incoming, e.Tenant.Human.ID)
	}
}

// paid writes a released run's payment of the amount to the supplier,
// settling the items (entry ids) at the amounts.
func (e actualsEnv) paid(t *testing.T, supplier uuid.UUID, amount int64, items map[uuid.UUID]int64) uuid.UUID {
	t.Helper()
	p := e.payment(t, e.run(t, e.main, "2026-04-10", "CAD", "released"), amount, "CAD", nil)
	e.exec(t, `UPDATE payments.payment SET supplier_id = $2 WHERE id = $1`, p, supplier)
	for item, a := range items {
		e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
			VALUES ($1, $2, 2026, $3, 1, $4, '2026-05-01')`, e.Tenant.ID, p, item, a)
	}
	return p
}

// setupActuals writes Maple Inc's statement lines, read on Friday 17 April
// 2026:
//   - CAD (MAIN), a statement of 12-17 April: on the 12th a line before
//     the range; on the 13th -800.00 posted for a payment settling an
//     invoice of rent 500.00 and equipment 300.00 (rent and capital
//     expenditure); on the 14th -101.00 matched to a payment of 100.00 to
//     a tax authority settling no item (taxes, the bank's 1.00 with it)
//     and -15.00 posted by the posting rule NCHG (bank charges); on the
//     15th a receipt of 2,000.00 not matched and -50.00 a rule only
//     proposes (their defaults, unclassified); on the 16th -7.00 excluded
//     by a person and 300.00 matched to the incoming payments clearing
//     account (their defaults); on the 17th a line after the range;
//   - USD, a statement of 16 April: -0.10 not matched.
func setupActuals(t *testing.T) actualsEnv {
	t.Helper()
	e := actualsEnv{forecastEnv: setupForecast(t, withActuals, actualPerms...), docs: map[string]uuid.UUID{}}
	s1, tax := e.NewSupplier(t, "BP90000051"), e.NewSupplier(t, "BP90000052")
	e.exec(t, `UPDATE partner.supplier_company SET planning_group = 'TAX_AUTH', version = version + 1 WHERE partner_id = $1 AND legal_entity_id = $2`, tax, e.le)
	invoice := e.document(t, s1, "2026-04-01", -80000, docLine{"530000", 50000, "", ""}, docLine{"510000", 30000, "", ""})
	e.docs["invoice payment"] = e.paid(t, s1, 80000, map[uuid.UUID]int64{invoice: 80000})
	e.docs["tax payment"] = e.paid(t, tax, 10000, nil)
	e.cadLines = e.statement(t, e.main, "CAD", "2026-04-12", "2026-04-17", 1000000, 1222700, nil, "imported")
	e.usdLines = e.statement(t, e.usd, "USD", "2026-04-16", "2026-04-16", 100000, 99990, nil, "imported")
	e.actualLine(t, e.cadLines, 1, "2026-04-12", 1000, "unmatched", "", target{})
	e.actualLine(t, e.cadLines, 2, "2026-04-13", -80000, "posted", "matched", target{payments: []uuid.UUID{e.docs["invoice payment"]}})
	e.actualLine(t, e.cadLines, 3, "2026-04-14", -10100, "matched", "matched", target{payments: []uuid.UUID{e.docs["tax payment"]}})
	e.actualLine(t, e.cadLines, 4, "2026-04-14", -1500, "posted", "matched", target{rule: "NCHG"})
	e.actualLine(t, e.cadLines, 5, "2026-04-15", 200000, "unmatched", "", target{})
	e.actualLine(t, e.cadLines, 6, "2026-04-15", -5000, "proposed", "open", target{payments: []uuid.UUID{e.docs["invoice payment"]}})
	e.actualLine(t, e.cadLines, 7, "2026-04-16", -700, "excluded", "excluded", target{})
	e.actualLine(t, e.cadLines, 8, "2026-04-16", 30000, "matched", "matched", target{incoming: true})
	e.actualLine(t, e.cadLines, 9, "2026-04-17", 2000, "unmatched", "", target{})
	e.actualLine(t, e.usdLines, 1, "2026-04-16", -10, "unmatched", "", target{})
	return e
}

// actualsOf reads the actuals of 13-16 April with the query.
func (e actualsEnv) actualsOf(t *testing.T, query string) map[string]any {
	t.Helper()
	a, _ := e.Must(200, "GET", "/v1/cash-actuals?legal_entity_id="+e.le.String()+"&from=2026-04-13&to=2026-04-16"+query, "", "")
	return a
}

// num is an amount without its currency.
func num(v any) string { return strings.Fields(amount(v))[0] }

// actualSums renders each currency's buckets that have lines as "start in/out
// total xcount (unclassified xcount)", its banks and its total.
func actualSums(t *testing.T, a map[string]any) []string {
	t.Helper()
	sum := func(m map[string]any) string {
		u := m["unclassified"].(map[string]any)
		return fmt.Sprintf("%s/%s %s x%v (%s x%v)", num(m["inflow"]), num(m["outflow"]), amount(m["total"]), m["count"], num(u["amount"]), u["count"])
	}
	var out []string
	for _, c := range a["currencies"].([]any) {
		cm := c.(map[string]any)
		line := fmt.Sprint(cm["currency"], ":")
		for _, b := range cm["buckets"].([]any) {
			if bm := b.(map[string]any); bm["count"] != float64(0) {
				line += fmt.Sprintf(" %s %s;", bm["start"], sum(bm))
			}
		}
		for _, b := range cm["banks"].([]any) {
			bm := b.(map[string]any)
			var bs []string
			for _, x := range bm["buckets"].([]any) {
				bs = append(bs, num(x))
			}
			line += fmt.Sprintf(" bank %s active=%v %v %s x%v;", bm["house_bank_code"], bm["active"], bs, num(bm["total"]), bm["count"])
		}
		out = append(out, line+" total "+sum(cm))
	}
	return out
}

// actualItems renders a currency's rows by liquidity item: each indented
// by its level with its total, then its own shares by match.
func actualItems(t *testing.T, a map[string]any, cur string) []string {
	t.Helper()
	var out []string
	for _, c := range a["currencies"].([]any) {
		cm := c.(map[string]any)
		if cm["currency"] != cur {
			continue
		}
		for _, r := range cm["items"].([]any) {
			rm := r.(map[string]any)
			line := fmt.Sprintf("%s%v %s", strings.Repeat("  ", int(rm["level"].(float64))), rm["liquidity_item"], num(rm["total"]))
			for _, m := range rm["matches"].([]any) {
				mm := m.(map[string]any)
				line += fmt.Sprintf(" %s %s", mm["match"], num(mm["total"]))
			}
			out = append(out, line)
		}
	}
	return out
}

// TestCashActuals: Maple Inc's statement lines of 13-16 April per
// currency, bucket and house bank, classified as their matches say
// (ADR-0051 §4, §11): a payment's line by the items it settled, a line of
// a payment of no item by its supplier's planning group, a posting rule's
// line by the rule's item, an excluded or incoming line and one with no
// decided match by their direction's default, the last counted
// unclassified; every active bank listed, the lines before and after the
// range left out.
func TestCashActuals(t *testing.T) {
	e := setupActuals(t)
	a := e.actualsOf(t, "")
	want(t, "header", fmt.Sprint(a["company_code"], " ", a["read_date"], " ", a["from"], " ", a["to"], " ", a["grain"], " ", len(a["buckets"].([]any))),
		"3000 2026-04-17 2026-04-13 2026-04-16 day 4")
	want(t, "sums", actualSums(t, a), []string{
		"CAD: 2026-04-13 0.00/-800.00 -800.00 CAD x1 (0.00 x0); 2026-04-14 0.00/-116.00 -116.00 CAD x2 (0.00 x0);" +
			" 2026-04-15 2000.00/-50.00 1950.00 CAD x2 (1950.00 x2); 2026-04-16 300.00/-7.00 293.00 CAD x2 (0.00 x0);" +
			" bank MAIN active=true [-800.00 -116.00 1950.00 293.00] 1327.00 x7; total 2300.00/-973.00 1327.00 CAD x7 (1950.00 x2)",
		"USD: 2026-04-16 0.00/-0.10 -0.10 USD x1 (-0.10 x1); bank USD active=true [0.00 0.00 0.00 -0.10] -0.10 x1; total 0.00/-0.10 -0.10 USD x1 (-0.10 x1)",
	})
	want(t, "CAD items", actualItems(t, a, "CAD"), []string{
		"INVESTING -300.00",
		"  CAPEX -300.00 payment -300.00",
		"OPERATING 1627.00",
		"  BANK_CHARGES -15.00 posting_rule -15.00",
		"  OP_RENT -500.00 payment -500.00",
		"  OP_SUPPLIERS -57.00 excluded -7.00 unmatched -50.00",
		"  REC_CUSTOMERS 2300.00 incoming 300.00 unmatched 2000.00",
		"  TAX -101.00 payment -101.00",
	})

	t.Run("a week", func(t *testing.T) {
		a := e.actualsOf(t, "&grain=week")
		b := a["buckets"].([]any)
		want(t, "buckets", fmt.Sprint(len(b), " ", b[0].(map[string]any)["end"]), "1 2026-04-16")
		want(t, "CAD", actualSums(t, a)[0], "CAD: 2026-04-13 2300.00/-973.00 1327.00 CAD x7 (1950.00 x2); bank MAIN active=true [1327.00] 1327.00 x7;"+
			" total 2300.00/-973.00 1327.00 CAD x7 (1950.00 x2)")
	})
	t.Run("every active bank, a bank no longer active with its lines", func(t *testing.T) {
		a, _ := e.Must(200, "GET", "/v1/cash-actuals?legal_entity_id="+e.le.String()+"&from=2026-04-13&to=2026-04-13", "", "")
		want(t, "a day without USD lines", actualSums(t, a)[1], "USD: bank USD active=true [0.00] 0.00 x0; total 0.00/0.00 0.00 USD x0 (0.00 x0)")
		pgtest.Exec(t, `UPDATE payments.house_bank SET status = 'inactive', active_gl_account_id = NULL, version = version + 1 WHERE id = $1`, e.usd)
		want(t, "USD inactive", actualSums(t, e.actualsOf(t, ""))[1],
			"USD: 2026-04-16 0.00/-0.10 -0.10 USD x1 (-0.10 x1); bank USD active=false [0.00 0.00 0.00 -0.10] -0.10 x1; total 0.00/-0.10 -0.10 USD x1 (-0.10 x1)")
	})
}

// TestCashActualsUnclassified: without defaults, what no rule assigns is
// the row without a liquidity item, last.
func TestCashActualsUnclassified(t *testing.T) {
	e := actualsEnv{forecastEnv: setupForecast(t, func(doc *config.Config) {
		withActuals(doc)
		doc.Cash.Derivation.Defaults = config.DerivationDefaults{}
	})}
	e.cadLines = e.statement(t, e.main, "CAD", "2026-04-13", "2026-04-13", 0, 100, nil, "imported")
	e.actualLine(t, e.cadLines, 1, "2026-04-13", 100, "unmatched", "", target{})
	want(t, "items", actualItems(t, e.actualsOf(t, ""), "CAD"), []string{"<nil> 1.00 unmatched 1.00"})
}

// actualLinesOf lists every page of the lines of 13-16 April with the
// query, rendered, and the first page; no token names a statement.
func (e actualsEnv) actualLinesOf(t *testing.T, query string, size int) (map[string]any, []string) {
	t.Helper()
	var out []string
	var first map[string]any
	token := ""
	for {
		q := fmt.Sprintf("/v1/cash-actual-lines?legal_entity_id=%s&from=2026-04-13&to=2026-04-16%s&page_size=%d", e.le, query, size)
		if token != "" {
			q += "&page_token=" + url.QueryEscape(token)
		}
		p, _ := e.Must(200, "GET", q, "", "")
		if first == nil {
			first = p
		}
		for _, it := range p["items"].([]any) {
			line := e.actualLineOf(it.(map[string]any))
			if len(out) > 0 && line[:10] < out[len(out)-1][:10] {
				t.Errorf("value dates: got %s after %s, want them never earlier", line[:10], out[len(out)-1][:10])
			}
			out = append(out, line)
		}
		next, _ := p["next_page_token"].(string)
		if next == "" {
			return first, out
		}
		for _, s := range []uuid.UUID{e.cadLines, e.usdLines} {
			if strings.Contains(next, s.String()) {
				t.Errorf("page token: got %s naming statement %s, want none", next, s)
			}
		}
		token = next
	}
}

// actualLineOf renders a listed line, its payments by the scenario's
// names.
func (e actualsEnv) actualLineOf(l map[string]any) string {
	var shares, pays, sups []string
	for _, s := range l["shares"].([]any) {
		sm := s.(map[string]any)
		shares = append(shares, fmt.Sprintf("%v %s", sm["liquidity_item"], num(sm["amount"])))
	}
	for _, p := range l["payments"].([]any) {
		pm := p.(map[string]any)
		name := fmt.Sprint(pm["payment_id"])
		for n, id := range e.docs {
			if id.String() == name {
				name = n
			}
		}
		supplier := "-"
		if s, ok := pm["supplier"].(map[string]any); ok {
			supplier = fmt.Sprint(s["number"])
		}
		pays = append(pays, fmt.Sprintf("%s %v %s to %s", name, pm["payment_run_number"] != "", num(pm["amount"]), supplier))
	}
	for _, s := range l["suppliers"].([]any) {
		sups = append(sups, fmt.Sprint(s.(map[string]any)["number"]))
	}
	statement := "-"
	if s, ok := l["statement"].(map[string]any); ok {
		statement = fmt.Sprintf("%v(%v,%v)", s["line_no"], s["statement_ref"] != "", s["bank_reference"])
	}
	return fmt.Sprintf("%s %s #%s %s %s%v unclassified=%v %s share %s %v; %v of %v; %v", l["value_date"], l["house_bank_code"], statement, l["status"], l["match"],
		map[bool]string{true: "/" + fmt.Sprint(l["posting_rule"]), false: ""}[l["posting_rule"] != nil], l["unclassified"], amount(l["amount"]), num(l["share"]),
		shares, pays, l["payment_count"], sups)
}

// TestCashActualLines: the lines of a currency in pages whose tokens name
// no statement, each with its shares, what classified it, its statement,
// payments and suppliers; of an item and its descendants with that share,
// of a bank, or with no decided match; the total of the selected shares
// and the count over every page (ADR-0051 §11).
func TestCashActualLines(t *testing.T) {
	e := setupActuals(t)
	p, lines := e.actualLinesOf(t, "&currency=CAD", 2)
	want(t, "page", fmt.Sprint(p["company_code"], " ", p["from"], " ", p["to"], " ", amount(p["total"]), " ", p["count"], " ", p["unclassified"]),
		"3000 2026-04-13 2026-04-16 1327.00 CAD 7 false")
	wantLines := []string{
		"2026-04-13 MAIN #2(true,B) posted payment unclassified=false -800.00 CAD share -800.00 [OP_RENT -500.00 CAPEX -300.00]; [invoice payment true -800.00 to BP90000051] of 1; [BP90000051]",
		"2026-04-14 MAIN #4(true,B) posted posting_rule/NCHG unclassified=false -15.00 CAD share -15.00 [BANK_CHARGES -15.00]; [] of 0; []",
		"2026-04-14 MAIN #3(true,B) matched payment unclassified=false -101.00 CAD share -101.00 [TAX -101.00]; [tax payment true -100.00 to BP90000052] of 1; [BP90000052]",
		"2026-04-15 MAIN #5(true,B) unmatched unmatched unclassified=true 2000.00 CAD share 2000.00 [REC_CUSTOMERS 2000.00]; [] of 0; []",
		"2026-04-15 MAIN #6(true,B) proposed unmatched unclassified=true -50.00 CAD share -50.00 [OP_SUPPLIERS -50.00]; [] of 0; []",
		"2026-04-16 MAIN #7(true,B) excluded excluded unclassified=false -7.00 CAD share -7.00 [OP_SUPPLIERS -7.00]; [] of 0; []",
		"2026-04-16 MAIN #8(true,B) matched incoming unclassified=false 300.00 CAD share 300.00 [REC_CUSTOMERS 300.00]; [] of 0; []",
	}
	slices.Sort(lines)
	slices.Sort(wantLines)
	want(t, "lines", lines, wantLines)
	for _, c := range []struct{ name, query, total, count string }{
		{"an item and its descendants", "&currency=CAD&liquidity_item=OPERATING", "1627.00 CAD", "7"},
		{"an item", "&currency=CAD&liquidity_item=CAPEX", "-300.00 CAD", "1"},
		{"the unclassified", "&currency=CAD&unclassified=true", "1950.00 CAD", "2"},
		{"a bank", "&currency=CAD&house_bank_id=" + e.main.String(), "1327.00 CAD", "7"}, {"another bank", "&currency=CAD&house_bank_id=" + e.usd.String(), "0.00 CAD", "0"},
		{"another currency", "&currency=USD", "-0.10 USD", "1"},
		{"a currency of no line", "&currency=XYZ", "0.00 XYZ", "0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := e.actualLinesOf(t, c.query, 50)
			want(t, "total and count", fmt.Sprint(amount(p["total"]), " ", p["count"]), c.total+" "+c.count)
		})
	}
	_, capex := e.actualLinesOf(t, "&currency=CAD&liquidity_item=INVESTING", 50)
	want(t, "a share of a line", capex, []string{
		"2026-04-13 MAIN #2(true,B) posted payment unclassified=false -800.00 CAD share -300.00 [OP_RENT -500.00 CAPEX -300.00]; [invoice payment true -800.00 to BP90000051] of 1; [BP90000051]"})
}

// TestCashActualLinesGated: a caller holding cash.forecast.read alone sees
// the lines without their statements, payments or suppliers; one barred
// from a permission by a segregation-of-duties rule likewise.
func TestCashActualLinesGated(t *testing.T) {
	e := setupActuals(t)
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'payments.run.read', 'payments.statement.read', 'd', current_date, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	_, lines := e.actualLinesOf(t, "&currency=CAD&liquidity_item=CAPEX", 50)
	want(t, "statements and payments barred", lines, []string{
		"2026-04-13 MAIN #- posted payment unclassified=false -800.00 CAD share -300.00 [OP_RENT -500.00 CAPEX -300.00]; [] of 1; [BP90000051]"})
	for _, p := range actualPerms {
		e.Tenant.Revoke(t, p, e.Tenant.Human)
	}
	_, lines = e.actualLinesOf(t, "&currency=CAD&liquidity_item=CAPEX", 50)
	want(t, "the lines alone", lines, []string{
		"2026-04-13 MAIN #- posted payment unclassified=false -800.00 CAD share -300.00 [OP_RENT -500.00 CAPEX -300.00]; [] of 1; []"})
}

// TestCashActualsRefusals: a grain not known, a range ending before it
// starts or of more periods than the grain's limit, an item with the
// unclassified lines, and another company's legal entity are refused.
func TestCashActualsRefusals(t *testing.T) {
	e := setupActuals(t)
	base := "/v1/cash-actuals?legal_entity_id=" + e.le.String()
	lines := "/v1/cash-actual-lines?currency=CAD&legal_entity_id=" + e.le.String()
	for _, c := range []struct{ name, path, field string }{
		{"a grain not known", base + "&from=2026-04-13&to=2026-04-16&grain=year", "grain"},
		{"a range backwards", base + "&from=2026-04-13&to=2026-04-12", "to"},
		{"too many days", base + "&from=2026-01-01&to=2026-04-16", "to"},
		{"too many weeks", base + "&from=2024-01-01&to=2026-04-16&grain=week", "to"},
		{"lines over 36 months", lines + "&from=2023-01-01&to=2026-04-16", "to"},
		{"lines backwards", lines + "&from=2026-04-13&to=2026-04-12", "to"},
		{"an item with the unclassified", lines + "&from=2026-04-13&to=2026-04-16&liquidity_item=TAX&unclassified=true", "unclassified"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.Problem(422, "request.validation_failed", "GET", c.path, "", "")
			want(t, "field", fmt.Sprint(p["errors"].([]any)[0].(map[string]any)["field"]), c.field)
		})
	}
	e.Must(200, "GET", base+"&from=2026-04-13&to=2026-07-13", "", "")
	e.Must(200, "GET", base+"&from=2023-05-01&to=2026-04-30&grain=month", "", "")
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", base+"&from=2026-04-13&to=2026-04-16", "", "")
	e.As(other).Problem(404, "request.not_found", "GET", lines+"&from=2026-04-13&to=2026-04-16", "", "")
}

// TestCashActualsTooLarge: actuals whose sums are out of the range of an
// amount are refused.
func TestCashActualsTooLarge(t *testing.T) {
	e := actualsEnv{forecastEnv: setupForecast(t, withActuals)}
	e.cadLines = e.statement(t, e.main, "CAD", "2026-04-13", "2026-04-13", 0, 0, nil, "imported")
	e.actualLine(t, e.cadLines, 1, "2026-04-13", math.MaxInt64-1, "unmatched", "", target{})
	e.actualLine(t, e.cadLines, 2, "2026-04-13", math.MaxInt64-1, "unmatched", "", target{})
	e.Problem(422, "cash.amount_too_large", "GET", "/v1/cash-actuals?legal_entity_id="+e.le.String()+"&from=2026-04-13&to=2026-04-13", "", "")
	e.Problem(422, "cash.amount_too_large", "GET", "/v1/cash-actual-lines?currency=CAD&legal_entity_id="+e.le.String()+"&from=2026-04-13&to=2026-04-13", "", "")
}

// TestCashActualsFailures calls the actuals and their lines outside a unit
// of work and with each of their database statements failing in turn; the
// lines fail with the partner names too.
func TestCashActualsFailures(t *testing.T) {
	e := setupActuals(t)
	from, to := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)
	endpoints := func() *cash.Endpoints { return app.Cash(app.Config{Now: func() time.Time { return readAt }}) }
	for name, read := range map[string]func(ctx context.Context) error{
		"actuals": func(ctx context.Context) error {
			_, err := endpoints().Actuals(ctx, cash.ActualsQuery{LegalEntity: e.le, From: from, To: to})
			return err
		},
		"lines": func(ctx context.Context) error {
			_, err := endpoints().ActualLines(ctx, cash.ActualLinesQuery{LegalEntity: e.le, Currency: "CAD", From: from, To: to, PageSize: 50})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := read(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, read)
		})
	}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints().ListCashActualLines(ctx, gen.ListCashActualLinesParams{LegalEntityID: e.le, Currency: "CAD", From: from, To: to,
			PageSize: gen.NewOptInt32(0)})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "page_size") {
		t.Errorf("a page size of none: got %v, want it refused", err)
	}
	failing := endpoints()
	failing.Partners = failingPartners{}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := failing.ActualLines(ctx, cash.ActualLinesQuery{LegalEntity: e.le, Currency: "CAD", From: from, To: to, PageSize: 50})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "names of the forecast's suppliers") {
		t.Errorf("the suppliers' names not read: got %v, want the read failed", err)
	}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints().ActualLines(ctx, cash.ActualLinesQuery{LegalEntity: e.le, Currency: "CAD", From: from, To: to, PageSize: 50, PageToken: "x"})
		return err
	})
	if err == nil {
		t.Error("a page token not issued: got none, want it refused")
	}
}

// TestCashActualsOutOfRange: a line paying a document whose lines cannot
// be split in the range of an amount fails the actuals rather than
// classifying it wrongly; a company whose stored zone does not load fails
// them rather than reading the date in UTC.
func TestCashActualsOutOfRange(t *testing.T) {
	e := actualsEnv{forecastEnv: setupForecast(t, withActuals), docs: map[string]uuid.UUID{}}
	s := e.NewSupplier(t, "BP90000053")
	doc := e.document(t, s, "2026-04-01", -100, docLine{"530000", math.MaxInt64, "V1", ""}, docLine{"510000", -math.MaxInt64 + 1, "V1", ""},
		docLine{"140000", math.MaxInt64, "V1", "input"})
	e.cadLines = e.statement(t, e.main, "CAD", "2026-04-13", "2026-04-13", 0, -100, nil, "imported")
	e.actualLine(t, e.cadLines, 1, "2026-04-13", -100, "matched", "matched", target{payments: []uuid.UUID{e.paid(t, s, 100, map[uuid.UUID]int64{doc: 100})}})
	day := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	read := func() error {
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := endpoints.Actuals(ctx, cash.ActualsQuery{LegalEntity: e.le, From: day, To: day})
			return err
		})
	}
	if err := read(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("out of range: got %v, want money.ErrOverflow", err)
	}
	endpoints.Settings = zoneLost{endpoints.Settings}
	if err := read(); err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
		t.Errorf("zone lost: got %v, want the company's zone refused", err)
	}
	if _, err := (payments.CashPosition{}).Actuals(context.Background(), e.le, day, day); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("statement lines outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
}

// TestCashActualLinesTwoPayments: a line matched to two payments of two
// suppliers names each payment's supplier, and its amount is shared out
// over what they settled; without partner.supplier.read the payments are
// listed without their suppliers.
func TestCashActualLinesTwoPayments(t *testing.T) {
	e := actualsEnv{forecastEnv: setupForecast(t, withActuals, actualPerms...), docs: map[string]uuid.UUID{}}
	a, b := e.NewSupplier(t, "BP90000061"), e.NewSupplier(t, "BP90000062")
	rent := e.document(t, a, "2026-04-01", -30000, docLine{"530000", 30000, "", ""})
	equipment := e.document(t, b, "2026-04-01", -10000, docLine{"510000", 10000, "", ""})
	e.docs["rent payment"] = e.paid(t, a, 30000, map[uuid.UUID]int64{rent: 30000})
	e.docs["equipment payment"] = e.paid(t, b, 10000, map[uuid.UUID]int64{equipment: 10000})
	e.cadLines = e.statement(t, e.main, "CAD", "2026-04-14", "2026-04-14", 0, -40000, nil, "imported")
	e.actualLine(t, e.cadLines, 1, "2026-04-14", -40000, "posted", "matched", target{payments: []uuid.UUID{e.docs["rent payment"], e.docs["equipment payment"]}})
	_, lines := e.actualLinesOf(t, "&currency=CAD", 50)
	want(t, "two payments", lines, []string{"2026-04-14 MAIN #1(true,B) posted payment unclassified=false -400.00 CAD share -400.00 [OP_RENT -300.00 CAPEX -100.00];" +
		" [rent payment true -300.00 to BP90000061 equipment payment true -100.00 to BP90000062] of 2; [BP90000061 BP90000062]"})
	e.Tenant.Revoke(t, "partner.supplier.read", e.Tenant.Human)
	_, lines = e.actualLinesOf(t, "&currency=CAD", 50)
	want(t, "without suppliers", lines, []string{"2026-04-14 MAIN #1(true,B) posted payment unclassified=false -400.00 CAD share -400.00 [OP_RENT -300.00 CAPEX -100.00];" +
		" [rent payment true -300.00 to - equipment payment true -100.00 to -] of 2; []"})
}
