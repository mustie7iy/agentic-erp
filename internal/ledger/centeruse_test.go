package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The in-use refusals of center changes (T56d2, ADR-0050 §3): a change
// may not contradict what posted lines read.

// inUse is setupAssigned with lines posted on cost center 2000: a revenue
// line on 2026-05-09 and a cost line on 2026-05-10 (both entered); on 1000
// with PC1 (the default of 520000) on 2026-05-11; on 4000, the default of
// 540000, on 2026-07-31, before its validity; and a revenue line on PCOLD
// (entered, no cost center) on 2026-03-10.
func inUse(t *testing.T) assigned {
	t.Helper()
	a := setupAssigned(t)
	ap := ledger.PostingLine{AccountID: a.ap, Amount: -100, PartnerID: &a.supplier}
	bank := centered(a.bank, 100, nil, nil, nil)
	for day, l := range map[string][]ledger.PostingLine{"2026-05-09": {centered(a.revenue, -100, &a.cc2000, nil, nil), bank},
		"2026-05-10": {centered(a.it, 100, &a.cc2000, nil, nil), ap}, "2026-05-11": {centered(a.office, 100, nil, nil, nil), ap},
		"2026-07-31": {centered(a.freight, 100, nil, nil, nil), ap}, "2026-03-10": {centered(a.revenue, -100, nil, &a.pcOld, nil), bank}} {
		if _, err := a.post(t, day, l...); err != nil {
			t.Fatalf("post on %s: %v", day, err)
		}
	}
	return a
}

// requestChange requests a change of the center at path (op: lock, move
// or redate) and returns the problem's code, "" when it was accepted; an
// accepted change is rejected again, so the next one can be requested.
func (a assigned) requestChange(t *testing.T, path, op, body string) string {
	t.Helper()
	_, tag := a.Must(200, "GET", path, "", "")
	status, got, _ := a.Do("POST", path+":"+op, body, tag)
	if status == 200 {
		a.decide(t, got["id"], "reject")
		return ""
	}
	if status != 409 && status != 422 {
		t.Fatalf("%s %s: got status %d (%v), want 200, 409 or 422", op, path, status, got)
	}
	return fmt.Sprint(got["code"])
}

