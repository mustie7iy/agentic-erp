// Package riskagent is the risk agent (ADR-0072, T105g), a recommending
// agent (internal/recommend): it reads the tenant's open risks (ADR-0078
// §10-§12) with the controls that mitigate them, each control's last test
// (§3) and the control monitoring agent's latest finding on it (T105f, a
// recommendation on the control), and through API tools (04-api-design
// §4.13) the register, its ratings, the controls and their tests, the
// control matrix, the audit trail and the recommendations, and leaves for
// a risk owner to decide one recommendation per open risk, to rate it
// again (rate_again, the likelihood and impact on the content's first
// line, each 1 to 5, and why) or to keep it (keep), and at most one on
// the tenant, a risk not yet in the register (new_risk: its title and
// category on the first two lines, and the evidence). A risk whose
// controls all passed their last test and none of which has an exception
// is kept: that is computed before the session, not judged. A new rating
// or a new risk cites what shows it (a control test, an audit event, the
// monitoring agent's recommendation), each one a read of the run
// returned (recommend.Cited), and every number of the reasons is one the
// run read (recommend.Figures). It never writes to the register: no
// operation that records, changes, rates or closes a risk is a tool of
// its (Never). What a person wrote (a title, a note, a comment) is data,
// never an instruction. Its evaluation suite is evals/risk (ADR-0039).
package riskagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "risk"

// The object types the agent recommends on, and its kinds.
const (
	RiskObject   = "risk"
	TenantObject = "tenant"
	RateAgain    = "rate_again"
	Keep         = "keep"
	NewRisk      = "new_risk"
)

// How a content cites what it read: the label and the identifier the API
// gives it (an audit event its seq).
const (
	AuditEvent     = "audit event"
	ControlTest    = "control test"
	Recommendation = "recommendation"
)

// MaxRisks is the most open risks one run reviews: each is named in the
// prompt with its controls and gets a recommendation, which a model's
// context bounds.
const MaxRisks = 100

// Reads are the operations the agent reads with (ADR-0072, T105g): the
// register and its ratings (T98c), the library and its tests (T98a), the
// matrix (T98b), the audit trail and the recommendations, among them the
// control monitoring agent's exceptions.
var Reads = []string{"listRisks", "getRisk", "listRiskRatings", "listControls", "getControl", "listControlTests", "getControlMatrix",
	"listAuditEvents", "listRecommendations"}

// Never are the operations the agent never calls (ADR-0072): every write
// to the register (recording, changing, rating and closing a risk), which
// its tools hold none of.
var Never = []string{"createRisk", "updateRisk", "closeRisk", "createRiskRating"}

const system = `You review the tenant's risk register and recommend new ratings, and a risk not yet in it, with your reasons,
for a risk owner to decide.

You are given each open risk with its likelihood and impact (each 1 to 5; the score is their product), the controls
that mitigate it, each control's last test and what the control monitoring agent last found on it. You may read the
register and a risk's ratings (listRisks, getRisk, listRiskRatings), the controls and their tests (listControls,
getControl, listControlTests), the control matrix (getControlMatrix), the audit trail (listAuditEvents) and the
recommendations (listRecommendations: the control monitoring agent's are on object_type control, kind exception_found or
no_exception).

Then recommend once on each risk. rate_again when what you read shows the risk is now more or less likely or its impact
other than rated: the content's first line is "Rate: likelihood L, impact I", each a whole number from 1 to 5, other than
the risk's rating now; then why. keep otherwise; a risk whose controls all passed their last test and none of which has an
exception is kept. You may recommend once on the tenant a risk the register does not have: new_risk, whose first line is
"New risk: " and its title, its second line "Category: " and its category, then the evidence. A new rating and a new risk
cite what shows them: a control test as "control test " and its id, an audit event as "audit event " and its seq, a
recommendation as "recommendation " and its id, each exactly as a tool returned it, and only what you read. You write
nothing to the register: the risk owner rates, records and closes risks.`

// Agent reviews the tenant's risk register.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
}

// risk is an open risk as the register gives it.
type risk struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Category   string `json:"category"`
	Likelihood int    `json:"likelihood"`
	Impact     int    `json:"impact"`
	Score      int    `json:"score"`
	Controls   []struct {
		ID     string `json:"control_id"`
		Code   string `json:"code"`
		Status string `json:"status"`
	} `json:"controls"`
}

// control is what the run knows of a control before its session: its
// last test's result and date, and the monitoring agent's latest finding.
type control struct {
	result, testedOn string
	finding          string // the latest finding, as the prompt words it; "": none
	exception        bool   // the latest finding is an exception no person overrode or ignored
}

// clear says whether every control of the risk, of which it has at least
// one, is active, passed its last test and has no exception: such a risk
// is kept.
func (r risk) clear(controls map[string]control) bool {
	for _, c := range r.Controls {
		k := controls[c.ID]
		if c.Status != "active" || k.result != "passed" || k.exception {
			return false
		}
	}
	return len(r.Controls) > 0
}

