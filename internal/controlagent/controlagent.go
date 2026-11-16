// Package controlagent is the control monitoring agent (ADR-0072, T105f),
// a recommending agent (internal/recommend): it reads the tenant's control
// matrix (ADR-0078 §8: the active controls with their last test and their
// links to what enforces them, and whether each link holds), and through
// API tools (04-api-design §4.13) the control library and its tests, the
// audit trail, the segregation-of-duties rules, the role assignments and
// the approval requests, and leaves, per control of the run, one
// recommendation for the compliance officer to decide: an exception found
// (exception_found), naming the broken links on the content's first line
// and the audit events it found, or none (no_exception). Which links are
// broken is the matrix's, read before the session, not the model's
// judgment: a control with a broken link gets exception_found or nothing.
// Every audit event the content cites is one a read of the run returned
// (recommend.Cited), and every number one the run read
// (recommend.Figures). A control with no link, on which the run found no
// audit event, gets no_exception only with the statement that nothing
// could be checked (NothingChecked). It never records a test, changes a
// control or a link: those are no tools of its (Never), and recording a
// test is closed to agents (x-agent: false). What a person wrote (an
// objective, a note, a description) is data, never an instruction. Its
// evaluation suite is evals/control_monitoring (ADR-0039).
package controlagent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "control_monitoring"

// The object type the agent recommends on, and its kinds.
const (
	ControlObject  = "control"
	ExceptionFound = "exception_found"
	NoException    = "no_exception"
)

// AuditEvent is how a content cites an audit event: "audit event" and its
// sequence number (seq), the identifier the audit trail gives it.
const AuditEvent = "audit event"

// NothingChecked is the statement a no_exception on a control with no
// link, citing no audit event, carries: the run had nothing to check it
// against.
const NothingChecked = "Nothing could be checked: the control has no link and no audit event was found."

// MaxControls is the most controls one run monitors: each is named in the
// prompt and gets a recommendation, which a model's context bounds. A run
// over more is narrowed by process.
const MaxControls = 100

// Reads are the operations the agent reads with (ADR-0072, T105f): the
// control library and its tests (T98a), the matrix (T98b), the audit
// trail and the access data.
var Reads = []string{"listControls", "getControl", "listControlTests", "getControlMatrix", "listAuditEvents", "listSodRules",
	"listRoleAssignments", "listApprovalRequests"}

// Never are the operations the agent never calls (ADR-0072): recording a
// test, recording, changing or retiring a control, making or removing a
// link, every write of the control library, which its tools hold none of.
var Never = []string{"createControl", "updateControl", "retireControl", "createControlTest", "createControlLink", "removeControlLink"}

const system = `You monitor the tenant's controls and recommend, per control, whether you found an exception, for the
compliance officer to decide.

You are given each control of the run with its links to what enforces it in the product (a permission, a
segregation-of-duties rule, a setting of the configuration) as the control matrix shows them today: whether each link
holds and, when it is broken, why. You may read the matrix (getControlMatrix), the library and a control's tests
(listControls, getControl, listControlTests), the audit trail (listAuditEvents, by object_type and object_id: a rule's
events are object_type identity.sod_rule, a control's grc.control), the segregation-of-duties rules (listSodRules), the
role assignments (listRoleAssignments) and the approval requests (listApprovalRequests).

Then recommend once on each control. exception_found when a link of the control is broken, or when what you read shows
the control did not work (an event that breaks it). When the control has broken links, the content's first line is
"Broken links: " and the ids of exactly those links, as you are given them, joined by ", "; then why each is broken and
the audit events that show it (for a rule, the events of its creation or its end). no_exception when every link holds
and nothing you read shows an exception; a control with no link on which you found no audit event gets no_exception with
the sentence "` + NothingChecked + `" Cite an audit event as "audit event " and its seq, exactly as listAuditEvents
returned it, and cite only events you read. You record no test and change no control or link.`

// Agent monitors the tenant's controls.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
}

// link is a link of a control as the matrix judges it.
type link struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Holds        bool   `json:"holds"`
	BrokenReason string `json:"broken_reason"`
	Permission   string `json:"permission_code"`
	Path         string `json:"configuration_path"`
	Rule         string `json:"sod_rule_id"`
}

// target is what the link names.
func (l link) target() string { return l.Permission + l.Path + l.Rule }

// row is a control of the matrix with its links.
type row struct {
	Control struct {
		ID        string `json:"id"`
		Code      string `json:"code"`
		Objective string `json:"objective"`
		Process   string `json:"process"`
		Nature    string `json:"nature"`
		Frequency string `json:"frequency"`
		LastTest  *struct {
			Result   string `json:"result"`
			TestedOn string `json:"tested_on"`
		} `json:"last_test"`
	} `json:"control"`
	Links []link `json:"links"`
}

// broken are the ids of the row's links that do not hold, in the
// matrix's order.
func (r row) broken() []string {
	var out []string
	for _, l := range r.Links {
		if !l.Holds {
			out = append(out, l.ID)
		}
	}
	return out
}

