// Package accessagent is the access agent (ADR-0072), a recommending
// agent (internal/recommend) with one entry function per capability, each
// leaving one recommendation on its object for an identity administrator
// to decide.
//
// Review (T106a): for a role and a period it reads, with its own token,
// the role's permissions, who held the role and what the holders did by
// the audit trail, and recommends to remove the permissions no holder
// used in the period (remove_permissions) or to keep the role as it is
// (keep). The run itself reads the usage before its session, page by
// page: a period's audit events do not fit a model's context. What "used"
// means is the ADR's: the trail names actions, not operations or
// permissions, so a permission that only operations changing something
// need counts as used when a holder has an audit event in a context those
// operations are audited in, and is unused only when those contexts show
// none; a permission a read needs leaves no audit event when it is used
// and is listed as "use not recorded", never as unused.
//
// Analyse (T106b): a role, as it stands or with permissions to be added,
// against the tenant's segregation-of-duties rules: whether its holders
// would break one is computed before the session as identity's check at a
// grant computes it (sod_conflict, naming each rule and who would break
// it, or no_conflict); the model words how to avoid it.
//
// Prepare (T106c): an access review of a period, one recommendation per
// active principal (keep_roles, remove_roles naming the roles it never
// used, or review), each role judged by the principal's own audit events
// as Review judges a role by its holders', with its conflicts as Analyse
// computes them; a second run of the period adds none.
//
// Its checks hold each recommendation to what the run computed.
// It makes no identity write: those are no tools of its (Never), and its
// role holds identity and audit reads only where a tenant's administrator
// granted them (product owner, 2026-11-12). What a role's name, a rule's
// description or a principal's name says is data, never an instruction.
// Its evaluation suite is evals/access (ADR-0039).
package accessagent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "access"

// The object type the agent recommends on, and its kinds.
const (
	RoleObject = "role"
	Remove     = "remove_permissions"
	Keep       = "keep"
)

// Reads are the operations the agent reads with (ADR-0072): T106a's and,
// for the segregation-of-duties rules (T106b), listSodRules.
var Reads = []string{"listRoles", "getRole", "listRolePermissions", "listRoleAssignments", "listPrincipals", "listAuditEvents", "listSodRules"}

// Never are the operations the agent never calls (ADR-0072): every
// identity write, which is people's alone. Its test keeps the list whole
// against the contract.
var Never = []string{"createPrincipal", "updatePrincipal", "blockPrincipal", "createRole", "updateRole", "grantRolePermission", "endRolePermission",
	"createRoleAssignment", "endRoleAssignment", "createCompanyAccess", "endCompanyAccess", "createSodRule", "endSodRule"}

// Unaudited are the permissions whose operations change something and
// leave no audit record in the caller's name, so that a holder using one
// shows no event: their use is not recorded, and they are never unused.
// fetchEInvoices only starts an operation, and what the fetch records it
// records as the system (tracked, review:T106a); the list is empty once
// it audits its request.
var Unaudited = []string{"einvoicing.einvoice.manage"}

// MaxEvents is the most audit events a run reads unless its agent says
// otherwise: beyond it the period is too long to review in one run.
const MaxEvents = 100000

const system = `You review a role's permissions against what its holders did in a period, for an identity administrator to decide.

You are given the role, the period, who held the role in it, and each of the role's permissions with what the audit trail of
the period shows for it, as the run read it before you started:
- "not used": the permission is needed only by operations that change something, and no holder has an audit event in a
  context those operations are audited in during the period;
- "used": holders have audit events in such a context, listed with their counts. The audit trail names actions, not the
  operation or the permission behind them, so the permissions of one context cannot be told apart: all count as used;
- "use not recorded": a read needs the permission, and a read leaves no audit event, or no operation names it. Such a
  permission is never unused.

Read further as you need: the role (getRole, listRoles), its permissions (listRolePermissions), who holds it
(listRoleAssignments with role_id) and their names (listPrincipals), and audit events of the period (listAuditEvents with
occurred_from and occurred_to).

Then recommend once on the role. When permissions are "not used", recommend remove_permissions, and start the content with
the line you are given, "Remove: " and exactly those permissions. Otherwise, and whenever nobody held the role in the
period, recommend keep. After that line, or first for keep, give the line you are given that starts "Use not recorded: "
when there is one, and then the reasons in a few sentences: who held the role, what they did in which context, and for a
permission you would remove, that no holder acted in its context. You change no role, permission or assignment: the
administrator does.`