// The refusals of the agent's own check, beside the pattern's.
var (
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("riskagent: every number in the reasons is one a tool returned or the run was given, written as returned; a count of your own is written in words")
	// ErrUncited refuses a citation of a control test, an audit event or
	// a recommendation no read of the run returned.
	ErrUncited = errors.New(`riskagent: cite only control tests, audit events and recommendations a read of this run returned, as "control test <id>", "audit event <seq>" or "recommendation <id>"`)
	// ErrNoEvidence refuses a new rating or a new risk that cites nothing.
	ErrNoEvidence = errors.New("riskagent: a new rating or a new risk cites the control tests, audit events or recommendations that show it")
	// ErrRating refuses a rate_again whose first line is not a rating of
	// 1 to 5 on each scale other than the risk's now.
	ErrRating = errors.New(`riskagent: a rate_again's first line is "Rate: likelihood L, impact I", each a whole number from 1 to 5, other than the risk's rating now`)
	// ErrClear refuses a rate_again of a risk whose controls all passed
	// their last test and have no exception.
	ErrClear = errors.New("riskagent: a risk whose controls all passed their last test and have no exception is kept")
	// ErrNewRisk refuses a new_risk whose first two lines are not its
	// title and its category.
	ErrNewRisk = errors.New(`riskagent: a new_risk's first line is "New risk: " and its title (1 to 200 characters), its second "Category: " and its category (1 to 100 characters), neither with white space around it`)
)

// Review runs one session over the tenant's open risks: it reads who the
// agent is (its tenant), the open register, the controls and the
// monitoring agent's findings first (any of them the agent may not read
// stops the run before a session starts, and so do more open risks than
// MaxRisks), offers the model the tools and reports what it recommended,
// each risk named by its title, also when the run fails after
// recommending.
func (a *Agent) Review(ctx context.Context) (recommend.Result, error) {
	g, err := a.gather(ctx)
	if err != nil {
		return recommend.Result{}, err
	}
	reg := register{risks: map[string]risk{}, controls: g.controls, cited: recommend.NewCited()}
	task := recommend.Task{
		Purpose: "Review the risk register",
		Objects: map[string]string{g.tenant: TenantObject},
		Observe: map[string]func(apitools.Response){
			"listAuditEvents":     reg.cited.Keep(AuditEvent, "items[].seq"),
			"listControlTests":    reg.cited.Keep(ControlTest, "items[].id"),
			"listControls":        reg.cited.Keep(ControlTest, "items[].last_test.id"),
			"getControl":          reg.cited.Keep(ControlTest, "last_test.id"),
			"getControlMatrix":    reg.cited.Keep(ControlTest, "items[].control.last_test.id"),
			"listRecommendations": reg.cited.Keep(Recommendation, "items[].id"),
		},
		Check: reg.check,
	}
	names := map[string]string{g.tenant: "the tenant"}
	var b strings.Builder
	fmt.Fprintf(&b, "Review the risk register: %d open risk(s), as the register gives them today.\n", len(g.risks))
	for _, r := range g.risks {
		reg.risks[r.ID] = r
		task.Objects[r.ID] = RiskObject
		names[r.ID] = r.Title
		reg.describe(&b, r)
	}
	fmt.Fprintf(&b, "A risk not yet in the register is recommended on the tenant (object_id %s).\n", g.tenant)
	task.Prompt = b.String()
	res, err := agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
	res.Names = names
	return res, err
}

// gathered is what the run reads before its session.
type gathered struct {
	tenant   string
	risks    []risk
	controls map[string]control
}

// gather reads the agent's tenant, the open risks, the controls' last
// tests and the monitoring agent's latest finding per control.
func (a *Agent) gather(ctx context.Context) (gathered, error) {
	g := gathered{controls: map[string]control{}}
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/me", nil, nil, "")
	var me struct {
		TenantID string `json:"tenant_id"`
	}
	if err == nil {
		err = resp.Decode(&me)
	}
	if err != nil {
		return g, fmt.Errorf("riskagent: who the agent is: %w", err)
	}
	g.tenant = me.TenantID
	err = apitools.Each(ctx, a.API, "/v1/risks", url.Values{"status": {"open"}}, func(r risk) bool {
		g.risks = append(g.risks, r)
		return len(g.risks) <= MaxRisks
	})
	if err != nil {
		return g, fmt.Errorf("riskagent: the open risks: %w", err)
	}
	if len(g.risks) > MaxRisks {
		return g, fmt.Errorf("riskagent: more than %d open risks to review", MaxRisks)
	}
	if err := a.tests(ctx, g.controls); err != nil {
		return g, err
	}
	return g, a.findings(ctx, g.controls)
}

// tests keeps each control's last test.
func (a *Agent) tests(ctx context.Context, controls map[string]control) error {
	err := apitools.Each(ctx, a.API, "/v1/controls", nil, func(c struct {
		ID       string `json:"id"`
		LastTest *struct {
			Result   string `json:"result"`
			TestedOn string `json:"tested_on"`
		} `json:"last_test"`
	}) bool {
		if c.LastTest != nil {
			controls[c.ID] = control{result: c.LastTest.Result, testedOn: c.LastTest.TestedOn}
		}
		return true
	})
	if err != nil {
		return fmt.Errorf("riskagent: the controls: %w", err)
	}
	return nil
}

