package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Account assignment on the journal line (T56d, ADR-0050 §5): what
// postings derive and refuse.

// assigned is a tenant whose company 2000 keeps ledgers 0L and 2L and has
// cost and profit center accounting on from 2026, activated on 2026-09-01:
// segments SEG1 and SEG2; profit centers DUMMY (the default), PC1 (SEG1),
// PCOLD (valid until 2026-04-01) and PCUS (company 1000 only); cost centers
// 1000 (ADMIN, PC1, the default of 520000), 2000 (SALES, no profit center,
// locked for costs and revenue in February), 3000 (ADMIN, from 2026-06-01),
// 4000 (ADMIN, from 2026-08-01, the default of 540000), PEND (pending) and
// US (company 1000). PC1 is locked in March.
type assigned struct {
	centers
	seg1, seg2, dummy, pc1, pcOld, pcUS             uuid.UUID
	cc1000, cc2000, cc3000, cc4000, ccPending, ccUS uuid.UUID
	office, it, revenue, freight, bank, ap          uuid.UUID // accounts 520000, 530000, 400000, 540000, 100000, 211000 of 2000
}

func setupAssigned(t *testing.T) assigned {
	t.Helper()
	a := assigned{centers: setupCenters(t)}
	c := a.centers
	id := func(path string) uuid.UUID { return uuid.MustParse(path[len("/v1/profit-centers/"):]) }
	a.seg1, a.seg2 = uuid.MustParse(c.segment(t, "SEG1")), uuid.MustParse(c.segment(t, "SEG2"))
	a.dummy = id(c.profitCenter(t, "DUMMY", "", c.de))
	pc, _ := c.Must(201, "POST", "/v1/profit-centers", c.pcBody(fmt.Sprintf(`{"code": "PC1", "name": "PC 1", "responsible": "Jane Doe",
		"segment_id": %q, "legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, a.seg1, c.de)), "")
	c.decide(t, pc["pending_change_id"], "approve")
	a.pc1 = uuid.MustParse(pc["id"].(string))
	a.pcOld, a.pcUS = id(c.profitCenter(t, "PCOLD", "2026-04-01", c.de)), id(c.profitCenter(t, "PCUS", "", c.us))
	cc := func(le uuid.UUID, code, category, from, extra string, approve bool) uuid.UUID {
		got, _ := c.Must(201, "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "CC", "responsible": "J",
			"category": %q, "valid_from": %q%s, "reason": "r"}`, le, code, category, from, extra)), "")
		if approve {
			c.decide(t, got["pending_change_id"], "approve")
		}
		return uuid.MustParse(got["id"].(string))
	}
	a.cc1000 = cc(c.de, "1000", "ADMIN", "2026-01-01", fmt.Sprintf(`, "profit_center_id": %q`, a.pc1), true)
	a.cc2000 = cc(c.de, "2000", "SALES", "2026-01-01", "", true)
	a.cc3000, a.cc4000 = cc(c.de, "3000", "ADMIN", "2026-06-01", "", true), cc(c.de, "4000", "ADMIN", "2026-08-01", "", true)
	a.ccPending, a.ccUS = cc(c.de, "PEND", "ADMIN", "2026-01-01", "", false), cc(c.us, "US", "ADMIN", "2026-01-01", "", true)
	a.lock(t, "/v1/cost-centers/"+a.cc2000.String(), `{"valid_from": "2026-02-01", "valid_to": "2026-03-01", "lock_actual_cost": true,
		"lock_actual_revenue": true, "reason": "r"}`)
	a.lock(t, "/v1/profit-centers/"+a.pc1.String(), `{"valid_from": "2026-03-01", "valid_to": "2026-04-01", "locked": true, "reason": "r"}`)
	doc := parallel(t)
	co := doc.Company("2000")
	co.CostCenterAccounting, co.ProfitCenterAccounting, co.DefaultProfitCenter = true, true, "DUMMY"
	co.DefaultAssignments = []config.DefaultAssignment{{Account: "520000", CostCenter: "1000"}, {Account: "540000", CostCenter: "4000"}}
	if err := c.apply(t, "2026-09-01", doc); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period <= 12
		AND status = 'closed'`, c.ten.ID)
	acct := func(n string) uuid.UUID { return uuid.MustParse(c.account(t, "2000", n)) }
	a.office, a.it, a.revenue, a.freight, a.bank, a.ap = acct("520000"), acct("530000"), acct("400000"), acct("540000"), acct("100000"), acct("211000")
	return a
}

func (a assigned) in(t *testing.T) assigned {
	t.Helper()
	a.centers = a.centers.in(t)
	return a
}

// lock has the controller approve a lock of the center at path.
func (a assigned) lock(t *testing.T, path, body string) {
	t.Helper()
	_, tag := a.Must(200, "GET", path, "", "")
	ch, _ := a.Must(200, "POST", path+":lock", body, tag)
	a.decide(t, ch["id"], "approve")
}

// post posts the lines in company 2000 on the date through Postings, as a
// context does, and returns the leading ledger's entry.
func (a assigned) post(t *testing.T, day string, lines ...ledger.PostingLine) (ledger.Entry, error) {
	t.Helper()
	var posted ledger.Entry
	err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
		var err error
		posted, err = postings().Post(ctx, ledger.Posting{LegalEntityID: a.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day),
			Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: lines})
		return err
	})
	return posted, err
}

// centered is a line on the account with the centers given (nil: none).
func centered(account uuid.UUID, amount money.Minor, cc, pc, seg *uuid.UUID) ledger.PostingLine {
	return ledger.PostingLine{AccountID: account, Amount: amount, CostCenterID: cc, ProfitCenterID: pc, SegmentID: seg}
}

// assignmentOf is a line's centers as "cc/pc/segment", "-" for none.
func assignmentOf(l ledger.Line) string {
	s := func(id *uuid.UUID) string {
		if id == nil {
			return "-"
		}
		return id.String()
	}
	return s(l.CostCenterID) + "/" + s(l.ProfitCenterID) + "/" + s(l.SegmentID)
}

func wantAssigned(cc, pc, seg *uuid.UUID) string {
	return assignmentOf(ledger.Line{CostCenterID: cc, ProfitCenterID: pc, SegmentID: seg})
}

// TestAssignmentDerived: a posting's lines get the cost center entered or
// the account's default, the cost center's profit center or the company's
// default for a P&L line, and the profit center's segment; every ledger's
// copy carries the same, and a reversal its original's.
func TestAssignmentDerived(t *testing.T) {
	a := setupAssigned(t)
	ap := func(amount money.Minor) ledger.PostingLine {
		return ledger.PostingLine{AccountID: a.ap, Amount: amount, PartnerID: &a.supplier}
	}
	for name, c := range map[string]struct {
		day   string
		lines []ledger.PostingLine
		want  []string
	}{
		"default cost center": {"2026-05-10", []ledger.PostingLine{centered(a.office, 100, nil, nil, nil), ap(-100)},
			[]string{wantAssigned(&a.cc1000, &a.pc1, &a.seg1), wantAssigned(nil, nil, nil)}},
		"entered cost center, default profit center": {"2026-05-10", []ledger.PostingLine{centered(a.it, 100, &a.cc2000, nil, nil), ap(-100)},
			[]string{wantAssigned(&a.cc2000, &a.dummy, nil), wantAssigned(nil, nil, nil)}},
		"revenue on a category that allows it": {"2026-05-10", []ledger.PostingLine{centered(a.revenue, -100, &a.cc2000, nil, nil), centered(a.bank, 100, nil, nil, nil)},
			[]string{wantAssigned(&a.cc2000, &a.dummy, nil), wantAssigned(nil, nil, nil)}},
		"profit center entered on a balance sheet line": {"2026-05-10", []ledger.PostingLine{centered(a.bank, 100, nil, &a.pc1, nil), centered(a.revenue, -100, nil, nil, nil)},
			[]string{wantAssigned(nil, &a.pc1, &a.seg1), wantAssigned(nil, &a.dummy, nil)}},
		"the cost center's profit center and segment entered": {"2026-05-10", []ledger.PostingLine{centered(a.it, 100, &a.cc1000, &a.pc1, &a.seg1), ap(-100)},
			[]string{wantAssigned(&a.cc1000, &a.pc1, &a.seg1), wantAssigned(nil, nil, nil)}},
		"a default before its validity (a past period end)": {"2026-07-31", []ledger.PostingLine{centered(a.freight, 100, nil, nil, nil), ap(-100)},
			[]string{wantAssigned(&a.cc4000, &a.dummy, nil), wantAssigned(nil, nil, nil)}},
	} {
		t.Run(name, func(t *testing.T) {
			posted, err := a.in(t).post(t, c.day, c.lines...)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			for i, l := range posted.Lines {
				if got := assignmentOf(l); got != c.want[i] {
					t.Errorf("line %d: got %s, want %s", i+1, got, c.want[i])
				}
			}
			var copies int
			pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_line o JOIN ledger.journal_entry e ON e.id = o.journal_entry_id
				JOIN ledger.journal_entry c ON c.posting_group_id = e.posting_group_id AND c.id <> e.id
				JOIN ledger.journal_line l ON l.journal_entry_id = c.id AND l.line_no = o.line_no
				WHERE o.journal_entry_id = $1 AND (l.cost_center_id, l.profit_center_id, l.segment_id)
					IS NOT DISTINCT FROM (o.cost_center_id, o.profit_center_id, o.segment_id)`, []any{posted.ID}, &copies)
			if copies != len(c.lines) {
				t.Errorf("lines of the 2L copy with the same assignment: got %d, want %d", copies, len(c.lines))
			}
		})
	}
	t.Run("a reversal copies the assignment after a lock", func(t *testing.T) {
		posted, err := a.in(t).post(t, "2026-05-10", centered(a.it, 100, &a.cc2000, nil, nil), ap(-100))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		a.in(t).lock(t, "/v1/cost-centers/"+a.cc2000.String(), `{"valid_from": "2026-05-11", "valid_to": "2026-06-01", "lock_actual_cost": true, "reason": "r"}`)
		var rev ledger.Entry
		if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
			var err error
			rev, err = postings().Reverse(ctx, posted.ID, new(date("2026-05-20")), nil)
			return err
		}); err != nil {
			t.Fatalf("reverse: %v", err)
		}
		if got, want := assignmentOf(rev.Lines[0]), wantAssigned(&a.cc2000, &a.dummy, nil); got != want {
			t.Errorf("reversal line 1: got %s, want %s", got, want)
		}
	})
}