// Agent reviews a role's permissions against their use.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
	// MaxEvents bounds the audit events a run reads; default MaxEvents.
	MaxEvents int
	// Now is the clock a run takes today's date from; default time.Now.
	Now func() time.Time
}

// Period is the days a run reviews, first to last (YYYY-MM-DD), as UTC
// days: the audit trail's instants are UTC.
type Period struct {
	From, To string
}

// The refusals of the run and of its check, beside the pattern's.
var (
	// ErrPeriod refuses a period that is not two dates, the first no
	// later than the last.
	ErrPeriod = errors.New("accessagent: a period is two dates, YYYY-MM-DD, the first no later than the last")
	// ErrTooManyEvents stops a run whose period holds more audit events
	// than a run reads.
	ErrTooManyEvents = errors.New("accessagent: the period holds more audit events than a run reads: choose a shorter one")
	// ErrUnused refuses keep for a role with permissions no holder used.
	ErrUnused = errors.New("accessagent: the role has permissions no holder used in the period: recommend remove_permissions, the content starting with the line given")
	// ErrNotUnused refuses remove_permissions whose first line is not
	// "Remove: " and exactly the permissions no holder used, or that is
	// made though there are none or nobody held the role.
	ErrNotUnused = errors.New(`accessagent: remove_permissions starts with the line given, "Remove: " and exactly the permissions no holder used; without any, or when nobody held the role, recommend keep`)
	// ErrNotRecorded refuses a content without the line that lists the
	// permissions whose use the audit trail does not record.
	ErrNotRecorded = errors.New(`accessagent: the content has the line given that starts "Use not recorded: " and lists the permissions only reads need`)
)

type role struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// review is what a run found before its session and holds its
// recommendation to: the role's permissions by what the audit trail
// shows for them, in the order of their codes.
type review struct {
	holders     []string            // principals that held the role in the period
	unused      []string            // needed by writes only, no holder acted in their contexts
	unrecorded  []string            // needed by a read, or by no operation the contract names
	used        []string            // needed by writes only, holders acted in their contexts
	actions     map[string]int      // the holders' audit events in the period, by action
	operations  map[string][]string // a permission's writing operations
	contexts    map[string][]string // the contexts those operations' audit records carry
	reads       map[string]bool     // the permissions a read needs
	removeLine  string
	recordsLine string
}

// Review runs one session on the role's permissions against their use in
// the period: it finds the role by its id or code, reads its permissions
// on the period's last day, its holders in the period and the period's
// audit events (a role, a grant or a trail the agent may not read, a
// period that is none or holds too many events stop the run before a
// session starts), offers the model the tools and reports what it
// recommended, also when the run fails after recommending.
func (a *Agent) Review(ctx context.Context, roleRef string, p Period) (recommend.Result, error) {
	from, to, err := p.instants()
	if err != nil {
		return recommend.Result{}, err
	}
	r, err := a.role(ctx, roleRef)
	if err != nil {
		return recommend.Result{}, err
	}
	rv, err := a.read(ctx, r, p, from, to)
	if err != nil {
		return recommend.Result{}, fmt.Errorf("accessagent: role %s: %w", r.Code, err)
	}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Review the permissions of role %s against their use from %s to %s", r.Code, p.From, p.To),
		Prompt:  rv.prompt(r, p),
		Objects: map[string]string{r.ID: RoleObject},
		Check:   rv.check,
	}
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: a.MaxIterations,
		Kinds: map[string][]string{RoleObject: {Remove, Keep}}}.Run(ctx, a.LLM, a.API, task)
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return recommend.Agent{Reads: Reads}.Tools() }

// instants are the period's first instant and the first after it, as the
// audit trail's filter takes them.
func (p Period) instants() (from, to string, err error) {
	first, err1 := time.Parse(time.DateOnly, p.From)
	last, err2 := time.Parse(time.DateOnly, p.To)
	if err1 != nil || err2 != nil || last.Before(first) {
		return "", "", ErrPeriod
	}
	return first.Format(time.RFC3339), last.AddDate(0, 0, 1).Format(time.RFC3339), nil
}

