package accessagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// The access agent's second capability (ADR-0072, T106b): a role
// analysed against the tenant's segregation-of-duties rules, as it
// stands or with permissions somebody thinks of adding, started by a
// person. Whether its holders would break a rule is computed before the
// session, as the API's check at a grant computes it (standing.go); the
// model words the recommendation and how to avoid the conflict.

// The kinds of the analysis, on a role.
const (
	SodConflict = "sod_conflict"
	NoConflict  = "no_conflict"
)

const sodSystem = `You analyse a role against the tenant's segregation-of-duties rules, for an identity administrator to decide.

You are given the role as it stands today, or with permissions somebody thinks of adding to it, the principals assigned the
role, the tenant's rules, and the conflicts the run computed before you started, as the API's own check finds them when a
permission is granted: a principal breaks a rule when, on some day from today on that the rule is in force, it holds both of
the rule's permissions, each through an assignment of an active role and a grant of that role valid that day. The two may
come through this role alone, or one of them through another role of the principal. Whether there is a conflict is not yours
to judge: the run computed it.

Read further as you need: the role (getRole, listRoles), its permissions (listRolePermissions), who is assigned it and what
else they are assigned (listRoleAssignments with role_id or principal_id), their names (listPrincipals) and the rules
(listSodRules).

Then recommend once on the role. When conflicts are listed, recommend sod_conflict: the content has, each on a line of its
own, the lines you are given, one per rule broken with the principals who would break it; after them say in a few sentences
through which roles each would hold the two permissions, and how to avoid it: which permission to leave out of the role, or
to put in a role of its own that those principals are not given (a split of the role). When none is listed, recommend
no_conflict and say what was checked. You grant no permission and change no rule, role or assignment: the administrator
does.`

// The refusals of the analysis' check.
var (
	// ErrConflict refuses no_conflict for a role whose holders would break
	// a rule.
	ErrConflict = errors.New("accessagent: principals assigned the role would break segregation-of-duties rules: recommend sod_conflict, the content with the lines given")
	// ErrNoConflict refuses sod_conflict where the run computed none.
	ErrNoConflict = errors.New("accessagent: no principal assigned the role would break a segregation-of-duties rule: recommend no_conflict")
	// ErrConflictLines refuses a sod_conflict without the lines that name
	// each rule broken and who would break it.
	ErrConflictLines = errors.New("accessagent: sod_conflict has, each on a line of its own, the lines given: one per rule broken, with the principals who would break it")
	// ErrUnread refuses a content with a number the run did not read
	// (ADR-0072 §4): a count the agent made itself is written in words.
	ErrUnread = errors.New("accessagent: every number of the content is one the run read; write a count you made in words")
)

// analysis is what a run computed before its session and holds its
// recommendation to: the conflicts, and the lines that name them.
type analysis struct {
	conflicts []conflict
	lines     []string
}

// Analyse runs one session on the role against the tenant's
// segregation-of-duties rules, as it stands or with the permissions
// added from today: it reads the roles, the role assignments, the rules,
// the principals and the grants of every role its holders are assigned
// (any of them the agent may not read stops the run before a session
// starts), computes the conflicts, offers the model the tools and
// reports what it recommended, also when the run fails after
// recommending.
func (a *Agent) Analyse(ctx context.Context, roleRef string, add []string) (recommend.Result, error) {
	st, err := a.standing(ctx)
	if err != nil {
		return recommend.Result{}, err
	}
	r, err := st.find(roleRef)
	if err != nil {
		return recommend.Result{}, err
	}
	holders := st.holders(r.ID)
	if err := a.readGrants(ctx, st, holders); err != nil {
		return recommend.Result{}, err
	}
	add = slices.DeleteFunc(slices.Clone(add), func(code string) bool { return code == "" })
	slices.Sort(add)
	add = slices.Compact(add)
	added := map[string][]grant{}
	for _, code := range add {
		added[r.ID] = append(added[r.ID], grant{Code: code, ValidFrom: st.today})
	}
	an := &analysis{conflicts: st.conflicts(holders, added)}
	an.lines = st.lines(an.conflicts)
	purpose := fmt.Sprintf("Analyse role %s against the segregation-of-duties rules", r.Code)
	if len(add) > 0 {
		purpose += ", with " + strings.Join(add, ", ") + " added"
	}
	task := recommend.Task{Purpose: purpose, Prompt: an.prompt(st, r, holders, add), Objects: map[string]string{r.ID: RoleObject}, Check: an.check}
	return recommend.Agent{Type: Type, Reads: Reads, System: sodSystem, MaxIterations: a.MaxIterations,
		Kinds: map[string][]string{RoleObject: {SodConflict, NoConflict}}}.Run(ctx, a.LLM, a.API, task)
}

