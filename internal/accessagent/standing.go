package accessagent

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// The tenant's access as it stands, read through the API before a
// session (ADR-0072, T106b), and the segregation-of-duties conflicts in
// it, computed as the API's own check at a grant finds them
// (internal/identity sodConflict): a principal breaks a rule when, on
// some day from today on that the rule is in force, it holds both of the
// rule's permissions, each through an assignment of an active role and a
// grant of that role valid that day. Nothing else gives a principal a
// permission a rule can name: the permissions every principal holds
// whatever its roles are refused as a rule's (identity.invalid_sod_rule).

// active is the status of a role whose permissions reach its holders.
const active = "active"

// span is a validity: its first day and the day it ends, exclusive, ""
// for none. ISO dates compare as text.
type span struct{ from, to string }

// and is the days both validities hold.
func (s span) and(o span) span {
	out := span{from: max(s.from, o.from), to: s.to}
	if out.to == "" || (o.to != "" && o.to < out.to) {
		out.to = o.to
	}
	return out
}

// empty reports a validity without a day.
func (s span) empty() bool { return s.to != "" && s.to <= s.from }

type assignment struct {
	PrincipalID string `json:"principal_id"`
	RoleID      string `json:"role_id"`
	ValidFrom   string `json:"valid_from"`
	ValidTo     string `json:"valid_to"`
}

type grant struct {
	Code      string `json:"permission_code"`
	ValidFrom string `json:"valid_from"`
	ValidTo   string `json:"valid_to"`
}

type rule struct {
	ID          string `json:"id"`
	PermissionA string `json:"permission_a"`
	PermissionB string `json:"permission_b"`
	Description string `json:"description"`
	ValidFrom   string `json:"valid_from"`
	ValidTo     string `json:"valid_to"`
}

// pair names a rule by its two permissions.
func (r rule) pair() string { return r.PermissionA + " / " + r.PermissionB }

type principal struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
}

// standing is the tenant's access as a run read it: the roles by id, the
// role assignments by principal, the rules in the order of their pairs,
// the principals by id, and each role's grants once read.
type standing struct {
	today       string
	roles       map[string]role
	assignments map[string][]assignment
	rules       []rule
	principals  map[string]principal
	grants      map[string][]grant
}

// today is the run's day, UTC's: the agent is given no read of the
// database's date.
func (a *Agent) today() string {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	return now().UTC().Format(time.DateOnly)
}

// standing reads the tenant's roles, every role assignment, past and to
// come, the segregation-of-duties rules and the principals.
func (a *Agent) standing(ctx context.Context) (*standing, error) {
	st := &standing{today: a.today(), roles: map[string]role{}, assignments: map[string][]assignment{}, principals: map[string]principal{},
		grants: map[string][]grant{}}
	roles, err := apitools.List[role](ctx, a.API, "/v1/roles", nil)
	if err != nil {
		return nil, fmt.Errorf("accessagent: the roles: %w", err)
	}
	for _, r := range roles {
		st.roles[r.ID] = r
	}
	assignments, err := apitools.List[assignment](ctx, a.API, "/v1/role-assignments", nil)
	if err != nil {
		return nil, fmt.Errorf("accessagent: the role assignments: %w", err)
	}
	for _, as := range assignments {
		if _, ok := st.roles[as.RoleID]; !ok {
			// A role made between the two reads: judging its holders
			// without it could miss a conflict.
			return nil, fmt.Errorf("accessagent: role %s of an assignment is not among the roles read: run again", as.RoleID)
		}
		st.assignments[as.PrincipalID] = append(st.assignments[as.PrincipalID], as)
	}
	if st.rules, err = apitools.List[rule](ctx, a.API, "/v1/sod-rules", nil); err != nil {
		return nil, fmt.Errorf("accessagent: the segregation-of-duties rules: %w", err)
	}
	slices.SortFunc(st.rules, func(x, y rule) int {
		return strings.Compare(x.pair()+" "+x.ValidFrom+" "+x.ID, y.pair()+" "+y.ValidFrom+" "+y.ID)
	})
	principals, err := apitools.List[principal](ctx, a.API, "/v1/principals", nil)
	if err != nil {
		return nil, fmt.Errorf("accessagent: the principals: %w", err)
	}
	for _, p := range principals {
		st.principals[p.ID] = p
	}
	return st, nil
}