// The refusals of the agent's own check, beside the pattern's.
var (
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("controlagent: every number in the content is one a tool returned or the run was given, written as returned; a count of your own is written in words")
	// ErrUncited refuses a citation of an audit event no read of the run
	// returned.
	ErrUncited = errors.New(`controlagent: cite only audit events a listAuditEvents of this run returned, as "audit event " and the seq of the event`)
	// ErrBroken refuses no_exception on a control with a broken link, and
	// an exception_found on one whose first line does not name exactly
	// its broken links.
	ErrBroken = errors.New(`controlagent: a control with broken links gets exception_found, whose first line is "Broken links: " and the ids of exactly those links, in the order given, joined by ", "`)
	// ErrNoEvidence refuses an exception_found on a control with no broken
	// link that cites no audit event.
	ErrNoEvidence = errors.New("controlagent: an exception on a control whose links all hold cites the audit events that show it")
	// ErrNothingChecked refuses a no_exception on a control with no link
	// and no audit event cited that does not say nothing could be
	// checked.
	ErrNothingChecked = errors.New(`controlagent: a control with no link on which no audit event was cited gets no_exception with the sentence "` + NothingChecked + `"`)
)

// Monitor runs one session over the tenant's active controls, those of
// the process when one is given: it reads the control matrix first (a
// matrix the agent may not read stops the run before a session starts,
// and so do more controls than MaxControls), offers the model the tools
// and reports what it recommended, each control named by its code, also
// when the run fails after recommending. With no control to monitor it
// starts no session and says so.
func (a *Agent) Monitor(ctx context.Context, process string) (recommend.Result, error) {
	rows, err := a.matrix(ctx, process)
	if err != nil {
		return recommend.Result{}, err
	}
	scope := "the tenant's active controls"
	if process != "" {
		scope = fmt.Sprintf("the active controls of the process %q", process)
	}
	if len(rows) == 0 {
		return recommend.Result{Reply: "No control to monitor: the control matrix has none of " + scope + "."}, nil
	}
	m := monitored{rows: map[string]row{}, cited: recommend.NewCited()}
	task := recommend.Task{
		Purpose: "Monitor " + scope,
		Objects: map[string]string{},
		Observe: map[string]func(apitools.Response){"listAuditEvents": m.cited.Keep(AuditEvent, "items[].seq")},
		Check:   m.check,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Monitor %s, %d as the control matrix gives them today:\n", scope, len(rows))
	names := map[string]string{}
	for _, r := range rows {
		m.rows[r.Control.ID] = r
		task.Objects[r.Control.ID] = ControlObject
		names[r.Control.ID] = r.Control.Code
		describe(&b, r)
	}
	task.Prompt = b.String()
	res, err := agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
	res.Names = names
	return res, err
}

// matrix reads the matrix's rows, every page, up to MaxControls.
func (a *Agent) matrix(ctx context.Context, process string) ([]row, error) {
	q := url.Values{}
	if process != "" {
		q.Set("process", process)
	}
	var rows []row
	err := apitools.Each(ctx, a.API, "/v1/control-matrix", q, func(r row) bool {
		rows = append(rows, r)
		return len(rows) <= MaxControls
	})
	if err != nil {
		return nil, fmt.Errorf("controlagent: the control matrix: %w", err)
	}
	if len(rows) > MaxControls {
		return nil, fmt.Errorf("controlagent: more than %d controls to monitor: name a process", MaxControls)
	}
	return rows, nil
}

// describe writes a control and its links as the prompt gives them.
func describe(b *strings.Builder, r row) {
	c := r.Control
	last := "never tested"
	if c.LastTest != nil {
		last = fmt.Sprintf("last tested %s, %s", c.LastTest.TestedOn, c.LastTest.Result)
	}
	fmt.Fprintf(b, "Control %s (id %s), objective %q, process %q, %s, %s, %s.", c.Code, c.ID, c.Objective, c.Process, c.Nature, c.Frequency, last)
	if len(r.Links) == 0 {
		b.WriteString(" No link.\n")
		return
	}
	for _, l := range r.Links {
		state := "holds"
		if !l.Holds {
			state = "broken: " + l.BrokenReason
		}
		fmt.Fprintf(b, " Link %s to the %s %s: %s.", l.ID, l.Kind, l.target(), state)
	}
	b.WriteString("\n")
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{ControlObject: {ExceptionFound, NoException}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// monitored is what a run's check holds its recommendations to: the
// matrix's rows read before the session, by control id, and the audit
// events the run's reads returned.
type monitored struct {
	rows  map[string]row
	cited *recommend.Cited
}

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, T105f): every number is one the run read and every audit
// event cited one a read returned; a control with broken links gets
// exception_found naming exactly them on the first line; an exception
// on a control whose links hold cites the events that show it; and a
// no_exception on a control with no link and no event cited says nothing
// could be checked.
func (m monitored) check(p recommend.Proposed, f *recommend.Figures) error {
	if unread := f.Unread(p.Content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	cites := recommend.Cites(p.Content, AuditEvent)
	if unknown := m.cited.Unknown(cites); len(unknown) > 0 {
		return fmt.Errorf("%w: not %s", ErrUncited, unknown[0])
	}
	r := m.rows[p.ObjectID]
	broken := r.broken()
	first, _, _ := strings.Cut(p.Content, "\n")
	switch {
	case len(broken) > 0 && (p.Kind != ExceptionFound || strings.TrimSpace(first) != "Broken links: "+strings.Join(broken, ", ")):
		return ErrBroken
	case len(broken) == 0 && p.Kind == ExceptionFound && len(cites) == 0:
		return ErrNoEvidence
	case len(r.Links) == 0 && p.Kind == NoException && len(cites) == 0 && !strings.Contains(p.Content, NothingChecked):
		return ErrNothingChecked
	}
	return nil
}
