package api

// Response statuses (ADR-0052 G7, As built batch 93): every status an
// operation can answer is one of its declared responses, so a consumer
// holding only the contract knows each answer it must handle (a 404 of a
// document not found or not visible, a 409 of a tenant not set up, a 503
// of a store that cannot be reached). The statuses are derived from the
// same call graph as the problem codes (problemcodes_test.go):
//
//   - the status each problem.New call makes its codes with, for every
//     code the operation's own code makes. problem.From's translation of
//     the database's errors is the request pipeline's, answered by any
//     operation and listed in the document's x-problem-codes, so the walk
//     does not enter it; the codes of registered constraints on the tables
//     the operation writes are its own (they are in its x-problem-codes);
//   - 404 for an operation whose path names a resource by a parameter
//     (04 §4.6: not found, or in a company the caller cannot see).
//     request.not_found is not taken from the graph: the lookups making it
//     are shared by a create reading back the row it has just written and
//     by a reference a context answers as 422 through problem.Reference,
//     which the graph, knowing no branch conditions, cannot tell from a
//     404 the operation answers (some 75 functions on main as of batch 90);
//   - the statuses a function relaying problems at run time passes on, as
//     relayedStatuses declares them.
//
// 500 (platform.internal) is left out: any operation can fail with it, and
// none declares it.