// TestAssignmentRefused: a posting is refused for a missing cost center,
// a center it cannot carry, or a profit center or segment that is not the
// one derived.
func TestAssignmentRefused(t *testing.T) {
	a := setupAssigned(t)
	other := uuid.New()
	for name, c := range map[string]struct {
		day        string
		line       ledger.PostingLine
		code, why  string
		otherDebit bool // the line is a credit: the bank takes the debit
	}{
		"a primary cost line without a default":        {"2026-05-10", centered(a.it, 100, nil, nil, nil), "ledger.cost_center_required", "required", false},
		"a cost center on a balance sheet line":        {"2026-05-10", centered(a.bank, -100, &a.cc1000, nil, nil), "ledger.cost_center_invalid", "not_allowed", true},
		"an unknown cost center":                       {"2026-05-10", centered(a.it, 100, &other, nil, nil), "ledger.cost_center_invalid", "unknown", false},
		"another company's cost center":                {"2026-05-10", centered(a.it, 100, &a.ccUS, nil, nil), "ledger.cost_center_invalid", "not_in_company", false},
		"a pending cost center":                        {"2026-05-10", centered(a.it, 100, &a.ccPending, nil, nil), "ledger.cost_center_invalid", "not_active", false},
		"a cost center before its validity":            {"2026-05-10", centered(a.it, 100, &a.cc3000, nil, nil), "ledger.cost_center_invalid", "not_valid_on_date", false},
		"a default entered before its validity":        {"2026-07-31", centered(a.freight, 100, &a.cc4000, nil, nil), "ledger.cost_center_invalid", "not_valid_on_date", false},
		"a cost center locked for costs":               {"2026-02-10", centered(a.it, 100, &a.cc2000, nil, nil), "ledger.cost_center_invalid", "locked_for_costs", false},
		"a cost center locked for revenue":             {"2026-02-10", centered(a.revenue, -100, &a.cc2000, nil, nil), "ledger.cost_center_invalid", "locked_for_revenue", true},
		"revenue on a category that takes none":        {"2026-05-10", centered(a.revenue, -100, &a.cc1000, nil, nil), "ledger.cost_center_invalid", "revenue_not_allowed", true},
		"another profit center than the cost center's": {"2026-05-10", centered(a.it, 100, &a.cc1000, &a.dummy, nil), "ledger.profit_center_mismatch", "mismatch", false},
		"an unknown profit center":                     {"2026-05-10", centered(a.revenue, -100, nil, &other, nil), "ledger.profit_center_invalid", "unknown", true},
		"a profit center of another company":           {"2026-05-10", centered(a.revenue, -100, nil, &a.pcUS, nil), "ledger.profit_center_invalid", "not_in_company", true},
		"a profit center after its validity":           {"2026-05-10", centered(a.revenue, -100, nil, &a.pcOld, nil), "ledger.profit_center_invalid", "not_valid_on_date", true},
		"a derived profit center that is locked":       {"2026-03-10", centered(a.office, 100, nil, nil, nil), "ledger.profit_center_invalid", "locked", false},
		"a segment without a profit center":            {"2026-05-10", centered(a.bank, -100, nil, nil, &a.seg1), "ledger.segment_mismatch", "mismatch", true},
		"another segment than the profit center's":     {"2026-05-10", centered(a.revenue, -100, nil, &a.pc1, &a.seg2), "ledger.segment_mismatch", "mismatch", true},
	} {
		t.Run(name, func(t *testing.T) {
			offset := ledger.PostingLine{AccountID: a.ap, Amount: -c.line.Amount, PartnerID: &a.supplier}
			if c.otherDebit {
				offset = ledger.PostingLine{AccountID: a.bank, Amount: -c.line.Amount}
				if c.line.AccountID == a.bank {
					offset = ledger.PostingLine{AccountID: a.ap, Amount: -c.line.Amount, PartnerID: &a.supplier}
				}
			}
			_, err := a.in(t).post(t, c.day, c.line, offset)
			p := problem.From(err)
			if p.Code != c.code {
				t.Fatalf("post: got %v, want %s", err, c.code)
			}
			if len(p.Errors) != 1 || p.Errors[0].Code != c.why || !strings.HasPrefix(p.Errors[0].Field, "lines[0].") {
				t.Errorf("field errors: got %+v, want one on lines[0] with code %s", p.Errors, c.why)
			}
		})
	}
	t.Run("parking", func(t *testing.T) {
		a := a.in(t)
		lines := func(account uuid.UUID) []line {
			return []line{{account.String(), "1.00", ""}, {a.ap.String(), "-1.00", a.supplier.String()}}
		}
		a.Problem(422, "ledger.cost_center_required", "POST", "/v1/parked-journal-entries", entry(a.de, "2026-05-10", "EUR", lines(a.it)...), "")
		// The default's profit center PC1 is locked in March.
		a.Problem(422, "ledger.profit_center_invalid", "POST", "/v1/parked-journal-entries", entry(a.de, "2026-03-10", "EUR", lines(a.office)...), "")
		a.Must(201, "POST", "/v1/parked-journal-entries", entry(a.de, "2026-05-10", "EUR", lines(a.office)...), "")
		// No fiscal year: refused when it is posted, not parked.
		a.Must(201, "POST", "/v1/parked-journal-entries", entry(a.de, "2041-05-10", "EUR", lines(a.office)...), "")
	})
}