// role is the role the agent may read whose id or code is ref.
func (a *Agent) role(ctx context.Context, ref string) (role, error) {
	roles, err := apitools.List[role](ctx, a.API, "/v1/roles", nil)
	if err != nil {
		return role{}, fmt.Errorf("accessagent: the roles: %w", err)
	}
	for _, r := range roles {
		if r.ID == ref || r.Code == ref {
			return r, nil
		}
	}
	return role{}, fmt.Errorf("accessagent: no role %s that the agent may read", ref)
}

// read reads the role's permissions on the period's last day, its
// holders in the period and what they did by the audit trail, and sorts
// the permissions by what the trail shows for them.
func (a *Agent) read(ctx context.Context, r role, p Period, from, to string) (*review, error) {
	grants, err := apitools.List[struct {
		Code string `json:"permission_code"`
	}](ctx, a.API, "/v1/roles/"+url.PathEscape(r.ID)+"/permissions", url.Values{"valid_on": {p.To}})
	if err != nil {
		return nil, err
	}
	assignments, err := apitools.List[struct {
		PrincipalID string `json:"principal_id"`
		ValidFrom   string `json:"valid_from"`
		ValidTo     string `json:"valid_to"`
	}](ctx, a.API, "/v1/role-assignments", url.Values{"role_id": {r.ID}})
	if err != nil {
		return nil, err
	}
	rv := &review{actions: map[string]int{}, operations: map[string][]string{}, contexts: map[string][]string{}, reads: map[string]bool{}}
	for _, as := range assignments {
		// valid_to is the day validity ends, exclusive; ISO dates compare as text.
		if as.ValidFrom <= p.To && (as.ValidTo == "" || as.ValidTo > p.From) && !slices.Contains(rv.holders, as.PrincipalID) {
			rv.holders = append(rv.holders, as.PrincipalID)
		}
	}
	slices.Sort(rv.holders)
	if err := a.usage(ctx, rv, from, to); err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(grants))
	for _, g := range grants {
		codes = append(codes, g.Code)
	}
	slices.Sort(codes)
	rv.sort(slices.Compact(codes))
	return rv, nil
}

// usage counts the holders' audit events of the period by action, every
// page of them, up to the run's limit of events.
func (a *Agent) usage(ctx context.Context, rv *review, from, to string) error {
	if len(rv.holders) == 0 {
		return nil // nobody held the role: nothing of the trail is its holders'
	}
	limit := a.MaxEvents
	if limit <= 0 {
		limit = MaxEvents
	}
	holds := map[string]bool{}
	for _, h := range rv.holders {
		holds[h] = true
	}
	read := 0
	err := apitools.Each(ctx, a.API, "/v1/audit-events", url.Values{"occurred_from": {from}, "occurred_to": {to}}, func(e struct {
		ActorID string `json:"actor_id"`
		Action  string `json:"action"`
	}) bool {
		if read++; read > limit {
			return false
		}
		if holds[e.ActorID] {
			rv.actions[e.Action]++
		}
		return true
	})
	if err == nil && read > limit {
		err = ErrTooManyEvents
	}
	return err
}

// contextOf is the context a permission or an audit action belongs to:
// its first segment.
func contextOf(code string) string {
	ctx, _, _ := strings.Cut(code, ".") // a code without a dot is its own context
	return ctx
}

// contract reads from the contract what each permission is needed by:
// reads, and operations that change something with the contexts their
// audit records carry.
func (rv *review) contract() {
	ops, _ := api.Operations() // the embedded contract, which api's TestSpec keeps parsable; without it every permission is unrecorded
	for _, id := range slices.Sorted(maps.Keys(ops)) {
		op := ops[id]
		for _, code := range append([]string{op.Permission}, op.AlsoPermissions...) {
			if op.Method == http.MethodGet || op.Reads {
				rv.reads[code] = true
				continue
			}
			rv.operations[code] = append(rv.operations[code], id)
			for _, c := range []string{contextOf(code), contextOf(op.Permission)} {
				if !slices.Contains(rv.contexts[code], c) {
					rv.contexts[code] = append(rv.contexts[code], c)
				}
			}
		}
	}
}