func TestCenterChangesInUse(t *testing.T) {
	a := inUse(t)
	cc2000, cc4000 := "/v1/cost-centers/"+a.cc2000.String(), "/v1/cost-centers/"+a.cc4000.String()
	pc1, pcOld := "/v1/profit-centers/"+a.pc1.String(), "/v1/profit-centers/"+a.pcOld.String()
	for name, c := range map[string]struct {
		path, op, body, want string
	}{
		"a cost lock on a posted date": {cc2000, "lock", `{"valid_from": "2026-05-01", "valid_to": "2026-06-01", "lock_actual_cost": true, "reason": "r"}`,
			"ledger.cost_center_period_in_use"},
		"a cost lock on a date with only a revenue line": {cc2000, "lock",
			`{"valid_from": "2026-05-09", "valid_to": "2026-05-10", "lock_actual_cost": true, "reason": "r"}`, ""},
		"a revenue lock on a date with a revenue line": {cc2000, "lock",
			`{"valid_from": "2026-04-01", "valid_to": "2026-05-10", "lock_actual_revenue": true, "reason": "r"}`, "ledger.cost_center_period_in_use"},
		"a revenue lock on a date with only a cost line": {cc2000, "lock", `{"valid_from": "2026-05-10", "lock_actual_revenue": true, "reason": "r"}`, ""},
		"both locks on a date with only a revenue line": {cc2000, "lock",
			`{"valid_from": "2026-05-09", "valid_to": "2026-05-10", "lock_actual_cost": true, "lock_actual_revenue": true, "reason": "r"}`,
			"ledger.cost_center_period_in_use"},
		"a plan lock on a posted date":    {cc2000, "lock", `{"valid_from": "2026-05-01", "lock_plan": true, "reason": "r"}`, ""},
		"a lock after the last posting":   {cc2000, "lock", `{"valid_from": "2026-05-11", "lock_actual_cost": true, "reason": "r"}`, ""},
		"a lock before the first posting": {cc2000, "lock", `{"valid_from": "2026-04-01", "valid_to": "2026-05-09", "lock_actual_cost": true, "reason": "r"}`, ""},
		"a move on a posted date": {cc2000, "move", `{"valid_from": "2026-05-01", "category": "ADMIN", "reason": "r"}`,
			"ledger.cost_center_period_in_use"},
		"a move to a profit center on a posted date": {cc2000, "move", fmt.Sprintf(`{"valid_from": "2026-05-10", "profit_center_id": %q, "reason": "r"}`, a.pc1),
			"ledger.cost_center_period_in_use"},
		"a move after the last posting": {cc2000, "move", `{"valid_from": "2026-05-11", "category": "ADMIN", "reason": "r"}`, ""},
		"an end on the posting date":    {cc2000, "redate", `{"valid_to": "2026-05-10", "reason": "r"}`, "ledger.cost_center_posted_after"},
		"an end after the last posting": {cc2000, "redate", `{"valid_to": "2026-05-11", "reason": "r"}`, ""},
		"a start after a posting":       {cc2000, "redate", `{"valid_from": "2026-05-11", "reason": "r"}`, "ledger.cost_center_period_in_use"},
		"an earlier start":              {cc2000, "redate", `{"valid_from": "2025-12-01", "reason": "r"}`, ""},
		// The cost line of 2026-07-31 took 4000's first period, from 2026-08-01.
		"a lock of the first period a default's posting before it took": {cc4000, "lock",
			`{"valid_from": "2026-08-01", "valid_to": "2026-09-01", "lock_actual_cost": true, "reason": "r"}`, "ledger.cost_center_period_in_use"},
		"a revenue lock of the first period a default's cost line before it took": {cc4000, "lock",
			`{"valid_from": "2026-08-01", "valid_to": "2026-09-01", "lock_actual_revenue": true, "reason": "r"}`, ""},
		"a lock after the first day of a default's first period": {cc4000, "lock",
			`{"valid_from": "2026-08-02", "valid_to": "2026-09-01", "lock_actual_cost": true, "reason": "r"}`, ""},
		"a segment move of a posted profit center": {pc1, "move", fmt.Sprintf(`{"segment_id": %q, "reason": "r"}`, a.seg2), "ledger.segment_in_use"},
		"a segment move of a profit center without lines": {"/v1/profit-centers/" + a.pcUS.String(), "move",
			fmt.Sprintf(`{"segment_id": %q, "reason": "r"}`, a.seg2), ""},
		// T56d4: a profit center's lock is read by every line carrying it.
		"a profit center lock on a posted date": {pc1, "lock", `{"valid_from": "2026-05-01", "valid_to": "2026-06-01", "locked": true, "reason": "r"}`,
			"ledger.profit_center_period_in_use"},
		"a profit center lock on a revenue line": {pcOld, "lock", `{"valid_from": "2026-03-10", "locked": true, "reason": "r"}`,
			"ledger.profit_center_period_in_use"},
		"a profit center lock after the last posting": {pc1, "lock", `{"valid_from": "2026-05-12", "valid_to": "2026-06-01", "locked": true, "reason": "r"}`, ""},
		"a profit center end on the posting date":     {pcOld, "redate", `{"valid_to": "2026-03-10", "reason": "r"}`, "ledger.profit_center_posted_after"},
		"a profit center end after the last posting":  {pcOld, "redate", `{"valid_to": "2026-03-11", "reason": "r"}`, ""},
		"a profit center start after a posting":       {pcOld, "redate", `{"valid_from": "2026-03-11", "reason": "r"}`, "ledger.profit_center_period_in_use"},
		"a profit center's earlier start":             {pcOld, "redate", `{"valid_from": "2025-12-01", "reason": "r"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := a.in(t).requestChange(t, c.path, c.op, c.body); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCenterChangesInUseAtApproval: a change requested before a posting
// it contradicts is refused when it is approved.
func TestCenterChangesInUseAtApproval(t *testing.T) {
	a := setupAssigned(t)
	for name, c := range map[string]struct {
		path, body, day string
		line            ledger.PostingLine
		want            string
	}{
		"a cost center lock": {"/v1/cost-centers/" + a.cc3000.String(), `{"valid_from": "2026-06-01", "valid_to": "2026-07-01", "lock_actual_cost": true, "reason": "r"}`,
			"2026-06-10", centered(a.it, 100, &a.cc3000, nil, nil), "ledger.cost_center_period_in_use"},
		"a profit center lock": {"/v1/profit-centers/" + a.pcOld.String(), `{"valid_from": "2026-02-01", "valid_to": "2026-03-01", "locked": true, "reason": "r"}`,
			"2026-02-10", centered(a.revenue, -100, nil, &a.pcOld, nil), "ledger.profit_center_period_in_use"},
	} {
		t.Run(name, func(t *testing.T) {
			a := a.in(t)
			_, tag := a.Must(200, "GET", c.path, "", "")
			ch, _ := a.Must(200, "POST", c.path+":lock", c.body, tag)
			if _, err := a.post(t, c.day, c.line, ledger.PostingLine{AccountID: a.bank, Amount: -c.line.Amount}); err != nil {
				t.Fatalf("post: %v", err)
			}
			err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
				return ledger.CenterChanges{}.Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
			})
			if got := problem.From(err).Code; got != c.want {
				t.Errorf("approval: got %v, want %s", err, c.want)
			}
		})
	}
}

// TestCenterChangesInUseOfReversals: a reversal dated after a center's
// new end counts as a posting on it (product owner 2026-10-25), for a cost
// center and for a profit center alike.
func TestCenterChangesInUseOfReversals(t *testing.T) {
	a := setupAssigned(t)
	for name, c := range map[string]struct {
		path, posted, reversed, end, want string
		line                              ledger.PostingLine
	}{
		"a cost center": {"/v1/cost-centers/" + a.cc3000.String(), "2026-06-10", "2026-07-15", "2026-07-01", "ledger.cost_center_posted_after",
			centered(a.it, 100, &a.cc3000, nil, nil)},
		"a profit center": {"/v1/profit-centers/" + a.pcOld.String(), "2026-03-02", "2026-03-20", "2026-03-15", "ledger.profit_center_posted_after",
			centered(a.revenue, -100, nil, &a.pcOld, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			a := a.in(t)
			posted, err := a.post(t, c.posted, c.line, ledger.PostingLine{AccountID: a.bank, Amount: -c.line.Amount})
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
				_, err := postings().Reverse(ctx, posted.ID, new(date(c.reversed)), nil)
				return err
			}); err != nil {
				t.Fatalf("reverse: %v", err)
			}
			if got := a.requestChange(t, c.path, "redate", fmt.Sprintf(`{"valid_to": %q, "reason": "r"}`, c.end)); got != c.want {
				t.Errorf("an end between the posting and its reversal: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCenterInUseDatabaseFailures fails each statement of changes whose
// in-use checks read posted lines.
func TestCenterInUseDatabaseFailures(t *testing.T) {
	a := inUse(t)
	h := endpoints()
	apitest.EveryFailure(t, a.ten, a.ten.Human, "lock a posted cost center", func(ctx context.Context) error {
		_, err := h.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: date("2026-05-11"), LockActualCost: gen.NewOptBool(true), Reason: "r"},
			gen.LockCostCenterParams{ID: a.cc2000, IfMatch: "*"})
		return err
	})
	apitest.EveryFailure(t, a.ten, a.ten.Human, "end a posted cost center", func(ctx context.Context) error {
		_, err := h.RedateCostCenter(ctx, &gen.CenterRedate{ValidTo: gen.NewOptNilDate(date("2026-12-01")), Reason: "r"},
			gen.RedateCostCenterParams{ID: a.cc2000, IfMatch: "*"})
		return err
	})
	apitest.EveryFailure(t, a.ten, a.ten.Human, "move a profit center's segment", func(ctx context.Context) error {
		_, err := h.MoveProfitCenter(ctx, &gen.ProfitCenterMove{SegmentID: gen.NewNilUUID(a.seg2), Reason: "r"},
			gen.MoveProfitCenterParams{ID: a.pcUS, IfMatch: "*"})
		return err
	})
	apitest.EveryFailure(t, a.ten, a.ten.Human, "lock a posted profit center", func(ctx context.Context) error {
		_, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: date("2026-03-11"), Locked: true, Reason: "r"},
			gen.LockProfitCenterParams{ID: a.pcOld, IfMatch: "*"})
		return err
	})
	apitest.EveryFailure(t, a.ten, a.ten.Human, "end a posted profit center", func(ctx context.Context) error {
		_, err := h.RedateProfitCenter(ctx, &gen.CenterRedate{ValidTo: gen.NewOptNilDate(date("2026-03-20")), Reason: "r"},
			gen.RedateProfitCenterParams{ID: a.pcOld, IfMatch: "*"})
		return err
	})
}

// TestAssignmentAndProfitCenterLockSerialize: a posting and the approval
// of a lock of its profit center queue on the profit center, as for a cost
// center's (TestAssignmentAndCenterLockSerialize): a posting waits for a
// lock being approved and is then refused for it, and the approval waits
// for a posting under way and is then refused for its line (T56d4).
func TestAssignmentAndProfitCenterLockSerialize(t *testing.T) {
	a := setupAssigned(t)
	path := "/v1/profit-centers/" + a.pc1.String()
	var pending any // the change last requested
	request := func(t *testing.T, from, to string) func(ctx context.Context) error {
		t.Helper()
		_, tag := a.in(t).Must(200, "GET", path, "", "")
		ch, _ := a.in(t).Must(200, "POST", path+":lock", fmt.Sprintf(`{"valid_from": %q, "valid_to": %q, "locked": true, "reason": "r"}`, from, to), tag)
		pending = ch["id"]
		return func(ctx context.Context) error {
			return ledger.CenterChanges{}.Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
		}
	}
	post := func(day string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: a.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day),
				Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					centered(a.revenue, -100, nil, &a.pc1, nil), centered(a.bank, 100, nil, nil, nil)}})
			return err
		}
	}
	t.Run("posting first", func(t *testing.T) {
		first, second := serializeOn(t, a.ten, "the profit center", rowLockWaits(t), post("2026-11-10"), request(t, "2026-11-01", "2026-12-01"))
		if first != nil {
			t.Fatalf("posting: got %v, want it committed", first)
		}
		if got := problem.From(second).Code; got != "ledger.profit_center_period_in_use" {
			t.Errorf("lock over the posting: got %v, want ledger.profit_center_period_in_use once the posting committed", second)
		}
		a.in(t).decide(t, pending, "reject") // the refused approval left the change pending
	})
	t.Run("lock first", func(t *testing.T) {
		first, second := serializeOn(t, a.ten, "the profit center", rowLockWaits(t), request(t, "2026-10-01", "2026-11-01"), post("2026-10-10"))
		if first != nil {
			t.Fatalf("lock: got %v, want it applied", first)
		}
		if p := problem.From(second); p.Code != "ledger.profit_center_invalid" || len(p.Errors) != 1 || p.Errors[0].Code != "locked" {
			t.Errorf("posting after the lock: got %v, want ledger.profit_center_invalid, locked", second)
		}
	})
}