// TestAssignmentConstraints: the database's refusals of a center of
// another company, for a writer that skipped the ledger's checks, map to
// the ledger's problems.
func TestAssignmentConstraints(t *testing.T) {
	for constraint, code := range map[string]string{
		"journal_line_cost_center_fkey":   "ledger.cost_center_invalid",
		"journal_line_profit_center_fkey": "ledger.profit_center_invalid",
		"journal_line_segment_fkey":       "ledger.segment_mismatch",
	} {
		t.Run(constraint, func(t *testing.T) {
			if got := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint}); got.Code != code || got.Status != 422 {
				t.Errorf("got %s (%d), want %s (422)", got.Code, got.Status, code)
			}
		})
	}
}

// TestAssignmentDatabaseFailures fails each statement of a posting that
// reads every rule and center in turn, and of parking one.
func TestAssignmentDatabaseFailures(t *testing.T) {
	a := setupAssigned(t)
	h := endpoints()
	apitest.EveryFailure(t, a.ten, a.ten.Human, "park with an assignment", func(ctx context.Context) error {
		_, err := h.ParkJournalEntry(ctx, &gen.JournalEntryCreate{LegalEntityID: a.de, DocumentDate: date("2026-05-10"), PostingDate: date("2026-05-10"),
			Currency: "EUR", Lines: []gen.JournalLineInput{
				{AccountID: a.office, Amount: gen.Money{Amount: "1.00", Currency: "EUR"}},
				{AccountID: a.bank, Amount: gen.Money{Amount: "-1.00", Currency: "EUR"}}}}, gen.ParkJournalEntryParams{})
		return err
	})
	apitest.EveryFailure(t, a.ten, a.ten.Human, "post with an assignment", func(ctx context.Context) error {
		_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: a.de, DocumentType: "SA", DocumentDate: date("2026-05-10"), PostingDate: date("2026-05-10"),
			Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				centered(a.office, 100, nil, nil, nil), centered(a.revenue, -300, &a.cc2000, nil, nil), centered(a.bank, 200, nil, &a.pc1, nil)}})
		return err
	})
}