import (
	"fmt"
	"go/constant"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

// relay is what a function making a problem with a status given at run
// time passes on: its statuses, and why.
type relay struct {
	statuses []int
	why      string
}

// relayedStatuses are the relays, by function.
var relayedStatuses = map[string]relay{
	module + "/internal/analytics/endpoint.apiError": {[]int{400, 403, 409, 422},
		"the 4xx of the API's list reads the semantic layer answers through as the caller: a parameter, a permission, a tenant not set up, a value"},
}

// unresolvedCounts is how many sites building a code at run time each
// function has found so far.
func (a *codeAnalysis) unresolvedCounts() map[string]int {
	out := make(map[string]int, len(a.unresolved))
	for fn, sites := range a.unresolved {
		out[fn] = len(sites)
	}
	return out
}

// statusOf records the status a problem.New call makes its codes with:
// for each code traced from it, and for each function that builds a code
// at run time found while tracing them (before is unresolvedCounts as it
// was). A status given at run time makes the caller a relay.
func (a *codeAnalysis) statusOf(e *callgraph.Edge, made []emission, before map[string]int) {
	c, ok := e.Site.Common().Args[0].(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.Int {
		a.relays[e.Caller.Func.String()] = true
		return
	}
	status, _ := constant.Int64Val(c.Value) // an HTTP status, within int64
	for _, m := range made {
		addStatus(a.statuses, m.code, int(status))
		if a.gateStatuses[m.gate] == nil {
			a.gateStatuses[m.gate] = map[string]map[int]bool{}
		}
		addStatus(a.gateStatuses[m.gate], m.code, int(status))
	}
	for fn, sites := range a.unresolved {
		if len(sites) > before[fn] {
			addStatus(a.runTime, fn, int(status))
		}
	}
}

// dynamicStatuses gives dynamicCodes' codes the statuses their functions'
// sites make them with.
func (a *codeAnalysis) dynamicStatuses() {
	for fn, codes := range dynamicCodes {
		for _, c := range codes {
			for st := range a.runTime[fn] {
				addStatus(a.statuses, c, st)
			}
		}
	}
}

func addStatus(m map[string]map[int]bool, key string, status int) {
	if m[key] == nil {
		m[key] = map[int]bool{}
	}
	m[key][status] = true
}

// checkStatuses checks each operation's responses against the statuses
// it answers, and relayedStatuses against the program; checkStatusesRefuse
// proves it fails what it must.
func checkStatuses(t *testing.T, a *codeAnalysis, spec node) {
	t.Helper()
	for _, e := range a.undeclaredStatuses(t, spec, relayedStatuses) {
		t.Error(e)
	}
}

// undeclaredStatuses is what is wrong of the operations' responses and of
// the relays declared.
func (a *codeAnalysis) undeclaredStatuses(t *testing.T, spec node, relays map[string]relay) []string {
	t.Helper()
	var out []string
	for fn := range a.relays {
		if _, ok := relays[fn]; !ok {
			out = append(out, fn+" makes a problem with a status given at run time: declare what it relays in relayedStatuses")
		}
	}
	for fn := range relays {
		if !a.relays[fn] {
			out = append(out, "relayedStatuses declares "+fn+", which relays no status any more")
		}
	}
	for _, o := range operations(t, spec) {
		id, _ := o.op["operationId"].(string)
		declared, _ := o.op["responses"].(node)
		if _, ok := declared["default"]; ok {
			continue
		}
		answers := a.answers(t, id, o.path, relays)
		for _, st := range slices.Sorted(maps.Keys(answers)) {
			if _, ok := declared[strconv.Itoa(st)]; !ok {
				out = append(out, fmt.Sprintf("%s answers %d (%s): got no %d among its responses, want one", id, st, strings.Join(answers[st], ", "), st))
			}
		}
	}
	slices.Sort(out)
	return out
}

// checkStatusesRefuse proves undeclaredStatuses fails an operation's
// response taken away, a 404 of a path's parameter, and relays undeclared
// or no longer made.
func checkStatusesRefuse(t *testing.T, a *codeAnalysis) {
	t.Helper()
	spec := loadSpec(t)
	responses := func(path, method string) node {
		return spec["paths"].(node)[path].(node)[method].(node)["responses"].(node)
	}
	delete(responses("/v1/supplier-invoices/{id}", "get"), "409")
	delete(responses("/v1/supplier-invoices/{id}", "get"), "404")
	relays := map[string]relay{module + "/internal/platform/problem.New": {}}
	got := a.undeclaredStatuses(t, spec, relays)
	for _, want := range []string{
		"getSupplierInvoice answers 409 (config.not_bootstrapped): got no 409 among its responses, want one",
		"getSupplierInvoice answers 404 (its path's parameter): got no 404 among its responses, want one",
		module + "/internal/analytics/endpoint.apiError makes a problem with a status given at run time: declare what it relays in relayedStatuses",
		"relayedStatuses declares " + module + "/internal/platform/problem.New, which relays no status any more",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("undeclaredStatuses: got %q, want it to hold %q", got, want)
		}
	}
	if len(got) != 4 {
		t.Errorf("undeclaredStatuses: got %d findings (%q), want 4", len(got), got)
	}
}

// answers is the statuses the operation answers, each with what answers
// it.
func (a *codeAnalysis) answers(t *testing.T, id, path string, relays map[string]relay) map[int][]string {
	t.Helper()
	out := map[int][]string{}
	r := a.walk([]*ssa.Function{a.operation(t, id)}, a.from)
	for c, sts := range a.statusesOf(r, a.untakenIn(r)) {
		if len(sts) == 0 {
			t.Errorf("%s: no status is known for %s", id, c)
		}
		for st := range sts {
			if c != "request.not_found" && st != 500 && !slices.Contains(out[st], c) {
				out[st] = append(out[st], c)
			}
		}
	}
	for fn, relayed := range relays {
		if f := a.fns[fn]; f != nil && r.fns[f] {
			for _, st := range relayed.statuses {
				out[st] = append(out[st], "relayed by "+fn+": "+relayed.why)
			}
		}
	}
	if strings.Contains(path, "{") {
		out[404] = append(out[404], "its path's parameter")
	}
	for st := range out {
		slices.Sort(out[st])
	}
	return out
}

// statusesOf is codesOf with each code's statuses: those the gates in
// the traversal make it with, and, for a code a gate makes at run time
// (dynamicCodes), a registered constraint's code and one kept in a
// variable, every status it is made with.
func (a *codeAnalysis) statusesOf(r reached, skip map[*ssa.Function]map[string]bool) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	add := func(c string, sts map[int]bool) {
		if out[c] == nil {
			out[c] = map[int]bool{}
		}
		maps.Copy(out[c], sts)
	}
	for f := range r.fns {
		for _, c := range a.gated[f] {
			if skip[f][c] {
				continue
			}
			sts, ok := a.gateStatuses[f][c]
			if !ok {
				sts = a.statuses[c]
			}
			add(c, sts)
		}
		for _, tbl := range a.writes[f] {
			for _, c := range a.constraints[tbl] {
				add(c, a.statuses[c])
			}
		}
	}
	for c := range r.kept {
		add(c, a.statuses[c])
	}
	return out
}
