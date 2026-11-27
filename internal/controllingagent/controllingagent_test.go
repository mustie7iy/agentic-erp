package controllingagent_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/controllingagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"tenancy.org.read", "ledger.entry.read", "ledger.entry.park", "ledger.entry.post", "ledger.account.read",
	"agents.session.read", "agents.session.record"}

// ledgers gives company 2000 three ledgers (fixture.ThreeLedgers) and a
// group TWO of two of them, 0L and 1L (ADR-0045).
func ledgers(doc *config.Config) {
	fixture.ThreeLedgers(doc)
	doc.LedgerGroups = append(doc.LedgerGroups, config.LedgerGroup{Code: "TWO", Name: "Two ledgers", Representative: config.LeadingLedger,
		Ledgers: []string{config.LeadingLedger, "1L"}})
}

// reposting is company 2000 with its cost centers (fixture.CostCenters:
// Admin, Sales, Locked) in three ledgers, and the agent's client of the
// API.
type reposting struct {
	fixture.Env
	centers fixture.Centers
	api     *apitools.Client
}

func setup(t *testing.T) reposting {
	t.Helper()
	f := fixture.NewWith(t, nil, ledgers, perms...)
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	c := f.CostCenters(t, ledgers)
	srv := httptest.NewServer(f.API.Handler)
	t.Cleanup(srv.Close)
	return reposting{Env: f, centers: c, api: &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}}
}

