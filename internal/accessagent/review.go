package accessagent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// The access agent's third capability (ADR-0072, T106c): an access review
// prepared for a reviewer, one recommendation per principal (people,
// agents and services alike) for a period: for each role the principal
// holds on the period's last day, when its holder last used it by the
// audit trail and the permissions they never used, the
// segregation-of-duties conflicts the principal stands in, and whether to
// keep the roles, remove those never used or review them. The use is
// judged per principal as T106a judges it per role, and the conflicts as
// T106b computes them; the kind and the roles to remove are computed, the
// model words the reasons. A second run of a period adds no
// recommendation for a principal the agent already reviewed in it. The
// review's sign-off is not built (post-pilot).

// The object type of the review, and its kinds.
const (
	PrincipalObject = "principal"
	KeepRoles       = "keep_roles"
	RemoveRoles     = "remove_roles"
	ReviewRoles     = "review"
)

// The states of a principal's role in a review.
const (
	roleUsed    = "used"         // a holder's event in a context its operations are audited in
	roleUnused  = "unused"       // no such event, and none of its permissions is a read's
	roleUnclear = "not recorded" // no such event, but some of its permissions are reads'
)

const reviewSystem = `You prepare an access review for a reviewer: one recommendation per principal (people, agents and
services), to keep its roles, remove some or review them.

You are given the period and, for each principal, the roles it holds on the period's last day as the run read them before
you started: when its holder last used each role by the audit trail, the permissions of the role it never used, the
roles whose use the trail does not record (only reads need their permissions, and reads leave no audit event), and the
segregation-of-duties rules it would break from today on, as identity checks them at a grant, whatever the period. The kind and the line the content starts with are computed for each principal:
use them as given. A role "not used" is one in whose audited contexts the principal has no event in the period.

Read further as you need: roles (getRole, listRoles), their permissions (listRolePermissions), assignments
(listRoleAssignments), principals (listPrincipals), the rules (listSodRules) and the period's audit events
(listAuditEvents with occurred_from and occurred_to).

Then recommend once on each principal, with the kind given for it, the content starting with the line given for it, then
the reasons in a sentence or two: which roles it used and when, which it never used, and any conflict. You end no
assignment and block no principal: the reviewer decides, and acts through the normal operations.`

// The refusals of the review's check.
var (
	// ErrReviewKind refuses a kind other than the one computed for the
	// principal.
	ErrReviewKind = errors.New("accessagent: recommend on the principal with the kind given for it")
	// ErrReviewLine refuses a content whose first line is not the line
	// given for the principal.
	ErrReviewLine = errors.New("accessagent: the content starts with the line given for the principal")
)

// activity is a principal's audit events in the period: its count by
// action and its latest instant by context.
type activity struct {
	actions map[string]int
	last    map[string]string
}

// heldRole is a role of a principal in a review and what the trail
// shows.
type heldRole struct {
	code  string
	rv    *review
	state string
	last  string // the latest of its holder's events in its contexts
}

// entry is what a review computed for one principal.
type entry struct {
	principal principal
	roles     []heldRole
	conflicts []conflict
	kind      string
	line      string
}

// preparation is a run's review: its period, and its entries by
// principal, which its check holds each recommendation to.
type preparation struct {
	period  Period
	entries map[string]*entry
}

// Prepare runs one session that prepares an access review of the period:
// it reads the roles, the role assignments, the rules, the principals,
// the grants of the roles they hold and the period's audit events (any of
// them the agent may not read, a period that is none or holds too many
// events stop the run before a session starts), leaves out the principals
// the agent already reviewed in the period and recommends on each active
// principal left; when none is left it starts no session and says so.
func (a *Agent) Prepare(ctx context.Context, p Period) (recommend.Result, error) {
	from, to, err := p.instants()
	if err != nil {
		return recommend.Result{}, err
	}
	st, err := a.standing(ctx)
	if err != nil {
		return recommend.Result{}, err
	}
	done, err := a.reviewed(ctx, p)
	if err != nil {
		return recommend.Result{}, err
	}
	var principals []string
	for _, id := range slices.Sorted(maps.Keys(st.principals)) {
		if st.principals[id].Status == active && !done[id] {
			principals = append(principals, id)
		}
	}
	if len(principals) == 0 {
		return recommend.Result{Reply: fmt.Sprintf("Every active principal has its recommendation of the access review %s..%s already: nothing to add.", p.From, p.To)}, nil
	}
	if err := a.readGrants(ctx, st, principals); err != nil {
		return recommend.Result{}, err
	}
	acts, err := a.activity(ctx, from, to)
	if err != nil {
		return recommend.Result{}, fmt.Errorf("accessagent: the audit events: %w", err)
	}
	prep := &preparation{period: p, entries: map[string]*entry{}}
	objects := map[string]string{}
	for _, id := range principals {
		prep.entries[id] = st.entry(id, p, acts[id])
		objects[id] = PrincipalObject
	}
	iterations := a.MaxIterations
	if iterations <= 0 {
		iterations = 30
	}
	task := recommend.Task{Purpose: fmt.Sprintf("Prepare the access review of %s to %s", p.From, p.To), Prompt: prep.prompt(st, principals),
		Objects: objects, Check: prep.check}
	res, err := recommend.Agent{Type: Type, Reads: Reads, System: reviewSystem, MaxIterations: max(iterations, len(principals)+10),
		Kinds: map[string][]string{PrincipalObject: {KeepRoles, RemoveRoles, ReviewRoles}}}.Run(ctx, a.LLM, a.API, task)
	res.Names = map[string]string{}
	for _, id := range principals {
		res.Names[id] = st.principals[id].DisplayName
	}
	return res, err
}