// find is the role whose id or code is ref.
func (st *standing) find(ref string) (role, error) {
	for _, id := range slices.Sorted(maps.Keys(st.roles)) {
		if r := st.roles[id]; r.ID == ref || r.Code == ref {
			return r, nil
		}
	}
	return role{}, fmt.Errorf("accessagent: no role %s that the agent may read", ref)
}

// holders are the principals with an assignment of the role, whenever it
// holds, in the order of their ids: the principals the API's check at a
// grant to the role looks at.
func (st *standing) holders(roleID string) []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(st.assignments)) {
		if slices.ContainsFunc(st.assignments[id], func(as assignment) bool { return as.RoleID == roleID }) {
			out = append(out, id)
		}
	}
	return out
}

// readGrants reads the grants, past and to come, of every role the
// principals are assigned, each role once.
func (a *Agent) readGrants(ctx context.Context, st *standing, principals []string) error {
	for _, p := range principals {
		for _, as := range st.assignments[p] {
			if _, read := st.grants[as.RoleID]; read {
				continue
			}
			grants, err := apitools.List[grant](ctx, a.API, "/v1/roles/"+url.PathEscape(as.RoleID)+"/permissions", nil)
			if err != nil {
				return fmt.Errorf("accessagent: the permissions of role %s: %w", st.roles[as.RoleID].Code, err)
			}
			st.grants[as.RoleID] = grants
		}
	}
	return nil
}

// holding is a permission held through a role, and when.
type holding struct {
	role string
	span span
}

// held are the permissions the principal holds on some day from today
// on, each with the roles that give it and when: through an assignment
// of an active role and a grant of that role, both from today on. added
// are grants not made yet, by role.
func (st *standing) held(principalID string, added map[string][]grant) map[string][]holding {
	out := map[string][]holding{}
	for _, as := range st.assignments[principalID] {
		r := st.roles[as.RoleID]
		if r.Status != active {
			continue // an inactive role gives its holders nothing
		}
		assigned := span{as.ValidFrom, as.ValidTo}.and(span{from: st.today})
		for _, g := range slices.Concat(st.grants[as.RoleID], added[as.RoleID]) {
			if s := assigned.and(span{g.ValidFrom, g.ValidTo}); !s.empty() {
				out[g.Code] = append(out[g.Code], holding{r.Code, s})
			}
		}
	}
	return out
}

// conflict is a rule a principal breaks, with the roles each of its
// permissions comes through on the days they collide.
type conflict struct {
	rule         rule
	principal    string
	viaA, viaB   []string
	withoutAdded bool // it stands as the access is, whatever is added
}

// conflicts are the rules the principals break on some day from today
// on, with the grants added, in the order of the principals and, for
// each, of the rules.
func (st *standing) conflicts(principals []string, added map[string][]grant) []conflict {
	var out []conflict
	for _, p := range principals {
		now, then := st.held(p, nil), st.held(p, added)
		for _, r := range st.rules {
			c := conflict{rule: r, principal: p}
			c.viaA, c.viaB = collide(r, then)
			if len(c.viaA) == 0 {
				continue
			}
			standing, _ := collide(r, now)
			c.withoutAdded = len(standing) > 0
			out = append(out, c)
		}
	}
	return out
}

// collide are the roles through which the rule's two permissions are held
// on a day the rule is in force, the first permission's and the
// second's; none when no such day exists.
func collide(r rule, held map[string][]holding) (viaA, viaB []string) {
	in := span{r.ValidFrom, r.ValidTo}
	for _, a := range held[r.PermissionA] {
		for _, b := range held[r.PermissionB] {
			if a.span.and(b.span).and(in).empty() {
				continue
			}
			viaA, viaB = with(viaA, a.role), with(viaB, b.role)
		}
	}
	return viaA, viaB
}

// with is the list with the value, once.
func with(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// named is a principal as a person reads it: its name, which is data,
// on one line, and its id.
func (st *standing) named(id string) string {
	name := strings.Join(strings.Fields(st.principals[id].DisplayName), " ")
	if name == "" {
		return id
	}
	return name + " (" + id + ")"
}