// posted posts, as the tenant's person, 100.00 EUR of office supplies
// (520000) on cost center Admin against the bank on 10 March 2026, the
// sales team's trade fair travel by its texts, with extra JSON of the
// entry (a ledger or a group), and returns the leading ledger's entry's
// id and document number.
func (r reposting) posted(t *testing.T, extra string) (id, number string) {
	t.Helper()
	body := fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-10", "posting_date": "2026-03-10", "currency": "EUR",
		"header_text": "Trade fair travel"%s, "lines": [
		{"account_id": %q, "amount": {"amount": "100.00", "currency": "EUR"}, "cost_center_id": %q, "line_text": "Trade fair travel of the sales team"},
		{"account_id": %q, "amount": {"amount": "-100.00", "currency": "EUR"}}]}`,
		r.DE, extra, r.Account(t, "2000", "520000"), r.centers.Admin, r.Account(t, "2000", "100000"))
	e, _ := r.Must(201, "POST", "/v1/journal-entries", body, "")
	return apitest.String(t, e, "id"), apitest.String(t, e, "document_number")
}

// proposal is the model's proposal of the entry's line onto the cost
// center, with a reason.
func proposal(entry string, line int, center uuid.UUID) map[string]any {
	return map[string]any{"journal_entry_id": entry, "line_no": line, "cost_center_id": center.String(),
		"reason": "The line is the sales team's trade fair travel, which the company posts on cost center 2000."}
}

// march is the run of company 2000's March 2026.
var march = controllingagent.Period{Company: "2000", Year: 2026, Period: 3}

// checks are values and what each should be, checked as subtests.
type checks map[string][2]any

func (cs checks) check(t *testing.T) {
	t.Helper()
	for name, c := range cs {
		t.Run(name, func(t *testing.T) {
			if c[0] != c[1] {
				t.Errorf("got %v, want %v", c[0], c[1])
			}
		})
	}
}

// TestRepostings: one session on the period; the model reads an entry
// and proposes; an unknown entry, a line without a center, the line's own
// center, a proposal without a reason, a center the ledger refuses (locked)
// and the same line twice are each refused and counted; the one parked is
// reported, built from the line itself: its whole amount on its account,
// off Admin and onto Sales, dated as posted, the reference naming the
// line, the reason its header text; a person posts it; and a later run
// does not propose the line again (ADR-0050 §12, T56j1).
func TestRepostings(t *testing.T) {
	r := setup(t)
	id, number := r.posted(t, "")
	model := llmtest.New(t,
		llmtest.Tool("getJournalEntry", map[string]any{"id": id}),
		llmtest.Tool(controllingagent.Proposer, proposal(uuid.NewString(), 1, r.centers.Sales)),
		llmtest.Tool(controllingagent.Proposer, proposal(id, 2, r.centers.Sales)),
		llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Admin)),
		llmtest.Tool(controllingagent.Proposer, map[string]any{"journal_entry_id": id, "line_no": 1, "cost_center_id": r.centers.Sales.String()}),
		llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Locked)),
		llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales)),
		llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales)),
		llmtest.Final("Proposed the trade fair travel's reposting to cost center 2000."))
	res, err := (&controllingagent.Agent{LLM: model.Runner(), API: r.api}).Repostings(context.Background(), march)
	if err != nil {
		t.Fatalf("repostings: %v", err)
	}
	if len(res.Proposals) != 1 {
		t.Fatalf("proposals: got %v, want one", res.Proposals)
	}
	p := res.Proposals[0]
	pe, tag := r.Must(200, "GET", "/v1/parked-journal-entries/"+p.ParkedEntryID, "", "")
	lines := apitest.List(t, pe, "lines")
	from, to := lines[0].(map[string]any), lines[1].(map[string]any)
	reference := fmt.Sprintf("RP 2026 SA %s/1", number)
	checks{
		"the proposal's line":          {fmt.Sprint(p.JournalEntryID, " ", p.LineNo), id + " 1"},
		"its reference reported":       {p.Reference, reference},
		"proposals refused":            {res.Refused, 6},
		"the reply":                    {res.Reply, "Proposed the trade fair travel's reposting to cost center 2000."},
		"tools offered":                {fmt.Sprint(slices.Sorted(slices.Values(model.ToolNames()))), fmt.Sprint(slices.Sorted(slices.Values(append(slices.Clone(controllingagent.Reads), controllingagent.Proposer))))},
		"an unknown entry refused":     {strings.Contains(model.Requests[2], "the line reposted is a line of a journal entry"), true},
		"a line without a center":      {strings.Contains(model.Requests[3], "the line reposted is a line of a journal entry"), true},
		"its own center refused":       {strings.Contains(model.Requests[4], "is reposted to another cost center"), true},
		"no reason refused":            {strings.Contains(model.Requests[5], "a reason of 1 to 200 characters"), true},
		"the ledger's refusal":         {strings.Contains(model.Requests[6], "ledger.cost_center_invalid"), true},
		"the same line twice":          {strings.Contains(model.Requests[8], "in this run"), true},
		"parked by the agent":          {fmt.Sprint(pe["status"], " ", pe["created_by_kind"]), "parked agent"},
		"its reference":                {pe["reference"], reference},
		"its header text, the reason":  {pe["header_text"], "The line is the sales team's trade fair travel, which the company posts on cost center 2000."},
		"dated as the line was posted": {fmt.Sprint(pe["document_date"], " ", pe["posting_date"]), "2026-03-10 2026-03-10"},
		"in every ledger":              {pe["ledger_specific"], false},
		"off Admin":                    {fmt.Sprint(from["amount"].(map[string]any)["amount"], " ", from["cost_center_id"]), "-100.00 " + r.centers.Admin.String()},
		"onto Sales":                   {fmt.Sprint(to["amount"].(map[string]any)["amount"], " ", to["cost_center_id"]), "100.00 " + r.centers.Sales.String()},
		"on the line's account":        {fmt.Sprint(from["account_id"], " ", to["account_id"]), fmt.Sprint(r.Account(t, "2000", "520000"), " ", r.Account(t, "2000", "520000"))},
	}.check(t)
	posted, _ := r.Must(200, "POST", "/v1/parked-journal-entries/"+p.ParkedEntryID+":post", `{}`, tag)
	if posted["status"] != "posted" {
		t.Fatalf("the person's posting: got %v, want posted", posted["status"])
	}
	again := llmtest.New(t, llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales)), llmtest.Final("Done."))
	res, err = (&controllingagent.Agent{LLM: again.Runner(), API: r.api}).Repostings(context.Background(), march)
	checks{
		"a later run's error":     {err, nil},
		"a later run's proposals": {len(res.Proposals), 0},
		"posted, not again":       {strings.Contains(again.Requests[1], "posted"), true},
	}.check(t)
}

// TestRepostingLedgers: a reposting posts to the ledgers the line's
// posting did: every ledger the company keeps, the leading ledger alone
// (ledger-specific), or, for a posting in two of its three ledgers, none:
// a person reposts it (ErrLedgers).
func TestRepostingLedgers(t *testing.T) {
	r := setup(t)
	leading := r.Ledger(t, config.LeadingLedger).String()
	for name, c := range map[string]struct {
		extra, want string
	}{
		"every ledger":            {"", "parked false"},
		"the leading ledger only": {fmt.Sprintf(`, "ledger_id": %q`, leading), "parked true"},
		"two ledgers of three":    {`, "ledger_group": "TWO"`, "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			id, _ := r.posted(t, c.extra)
			model := llmtest.New(t, llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales)), llmtest.Final("Done."))
			res, err := (&controllingagent.Agent{LLM: model.Runner(), API: r.api}).Repostings(context.Background(), march)
			if err != nil {
				t.Fatalf("repostings: %v", err)
			}
			got := "refused"
			if len(res.Proposals) == 1 {
				pe, _ := r.Must(200, "GET", "/v1/parked-journal-entries/"+res.Proposals[0].ParkedEntryID, "", "")
				got = fmt.Sprint(pe["status"], " ", pe["ledger_specific"])
				if pe["ledger_id"] != leading {
					t.Errorf("ledger: got %v, want the leading ledger %s", pe["ledger_id"], leading)
				}
			} else if !strings.Contains(model.Requests[1], controllingagent.ErrLedgers.Error()) {
				t.Errorf("refusal: got %s, want %v", llmtest.LastToolResult(model.Requests[1]), controllingagent.ErrLedgers)
			}
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestRepostingsOtherPeriod: a line of another period than the run's is
// not reposted, whatever the model asks.
func TestRepostingsOtherPeriod(t *testing.T) {
	r := setup(t)
	id, _ := r.posted(t, "")
	model := llmtest.New(t, llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales)), llmtest.Final("Done."))
	res, err := (&controllingagent.Agent{LLM: model.Runner(), API: r.api}).Repostings(context.Background(),
		controllingagent.Period{Company: "2000", Year: 2026, Period: 4})
	checks{
		"no error":  {err, nil},
		"proposals": {len(res.Proposals), 0},
		"refused":   {res.Refused, 1},
	}.check(t)
}

// TestRepostingsStopped: a person cannot start the agent's session; a
// model that does not answer fails the run with no reply, its session
// reported; a run bounded to one request stops after it; a period no
// company keeps and a company the agent may not read stop the run before
// a session starts.
func TestRepostingsStopped(t *testing.T) {
	r := setup(t)
	t.Run("a person", func(t *testing.T) {
		person := &apitools.Client{BaseURL: r.api.BaseURL, Token: r.API.Issuer.Token(t, r.Tenant.ID, r.Tenant.Human.Subject, nil)}
		_, err := (&controllingagent.Agent{LLM: llmtest.New(t).Runner(), API: person}).Repostings(context.Background(), march)
		if got := fmt.Sprint(err); !strings.Contains(got, "not_an_agent") {
			t.Errorf("got %s, want agents.not_an_agent", got)
		}
	})
	t.Run("a model that does not answer", func(t *testing.T) {
		res, err := (&controllingagent.Agent{LLM: llmtest.New(t).Runner(), API: r.api}).Repostings(context.Background(), march)
		checks{
			"an error":    {err != nil, true},
			"no reply":    {res.Reply, ""},
			"its session": {res.Session != "", true},
		}.check(t)
	})
	t.Run("one request at most", func(t *testing.T) {
		model := llmtest.New(t, llmtest.Tool("listAccounts", map[string]any{}), llmtest.Final("never asked"))
		res, err := (&controllingagent.Agent{LLM: model.Runner(), API: r.api, MaxIterations: 1}).Repostings(context.Background(), march)
		checks{
			"no error":    {err, nil},
			"model calls": {model.Calls(), 1},
			"proposals":   {len(res.Proposals), 0},
		}.check(t)
	})
	for name, c := range map[string]struct {
		p    controllingagent.Period
		want string
	}{
		"period 17":       {controllingagent.Period{Company: "2000", Year: 2026, Period: 17}, controllingagent.ErrPeriod.Error()},
		"period 0":        {controllingagent.Period{Company: "2000", Year: 2026}, controllingagent.ErrPeriod.Error()},
		"year 1900":       {controllingagent.Period{Company: "2000", Year: 1900, Period: 3}, controllingagent.ErrPeriod.Error()},
		"year 3000":       {controllingagent.Period{Company: "2000", Year: 3000, Period: 3}, controllingagent.ErrPeriod.Error()},
		"another company": {controllingagent.Period{Company: "9999", Year: 2026, Period: 3}, "no company 9999"},
	} {
		t.Run(name, func(t *testing.T) {
			model := llmtest.New(t)
			res, err := (&controllingagent.Agent{LLM: model.Runner(), API: r.api}).Repostings(context.Background(), c.p)
			checks{
				"the error":   {err != nil && strings.Contains(err.Error(), c.want), true},
				"no session":  {res.Session, ""},
				"model calls": {model.Calls(), 0},
			}.check(t)
		})
	}
}

// TestRepostingsFailures: an API that cannot be reached, and a contract
// without one of the tools, stop the run before the model is asked, the
// session ended rather than left running.
func TestRepostingsFailures(t *testing.T) {
	r := setup(t)
	reads := controllingagent.Reads
	for name, c := range map[string]struct {
		api   *apitools.Client
		reads []string
		want  string
	}{
		"no server":        {&apitools.Client{BaseURL: "http://127.0.0.1:1"}, reads, "connect"},
		"a tool not there": {r.api, append(slices.Clone(reads), "noSuchOperation"), "noSuchOperation"},
	} {
		t.Run(name, func(t *testing.T) {
			controllingagent.Reads = c.reads
			t.Cleanup(func() { controllingagent.Reads = reads })
			model := llmtest.New(t)
			_, err := (&controllingagent.Agent{LLM: model.Runner(), API: c.api}).Repostings(context.Background(), march)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %s", err, c.want)
			}
			if model.Calls() != 0 {
				t.Errorf("model calls: got %d, want 0", model.Calls())
			}
			sessions, _ := r.As(r.Tenant.Agent).Must(200, "GET", "/v1/agent-sessions?status=running", "", "")
			if got := len(apitest.List(t, sessions, "items")); got != 0 {
				t.Errorf("running sessions: got %d, want 0", got)
			}
		})
	}
}

// TestRepostingsTogether: the same line proposed twice in one turn, which
// the runner executes concurrently, is parked once and refused once (run
// with -race, review:batch76).
func TestRepostingsTogether(t *testing.T) {
	r := setup(t)
	id, _ := r.posted(t, "")
	model := llmtest.New(t,
		llmtest.Together(llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales)),
			llmtest.Tool(controllingagent.Proposer, proposal(id, 1, r.centers.Sales))),
		llmtest.Final("Proposed."))
	res, err := (&controllingagent.Agent{LLM: model.Runner(), API: r.api}).Repostings(context.Background(), march)
	if err != nil {
		t.Fatalf("repostings: %v", err)
	}
	parked, _ := r.Must(200, "GET", "/v1/parked-journal-entries?legal_entity_id="+r.DE.String()+"&created_by_kind=agent", "", "")
	checks{
		"proposals reported": {len(res.Proposals), 1},
		"proposals refused":  {res.Refused, 1},
		"entries parked":     {len(apitest.List(t, parked, "items")), 1},
	}.check(t)
}