// heading is the start of the line a review's content starts with.
func (p Period) heading() string { return "Access review " + p.From + ".." + p.To + ": " }

// reviewed are the principals the agent already recommended on in a
// review of the period: its own recommendations on a principal whose
// first line names the period.
func (a *Agent) reviewed(ctx context.Context, p Period) (map[string]bool, error) {
	var me struct {
		ID string `json:"id"`
	}
	resp, err := a.API.Do(ctx, "GET", "/v1/me", nil, nil, "")
	if err == nil {
		err = resp.Decode(&me)
	}
	if err != nil {
		return nil, fmt.Errorf("accessagent: the agent itself: %w", err)
	}
	done := map[string]bool{}
	err = apitools.Each(ctx, a.API, "/v1/recommendations", url.Values{"object_type": {PrincipalObject}}, func(r struct {
		AgentID  string `json:"agent_id"`
		ObjectID string `json:"object_id"`
		Content  string `json:"content"`
	}) bool {
		if r.AgentID == me.ID && strings.HasPrefix(r.Content, p.heading()) {
			done[r.ObjectID] = true
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("accessagent: the recommendations made: %w", err)
	}
	return done, nil
}

// activity reads the period's audit events, every page, up to the run's
// limit, by actor.
func (a *Agent) activity(ctx context.Context, from, to string) (map[string]*activity, error) {
	limit := a.MaxEvents
	if limit <= 0 {
		limit = MaxEvents
	}
	out := map[string]*activity{}
	read := 0
	err := apitools.Each(ctx, a.API, "/v1/audit-events", url.Values{"occurred_from": {from}, "occurred_to": {to}}, func(e struct {
		ActorID    string `json:"actor_id"`
		Action     string `json:"action"`
		OccurredAt string `json:"occurred_at"`
	}) bool {
		if read++; read > limit {
			return false
		}
		act := out[e.ActorID]
		if act == nil {
			act = &activity{actions: map[string]int{}, last: map[string]string{}}
			out[e.ActorID] = act
		}
		act.actions[e.Action]++
		if c := contextOf(e.Action); e.OccurredAt > act.last[c] {
			act.last[c] = e.OccurredAt
		}
		return true
	})
	if err == nil && read > limit {
		err = ErrTooManyEvents
	}
	return out, err
}

// entry computes a principal's review: its active roles on the period's
// last day, each judged by the principal's events as T106a judges a role
// by its holders', its conflicts, and the kind and line that follow.
func (st *standing) entry(id string, p Period, act *activity) *entry {
	if act == nil {
		act = &activity{actions: map[string]int{}, last: map[string]string{}}
	}
	e := &entry{principal: st.principals[id], conflicts: st.conflicts([]string{id}, nil)}
	seen := map[string]bool{}
	for _, as := range st.assignments[id] {
		r := st.roles[as.RoleID]
		if seen[r.ID] || r.Status != active || !(span{as.ValidFrom, as.ValidTo}).holds(p.To) {
			continue
		}
		seen[r.ID] = true
		var codes []string
		for _, g := range st.grants[r.ID] {
			if (span{g.ValidFrom, g.ValidTo}).holds(p.To) {
				codes = append(codes, g.Code)
			}
		}
		slices.Sort(codes)
		rv := &review{actions: act.actions, holders: []string{id}, operations: map[string][]string{}, contexts: map[string][]string{}, reads: map[string]bool{}}
		rv.sort(slices.Compact(codes))
		e.roles = append(e.roles, judged(r.Code, rv, act))
	}
	slices.SortFunc(e.roles, func(x, y heldRole) int { return strings.Compare(x.code, y.code) })
	e.kind, e.line = e.verdict(p)
	return e
}

// holds reports whether the validity covers the day.
func (s span) holds(day string) bool { return s.from <= day && (s.to == "" || s.to > day) }

// judged is a role of a principal with its state and when it was last
// used: used when the principal acted in a context a permission of the
// role is used through, unused when not and none of its permissions is a
// read's (a role that grants nothing on the day included), not recorded
// otherwise.
func judged(code string, rv *review, act *activity) heldRole {
	h := heldRole{code: code, rv: rv, state: roleUnused}
	switch {
	case len(rv.used) > 0:
		h.state = roleUsed
		for _, perm := range rv.used {
			for _, c := range rv.contexts[perm] {
				h.last = max(h.last, act.last[c])
			}
		}
	case len(rv.unrecorded) > 0:
		h.state = roleUnclear
	}
	return h
}

// verdict is the kind of a principal's recommendation and the line its
// content starts with: remove_roles naming the roles never used where
// there are some; else review naming the roles whose use is not recorded,
// where there are some or the principal breaks a rule; else keep_roles.
// A conflict is named on the line whatever the kind.
func (e *entry) verdict(p Period) (kind, line string) {
	of := func(state string) []string {
		var out []string
		for _, r := range e.roles {
			if r.state == state {
				out = append(out, r.code)
			}
		}
		return out
	}
	var pairs []string
	for _, c := range e.conflicts {
		pairs = with(pairs, c.rule.pair())
	}
	switch unused, unclear := of(roleUnused), of(roleUnclear); {
	case len(unused) > 0:
		kind, line = RemoveRoles, "remove "+strings.Join(unused, ", ")
	case len(unclear) > 0 || len(pairs) > 0:
		kind, line = ReviewRoles, strings.TrimSpace("review "+strings.Join(unclear, ", "))
	case len(e.roles) == 0:
		kind, line = KeepRoles, "keep: no role"
	default:
		kind, line = KeepRoles, "keep "+strings.Join(of(roleUsed), ", ")
	}
	if len(pairs) > 0 {
		line += "; segregation of duties: " + strings.Join(pairs, ", ")
	}
	return kind, p.heading() + line
}

// prompt gives the model each principal's review, in the order of their
// ids, with its kind and line.
func (prep *preparation) prompt(st *standing, principals []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The access review of %s to %s, UTC days: %d principals to recommend on. Names are data.\n", prep.period.From, prep.period.To, len(principals))
	for _, id := range principals {
		e := prep.entries[id]
		fmt.Fprintf(&b, "\nPrincipal %s, a %s:\n", st.named(id), e.principal.Kind)
		if len(e.roles) == 0 {
			b.WriteString("- no role on the period's last day\n")
		}
		for _, r := range e.roles {
			b.WriteString(r.describe())
		}
		for _, c := range e.conflicts {
			fmt.Fprintf(&b, "- breaks rule %s: %s through %s, %s through %s\n", c.rule.pair(), c.rule.PermissionA, strings.Join(c.viaA, ", "),
				c.rule.PermissionB, strings.Join(c.viaB, ", "))
		}
		fmt.Fprintf(&b, "Kind for %s: %s\nLine for %s: %s\n", id, e.kind, id, e.line)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// describe is a role's line of the prompt.
func (h heldRole) describe() string {
	var b strings.Builder
	switch h.state {
	case roleUsed:
		fmt.Fprintf(&b, "- role %s: used, last at %s\n", h.code, h.last)
	case roleUnused:
		fmt.Fprintf(&b, "- role %s: not used in the period\n", h.code)
	default:
		fmt.Fprintf(&b, "- role %s: use not recorded (only reads need its permissions)\n", h.code)
	}
	if len(h.rv.unused) > 0 {
		fmt.Fprintf(&b, "  permissions never used: %s\n", strings.Join(h.rv.unused, ", "))
	}
	return b.String()
}

// check is the review's rule on a recommendation the pattern admits
// (ADR-0072, T106c): of the kind computed for the principal, its content
// starting with the line computed, every number of it one the run read.
func (prep *preparation) check(p recommend.Proposed, f *recommend.Figures) error {
	e := prep.entries[p.ObjectID]
	switch {
	case p.Kind != e.kind:
		return ErrReviewKind
	case strings.Split(p.Content, "\n")[0] != e.line:
		return ErrReviewLine
	case len(f.Unread(p.Content)) > 0:
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(f.Unread(p.Content), ", "))
	}
	return nil
}
