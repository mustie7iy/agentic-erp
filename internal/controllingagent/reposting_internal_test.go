package controllingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// march is a line of company C's March 2026 in the leading ledger: 120.00
// EUR on cost center CC1.
func march() (entry, line) {
	l := line{LineNo: 1, AccountID: "A", CostCenterID: "CC1", ProfitCenterID: "PC1"}
	l.AmountLC.Amount, l.AmountLC.Currency = "120.00", "EUR"
	return entry{ID: "E", LegalEntityID: "C", LedgerID: "0L", Leading: true, FiscalYear: 2026, FiscalPeriod: 3, DocumentType: "SA",
		DocumentNumber: "100001", PostingDate: "2026-03-10", LocalCurrency: "EUR", Lines: []line{l}}, l
}

// TestSource: a line of another company, year, period or ledger, of a
// reversed or reversing entry, a line the entry has not, one without a
// center, on an internal order or in another currency is not reposted.
func TestSource(t *testing.T) {
	r := &run{company: "C", period: Period{Company: "2000", Year: 2026, Period: 3}}
	for name, c := range map[string]struct {
		change func(*entry)
		line   int
		want   error
	}{
		"the run's":         {func(*entry) {}, 1, nil},
		"another company":   {func(e *entry) { e.LegalEntityID = "D" }, 1, ErrSource},
		"another year":      {func(e *entry) { e.FiscalYear = 2025 }, 1, ErrSource},
		"another period":    {func(e *entry) { e.FiscalPeriod = 4 }, 1, ErrSource},
		"another ledger":    {func(e *entry) { e.Leading = false }, 1, ErrSource},
		"a reversal":        {func(e *entry) { e.ReversalOfID = "R" }, 1, ErrSource},
		"reversed":          {func(e *entry) { e.ReversedByID = "R" }, 1, ErrSource},
		"no such line":      {func(*entry) {}, 2, ErrSource},
		"no center":         {func(e *entry) { e.Lines[0].CostCenterID, e.Lines[0].ProfitCenterID = "", "" }, 1, ErrSource},
		"an internal order": {func(e *entry) { e.Lines[0].InternalOrderID = "O" }, 1, ErrSource},
		"another currency":  {func(e *entry) { e.Lines[0].AmountLC.Currency = "USD" }, 1, ErrSource},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := march()
			c.change(&e)
			if _, err := r.source(e, c.line); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

// TestTarget: a line with a cost center moves to another cost center; a
// line with a profit center alone to another profit center or a cost
// center.
func TestTarget(t *testing.T) {
	_, withCost := march()
	withProfit := withCost
	withProfit.CostCenterID = ""
	for name, c := range map[string]struct {
		l    line
		p    proposal
		want error
	}{
		"cost to another cost":       {withCost, proposal{cost: "CC2"}, nil},
		"cost to its own":            {withCost, proposal{cost: "CC1"}, ErrTarget},
		"cost to a profit center":    {withCost, proposal{profit: "PC2"}, ErrTarget},
		"profit to another profit":   {withProfit, proposal{profit: "PC2"}, nil},
		"profit to its own":          {withProfit, proposal{profit: "PC1"}, ErrTarget},
		"profit to a cost center":    {withProfit, proposal{cost: "CC2"}, nil},
		"profit to its own via cost": {withProfit, proposal{cost: "CC1"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if err := target(c.l, c.p); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

// TestProposalOf: the proposer's input needs an entry, a line from 1,
// exactly one center and a reason of 1 to 200 characters.
func TestProposalOf(t *testing.T) {
	ok := func() map[string]any {
		return map[string]any{"journal_entry_id": "E", "line_no": "1", "cost_center_id": "CC2", "reason": "Its text names the sales team."}
	}
	for name, c := range map[string]struct {
		change func(map[string]any)
		want   error
	}{
		"in form":           {func(map[string]any) {}, nil},
		"a reason of 200":   {func(in map[string]any) { in["reason"] = strings.Repeat("é", 200) }, nil},
		"a reason of 201":   {func(in map[string]any) { in["reason"] = strings.Repeat("é", 201) }, ErrInput},
		"a blank reason":    {func(in map[string]any) { in["reason"] = "  " }, ErrInput},
		"no entry":          {func(in map[string]any) { delete(in, "journal_entry_id") }, ErrInput},
		"line 0":            {func(in map[string]any) { in["line_no"] = 0 }, ErrInput},
		"a line in words":   {func(in map[string]any) { in["line_no"] = "one" }, ErrInput},
		"both centers":      {func(in map[string]any) { in["profit_center_id"] = "PC2" }, ErrInput},
		"no center":         {func(in map[string]any) { delete(in, "cost_center_id") }, ErrInput},
		"a center not text": {func(in map[string]any) { in["cost_center_id"] = 7 }, ErrInput},
	} {
		t.Run(name, func(t *testing.T) {
			in := ok()
			c.change(in)
			if _, err := proposalOf(in); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

// TestRepost: the reposting of a line with a profit center alone moves it
// between profit centers; one of a special period is parked into it; an
// amount that is no amount, or zero, is not reposted.
func TestRepost(t *testing.T) {
	e, l := march()
	l.CostCenterID = ""
	e.FiscalPeriod = 13
	body, err := repost(e, l, proposal{profit: "PC2", reason: "r"}, "RP", "")
	if err != nil {
		t.Fatal(err)
	}
	lines := body["lines"].([]any)
	from, to := lines[0].(map[string]any), lines[1].(map[string]any)
	for name, c := range map[string][2]any{
		"from":           {fmt.Sprint(from["profit_center_id"], " ", from["amount"].(map[string]any)["amount"]), "PC1 -120.00"},
		"to":             {fmt.Sprint(to["profit_center_id"], " ", to["amount"].(map[string]any)["amount"]), "PC2 120.00"},
		"special period": {body["fiscal_period"], 13},
		"every ledger":   {body["ledger_id"], nil},
	} {
		t.Run(name, func(t *testing.T) {
			if c[0] != c[1] {
				t.Errorf("got %v, want %v", c[0], c[1])
			}
		})
	}
	for _, amount := range []string{"0.00", "12.0.0", ""} {
		t.Run("amount "+amount, func(t *testing.T) {
			l.AmountLC.Amount = amount
			if _, err := repost(e, l, proposal{profit: "PC2"}, "RP", ""); !errors.Is(err, ErrSource) {
				t.Errorf("got %v, want %v", err, ErrSource)
			}
		})
	}
}

// fake is an API that answers each path with a status and a body, and
// 500 any other.
func fake(t *testing.T, routes map[string][2]string) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var status int
		if _, err := fmt.Sscan(a[0], &status); err != nil {
			t.Errorf("route %s: %v", r.URL.Path, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(a[1])) // the test reads what arrives
	}))
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL}
}

// TestAPIFailures: a read the run's code makes that fails stops the
// proposal (the parked entries, a ledger's copies) or the run (the
// ledgers), naming what it read.
func TestAPIFailures(t *testing.T) {
	api := fake(t, nil)
	r := &run{api: api, company: "C", ledgers: []ledger{{ID: "0L"}, {ID: "1L"}}, proposed: map[string]bool{}}
	if err := r.unproposed(context.Background(), "RP"); err == nil || !strings.Contains(err.Error(), "the parked entries of RP") {
		t.Errorf("parked entries: got %v, want their read's error", err)
	}
	e, _ := march()
	if _, err := r.postedTo(context.Background(), e); err == nil || !strings.Contains(err.Error(), "the copies of entry 100001 in ledger 1L") {
		t.Errorf("copies: got %v, want their read's error", err)
	}
	companies := fake(t, map[string][2]string{"GET /v1/legal-entities": {"200", `{"items": [{"id": "C", "code": "2000"}]}`}})
	res, err := (&Agent{API: companies}).Repostings(context.Background(), Period{Company: "2000", Year: 2026, Period: 3})
	if err == nil || !strings.Contains(err.Error(), "the ledgers of company C") || res.Session != "" {
		t.Errorf("ledgers: got %v and session %q, want their read's error before a session", err, res.Session)
	}
}

// TestParked: an entry the API parked is the run's though the session
// failed to record the call (apitools.ErrNotRecorded), and is no
// refusal; an answer that names no entry parked nothing.
func TestParked(t *testing.T) {
	api := fake(t, map[string][2]string{
		"POST /v1/agent-sessions":         {"201", `{"id": "S", "autonomy_level": 1}`},
		"POST /v1/parked-journal-entries": {"201", `{"id": "P"}`},
	})
	s, err := apitools.StartSession(context.Background(), api, "test", "m")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := s.Tools(context.Background(), Parker)
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{}
	r := &run{res: res, park: tools[0], proposed: map[string]bool{}}
	if _, err := r.propose(context.Background(), map[string]any{}); !errors.Is(err, ErrInput) || res.Refused != 1 {
		t.Errorf("refused: got %v and %d refused, want ErrInput counted", err, res.Refused)
	}
	_, err = r.parked(context.Background(), map[string]any{}, Proposal{Reference: "RP"})
	if !errors.Is(err, apitools.ErrNotRecorded) || len(res.Proposals) != 1 || res.Proposals[0].ParkedEntryID != "P" || !r.proposed["RP"] {
		t.Errorf("not recorded: got %v and %v, want the entry kept and ErrNotRecorded", err, res.Proposals)
	}
	unnamed := fake(t, map[string][2]string{"POST /v1/parked-journal-entries": {"201", `{}`}})
	tools, err = apitools.Tools(unnamed, nil, Parker)
	if err != nil {
		t.Fatal(err)
	}
	r.park = tools[0]
	if _, err := r.parked(context.Background(), map[string]any{}, Proposal{Reference: "RP2"}); err == nil || !strings.Contains(err.Error(), "names no parked entry") {
		t.Errorf("no entry: got %v, want the answer named as parking nothing", err)
	}
}

// TestRepostingRefused: a document number too long for the reference
// that names the line, and a line of no amount, refuse the proposal.
func TestRepostingRefused(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*entry)
		want   string
	}{
		"a long document number": {func(e *entry) { e.DocumentNumber = strings.Repeat("9", 30) }, "too long"},
		"no amount":              {func(e *entry) { e.Lines[0].AmountLC.Amount = "0.00" }, ErrSource.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := march()
			c.change(&e)
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			api := fake(t, map[string][2]string{"GET /v1/journal-entries/E": {"200", string(b)}, "GET /v1/parked-journal-entries": {"200", `{"items": []}`}})
			r := &run{api: api, res: &Result{}, company: "C", period: Period{Year: 2026, Period: 3}, proposed: map[string]bool{}}
			_, err = r.reposting(context.Background(), map[string]any{"journal_entry_id": "E", "line_no": 1, "cost_center_id": "CC2", "reason": "r"})
			if !errors.Is(err, ErrSource) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want ErrSource with %q", err, c.want)
			}
		})
	}
}