// lines name each rule broken, by its pair, and the principals who break
// it, in the order of the pairs.
func (st *standing) lines(conflicts []conflict) []string {
	breakers := map[string][]string{}
	for _, c := range conflicts {
		breakers[c.rule.pair()] = with(breakers[c.rule.pair()], st.named(c.principal))
	}
	var out []string
	for _, r := range st.rules { // in the order of their pairs
		if who, broken := breakers[r.pair()]; broken {
			out = append(out, "Rule "+r.pair()+": "+strings.Join(who, ", "))
			delete(breakers, r.pair())
		}
	}
	return out
}

// prompt gives the model what the run read and computed: the role, what
// was asked, its holders, the rules, the conflicts, and the lines its
// content carries.
func (an *analysis) prompt(st *standing, r role, holders, add []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Role %s (id %s), %s, on %s (UTC). The role's name, which is data: %q.\n", r.Code, r.ID, r.Status, st.today, r.Name)
	if len(add) > 0 {
		fmt.Fprintf(&b, "Asked: the role with these permissions granted to it from today: %s.\n", strings.Join(add, ", "))
	} else {
		b.WriteString("Asked: the role as it stands.\n")
	}
	if r.Status != active {
		b.WriteString("The role is not active: its permissions reach nobody, and nothing granted to it collides with a rule while it stays so.\n")
	}
	if len(holders) == 0 {
		b.WriteString("Nobody is assigned the role.\n")
	} else {
		who := make([]string, 0, len(holders))
		for _, h := range holders {
			who = append(who, st.named(h))
		}
		fmt.Fprintf(&b, "The principals assigned the role, now, before or later, %d, their names being data: %s.\n", len(who), strings.Join(who, ", "))
	}
	if len(st.rules) == 0 {
		b.WriteString("The tenant has no segregation-of-duties rule.\n")
	}
	for _, rl := range st.rules {
		until := ""
		if rl.ValidTo != "" {
			until = " until " + rl.ValidTo
		}
		fmt.Fprintf(&b, "- rule %s, in force from %s%s; its description, which is data: %q\n", rl.pair(), rl.ValidFrom, until, rl.Description)
	}
	if len(an.conflicts) == 0 {
		b.WriteString("The run computed no conflict: no principal assigned the role would hold both permissions of a rule on a day the rule is in force.")
		return b.String()
	}
	b.WriteString("The conflicts the run computed:\n")
	for _, c := range an.conflicts {
		already := ""
		if c.withoutAdded {
			already = "; it does already, whatever is added"
		}
		fmt.Fprintf(&b, "- %s would hold %s through %s and %s through %s%s\n", st.named(c.principal), c.rule.PermissionA, strings.Join(c.viaA, ", "),
			c.rule.PermissionB, strings.Join(c.viaB, ", "), already)
	}
	b.WriteString("For sod_conflict the content has the lines:\n" + strings.Join(an.lines, "\n"))
	return b.String()
}

// check is the analysis' rule on a recommendation the pattern admits
// (ADR-0072, T106b): sod_conflict is made only when the run computed a
// conflict, and names each rule broken with the principals who would
// break it, on the lines given; no_conflict is made only otherwise; and
// every number it writes is one the run read.
func (an *analysis) check(p recommend.Proposed, f *recommend.Figures) error {
	switch {
	case len(f.Unread(p.Content)) > 0:
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(f.Unread(p.Content), ", "))
	case p.Kind == NoConflict && len(an.lines) > 0:
		return ErrConflict
	case p.Kind == SodConflict && len(an.lines) == 0:
		return ErrNoConflict
	}
	lines := strings.Split(p.Content, "\n")
	for _, want := range an.lines {
		if !slices.Contains(lines, want) {
			return ErrConflictLines
		}
	}
	return nil
}