// TestAssignmentAndCenterLockSerialize: a posting and the approval of a
// lock of its cost center queue on the center: a posting waits for a lock
// being approved and is then refused for it, and the approval waits for a
// posting under way and is then refused for its line (T56d2), so no line
// lands on a center whose lock committed first and no lock covers a line
// that committed first.
func TestAssignmentAndCenterLockSerialize(t *testing.T) {
	a := setupAssigned(t)
	path := "/v1/cost-centers/" + a.cc2000.String()
	next := map[string]string{"10": "11", "11": "12"}
	request := func(t *testing.T, month string) func(ctx context.Context) error {
		t.Helper()
		_, tag := a.in(t).Must(200, "GET", path, "", "")
		// A month's lock: the second subtest posts after the first's.
		ch, _ := a.in(t).Must(200, "POST", path+":lock", fmt.Sprintf(`{"valid_from": "2026-%s-01", "valid_to": "2026-%s-01", "lock_actual_cost": true,
			"reason": "r"}`, month, next[month]), tag)
		return func(ctx context.Context) error {
			return ledger.CenterChanges{}.Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
		}
	}
	post := func(day string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: a.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day),
				Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					centered(a.it, 100, &a.cc2000, nil, nil), {AccountID: a.ap, Amount: -100, PartnerID: &a.supplier}}})
			return err
		}
	}
	// The lock first: the refused approval of the second leaves its change
	// pending.
	t.Run("lock first", func(t *testing.T) {
		first, second := serializeOn(t, a.ten, "the cost center", rowLockWaits(t), request(t, "10"), post("2026-10-10"))
		if first != nil {
			t.Fatalf("lock: got %v, want it applied", first)
		}
		if p := problem.From(second); p.Code != "ledger.cost_center_invalid" || len(p.Errors) != 1 || p.Errors[0].Code != "locked_for_costs" {
			t.Errorf("posting after the lock: got %v, want ledger.cost_center_invalid, locked_for_costs", second)
		}
	})
	t.Run("posting first", func(t *testing.T) {
		first, second := serializeOn(t, a.ten, "the cost center", rowLockWaits(t), post("2026-11-10"), request(t, "11"))
		if first != nil {
			t.Fatalf("posting: got %v, want it committed", first)
		}
		if got := problem.From(second).Code; got != "ledger.cost_center_period_in_use" {
			t.Errorf("lock after the posting: got %v, want ledger.cost_center_period_in_use once the posting committed", second)
		}
	})
}