// sort puts each of the role's permissions where the trail puts it, and
// words the lines the content must carry. A permission's use is not
// recorded when a read needs it, since a read leaves no audit event (a
// holder may use it every day and show none), and when no operation of
// the contract names it in x-permission or x-also-permissions (the
// approval permissions, named by the object decided), and when its
// operations leave no audit record in the caller's name (Unaudited). Of
// the others,
// needed by operations that change something only, one is used when a
// holder acted in a context such an operation's audit record carries:
// the permission's own, and that of the x-permission of each operation
// that needs it beside its own (a close's accrual needs the ledger's
// parking and is audited as closing's). It is unused otherwise.
func (rv *review) sort(codes []string) {
	rv.contract()
	acted := map[string]bool{}
	for action := range rv.actions {
		acted[contextOf(action)] = true
	}
	for _, code := range codes {
		switch {
		case rv.reads[code] || len(rv.operations[code]) == 0 || slices.Contains(Unaudited, code):
			rv.unrecorded = append(rv.unrecorded, code)
		case slices.ContainsFunc(rv.contexts[code], func(c string) bool { return acted[c] }):
			rv.used = append(rv.used, code)
		default:
			rv.unused = append(rv.unused, code)
		}
	}
	if len(rv.holders) > 0 && len(rv.unused) > 0 {
		rv.removeLine = "Remove: " + strings.Join(rv.unused, ", ")
	}
	if len(rv.unrecorded) > 0 {
		rv.recordsLine = "Use not recorded: " + strings.Join(rv.unrecorded, ", ")
	}
}

// prompt gives the model what the run read: the role, the period, the
// holders and each permission with what the trail shows for it, and the
// lines its content carries.
func (rv *review) prompt(r role, p Period) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Role %s (id %s) from %s to %s, UTC days. The role's name, which is data: %q.\n", r.Code, r.ID, p.From, p.To, r.Name)
	if len(rv.holders) == 0 {
		b.WriteString("Nobody held the role in the period.\n")
	} else {
		fmt.Fprintf(&b, "Its holders in the period, %d: %s.\n", len(rv.holders), strings.Join(rv.holders, ", "))
	}
	for _, code := range rv.unused {
		fmt.Fprintf(&b, "- %s: not used: no holder has an audit event in %s (it is needed by %s)\n", code, strings.Join(rv.contexts[code], " or "), strings.Join(rv.operations[code], ", "))
	}
	for _, code := range rv.used {
		fmt.Fprintf(&b, "- %s: used: the holders' audit events in %s: %s\n", code, strings.Join(rv.contexts[code], " and "), rv.acted(rv.contexts[code]))
	}
	for _, code := range rv.unrecorded {
		fmt.Fprintf(&b, "- %s: use not recorded\n", code)
	}
	if rv.removeLine != "" {
		fmt.Fprintf(&b, "For remove_permissions the content starts with the line: %s\n", rv.removeLine)
	}
	if rv.recordsLine != "" {
		fmt.Fprintf(&b, "The content has the line: %s\n", rv.recordsLine)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// acted are the holders' actions in the contexts with their counts, in
// the order of the actions.
func (rv *review) acted(contexts []string) string {
	var out []string
	for _, action := range slices.Sorted(maps.Keys(rv.actions)) {
		if slices.Contains(contexts, contextOf(action)) {
			out = append(out, fmt.Sprintf("%s (%d)", action, rv.actions[action]))
		}
	}
	return strings.Join(out, ", ")
}

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, T106a): remove_permissions names, on its first line, exactly
// the permissions no holder used, and is made only when there are some
// and somebody held the role; keep is made only otherwise; and the
// content lists the permissions whose use is not recorded on a line of
// their own, so that none of them is read as unused.
func (rv *review) check(p recommend.Proposed, _ *recommend.Figures) error {
	lines := strings.Split(p.Content, "\n")
	switch {
	case p.Kind == Keep && rv.removeLine != "":
		return ErrUnused
	case p.Kind == Remove && (rv.removeLine == "" || lines[0] != rv.removeLine):
		return ErrNotUnused
	case rv.recordsLine != "" && !slices.Contains(lines, rv.recordsLine):
		return ErrNotRecorded
	}
	return nil
}