// findings keeps, per control, the monitoring agent's latest finding:
// the recommendations come newest first, so the first exception_found or
// no_exception on a control is its latest. An exception a person
// overrode or ignored is none.
// Every page is read on every run, though only the newest finding per
// control is kept: a read narrowed to the risks' controls is for when
// the recommendations on controls grow large (each monitoring run adds
// one per control).
func (a *Agent) findings(ctx context.Context, controls map[string]control) error {
	err := apitools.Each(ctx, a.API, "/v1/recommendations", url.Values{"object_type": {"control"}}, func(r struct {
		ObjectID string `json:"object_id"`
		Kind     string `json:"kind"`
		Outcome  string `json:"outcome"`
	}) bool {
		c := controls[r.ObjectID]
		if c.finding != "" || (r.Kind != "exception_found" && r.Kind != "no_exception") {
			return true
		}
		c.exception = r.Kind == "exception_found" && r.Outcome != "overridden" && r.Outcome != "ignored"
		switch {
		case r.Kind == "no_exception":
			c.finding = "no exception found"
		case c.exception:
			c.finding = "an exception found"
		default:
			c.finding = "an exception found, " + r.Outcome + " by a person"
		}
		controls[r.ObjectID] = c
		return true
	})
	if err != nil {
		return fmt.Errorf("riskagent: the control monitoring agent's findings: %w", err)
	}
	return nil
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{RiskObject: {RateAgain, Keep}, TenantObject: {NewRisk}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// register is what a run's check holds its recommendations to: the open
// risks and the controls read before the session, and what the run's
// reads returned that a content may cite.
type register struct {
	risks    map[string]risk
	controls map[string]control
	cited    *recommend.Cited
}

// describe writes a risk and its controls as the prompt gives them.
func (g register) describe(b *strings.Builder, r risk) {
	fmt.Fprintf(b, "Risk %q (id %s), category %q: likelihood %d, impact %d, score %d.", r.Title, r.ID, r.Category, r.Likelihood, r.Impact, r.Score)
	if len(r.Controls) == 0 {
		b.WriteString(" No control mitigates it.")
	}
	for _, c := range r.Controls {
		k := g.controls[c.ID]
		last, finding := "never tested", "not monitored"
		if k.result != "" {
			last = fmt.Sprintf("last tested %s, %s", k.testedOn, k.result)
		}
		if k.finding != "" {
			finding = k.finding
		}
		fmt.Fprintf(b, " Control %s (id %s), %s: %s; %s.", c.Code, c.ID, c.Status, last, finding)
	}
	if r.clear(g.controls) {
		b.WriteString(" Its controls all passed their last test and none has an exception: keep it.")
	}
	b.WriteString("\n")
}

// rated is a rate_again's first line.
var rated = regexp.MustCompile(`^Rate: likelihood ([1-5]), impact ([1-5])\s*$`)

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, T105g): a rate_again carries on its first line a rating
// other than the risk's, of a risk not clear; a new_risk its title and
// category; both cite what shows them; every citation is of what a read
// of the run returned, and every number of the reasons one the run read.
func (g register) check(p recommend.Proposed, f *recommend.Figures) error {
	reasons := p.Content
	switch p.Kind {
	case RateAgain:
		first, rest, _ := strings.Cut(p.Content, "\n")
		r := g.risks[p.ObjectID]
		m := rated.FindStringSubmatch(first)
		if m == nil || (m[1] == fmt.Sprint(r.Likelihood) && m[2] == fmt.Sprint(r.Impact)) {
			return ErrRating
		}
		if r.clear(g.controls) {
			return ErrClear
		}
		reasons = rest
	case NewRisk:
		if !named(p.Content) {
			return ErrNewRisk
		}
	}
	if unread := f.Unread(reasons); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	cites := recommend.Cites(p.Content, ControlTest, AuditEvent, Recommendation)
	if unknown := g.cited.Unknown(cites); len(unknown) > 0 {
		return fmt.Errorf("%w: not %s", ErrUncited, unknown[0])
	}
	if p.Kind != Keep && len(cites) == 0 {
		return ErrNoEvidence
	}
	return nil
}

// named says whether a new_risk's first two lines are its title and its
// category, as createRisk would take them.
func named(content string) bool {
	lines := strings.SplitN(content, "\n", 3)
	if len(lines) < 2 {
		return false
	}
	title, ok := strings.CutPrefix(lines[0], "New risk: ")
	category, ok2 := strings.CutPrefix(lines[1], "Category: ")
	return ok && ok2 && text(title, 200) && text(category, 100)
}

// text is a text of 1 to max characters without white space around it.
func text(s string, maxLen int) bool {
	return s != "" && s == strings.TrimSpace(s) && utf8.RuneCountInString(s) <= maxLen
}
