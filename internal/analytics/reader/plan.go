package reader

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/analytics/datasets"
)

// compiled is a plan's SQL over the views and the datasets it reads.
type compiled struct {
	sql      string
	args     []any
	through  string   // the latest date of the fact in the views
	datasets []string // the fact dataset, then each dataset it joins or derives from
	sum      bool
}

// planner resolves a plan's names against the contract.
type planner struct {
	fact  *datasets.Dataset
	joins map[string]*datasets.Dataset // the fact's dimension datasets, by name
	used  map[string]bool              // the dimension datasets the plan reads
	start int                          // the fiscal year's first month
	args  []any
}

// resolved is a name a plan may use: its SQL and the ranges it takes.
type resolved struct {
	expr   string
	ranges string // "date" (ISO dates), "int" (fiscal periods) or "" (none)
}

// fiscalNames are the periods derived from the fact's date and the
// semantic layer's calendar.
var fiscalNames = []string{"fiscal_year", "fiscal_quarter", "fiscal_month"}

// compile checks a plan against the contract and the scope's fact types
// and writes its SQL: every value a parameter, every name the contract's,
// quoted.
func compile(contract map[string]*datasets.Dataset, scope Scope, p Plan) (compiled, error) {
	fact := contract[p.Dataset]
	if fact == nil || fact.Kind != "fact" {
		return compiled{}, refused("dataset %q is not a fact dataset", p.Dataset)
	}
	if !slices.Contains(scope.Datasets, p.Dataset) {
		return compiled{}, refused("dataset %s is outside the principal's fact types", p.Dataset)
	}
	if p.FiscalYearStartMonth < 0 || p.FiscalYearStartMonth > 12 {
		return compiled{}, refused("fiscal year start month %d", p.FiscalYearStartMonth)
	}
	pl := &planner{fact: fact, joins: map[string]*datasets.Dataset{}, used: map[string]bool{}, start: max(p.FiscalYearStartMonth, 1)}
	for _, j := range fact.Joins {
		pl.joins[j.Dataset] = contract[j.Dataset]
	}
	measure, err := pl.measure(p.Aggregate)
	if err != nil {
		return compiled{}, err
	}
	groups, err := pl.groups(p.By)
	if err != nil {
		return compiled{}, err
	}
	where, err := pl.where(p.Where, p.Ledger)
	if err != nil {
		return compiled{}, err
	}
	return pl.build(p, measure, groups, where), nil
}

// build assembles the compiled plan.
func (pl *planner) build(p Plan, measure string, groups []string, where string) compiled {
	fact := pl.fact
	sum := p.Aggregate == "sum"
	if sum {
		groups = append(groups, ident(fact.Dataset, "currency"), ident(fact.Dataset, "exponent"))
	}
	cols := append(slices.Clone(groups), measure)
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + pgx.Identifier{fact.Dataset}.Sanitize() + pl.joinSQL() + where
	if len(groups) > 0 {
		q += " GROUP BY " + strings.Join(groups, ", ") + " ORDER BY " + strings.Join(groups, ", ")
	}
	c := compiled{sql: q, args: pl.args, datasets: []string{fact.Dataset}, sum: sum,
		through: "SELECT COALESCE(CAST(MAX(" + ident(fact.Dataset, fact.Date) + ") AS VARCHAR), '') FROM " + pgx.Identifier{fact.Dataset}.Sanitize()}
	for _, j := range fact.Joins {
		if pl.used[j.Dataset] {
			c.datasets = append(c.datasets, j.Dataset)
		}
	}
	for _, d := range fact.Derived {
		if pl.used[d.From] {
			c.datasets = append(c.datasets, d.From)
		}
	}
	return c
}

// measure is the aggregate's SQL.
func (pl *planner) measure(aggregate string) (string, error) {
	switch aggregate {
	case "sum":
		return "SUM(" + ident(pl.fact.Dataset, pl.fact.WithRole("amount")[0].Name) + ")", nil
	case "count_distinct":
		return "COUNT(DISTINCT " + ident(pl.fact.Dataset, pl.fact.WithRole("id")[0].Name) + ")", nil
	}
	return "", refused("aggregate %q", aggregate)
}

// groups is the plan's breakdown, each a string ("" for none).
func (pl *planner) groups(by []string) ([]string, error) {
	out := make([]string, 0, len(by)+2)
	for _, name := range by {
		r, err := pl.resolve(name)
		if err != nil {
			return nil, err
		}
		out = append(out, text(r.expr))
	}
	return out, nil
}

// where is the plan's filters, each value a parameter, and the ledger of a
// dataset keyed by ledger, which a plan must name.
func (pl *planner) where(filters []Filter, ledger string) (string, error) {
	var conds []string
	if pl.fact.Column("ledger_id") != nil {
		id, err := uuid.Parse(ledger)
		if err != nil {
			return "", refused("dataset %s is read in one ledger: the plan names %q", pl.fact.Dataset, ledger)
		}
		conds = append(conds, ident(pl.fact.Dataset, "ledger_id")+" = CAST("+pl.arg(id.String())+" AS UUID)")
	}
	for _, f := range filters {
		r, err := pl.resolve(f.Column)
		if err != nil {
			return "", err
		}
		cond, err := pl.condition(r, f)
		if err != nil {
			return "", err
		}
		conds = append(conds, cond)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), nil
}

// condition is one filter's SQL.
func (pl *planner) condition(r resolved, f Filter) (string, error) {
	want := map[string]int{">=": 1, "<=": 1, "between": 2}
	if n, ranged := want[f.Op]; ranged {
		args, ok := rangeArgs(r.ranges, f.Values)
		if !ok || len(f.Values) != n {
			return "", refused("filter %s %s: a range takes the date or a fiscal period and %d values of it", f.Column, f.Op, n)
		}
		col := r.expr
		if r.ranges == "date" {
			col = text(col) // ISO dates compare as strings, as the semantic layer compares them
		}
		if f.Op == "between" {
			return col + " BETWEEN " + pl.arg(args[0]) + " AND " + pl.arg(args[1]), nil
		}
		return col + " " + f.Op + " " + pl.arg(args[0]), nil
	}
	if (f.Op != "in" && f.Op != "not_in") || len(f.Values) == 0 {
		return "", refused("filter %s: op %q with %d values", f.Column, f.Op, len(f.Values))
	}
	list := make([]string, len(f.Values))
	for i, v := range f.Values {
		list[i] = pl.arg(v)
	}
	not := map[bool]string{true: " NOT IN (", false: " IN ("}[f.Op == "not_in"]
	return text(r.expr) + not + strings.Join(list, ", ") + ")", nil
}

// rangeArgs is a range's values as parameters: ISO dates for the date,
// integers for a fiscal period; ok is false for anything else.
func rangeArgs(ranges string, values []string) ([]any, bool) {
	out := make([]any, len(values))
	for i, v := range values {
		switch ranges {
		case "date":
			if _, err := time.Parse(time.DateOnly, v); err != nil {
				return nil, false
			}
			out[i] = v
		case "int":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, false
			}
			out[i] = n
		default:
			return nil, false
		}
	}
	return out, true
}

// resolve finds a name among the fact's dimensions, currency and date, its
// fiscal periods and derived names, then among the attributes of the
// dimensions it joins; a key, an amount or a column the contract does not
// list is not one a plan may name. The currency is one where a company's
// amounts are in several (the cash actuals of its foreign-currency house
// banks); the semantic layer offers it only for a metric that lists it.
func (pl *planner) resolve(name string) (resolved, error) {
	if c := pl.fact.Column(name); c != nil && (c.Role == "dimension" || c.Role == "date" || c.Role == "currency") {
		r := resolved{expr: ident(pl.fact.Dataset, name)}
		if c.Role == "date" {
			r.ranges = "date"
		}
		return r, nil
	}
	if slices.Contains(fiscalNames, name) && pl.fact.Date != "" {
		return resolved{expr: pl.fiscal(name), ranges: "int"}, nil
	}
	if r, ok := pl.derived(name); ok {
		return r, nil
	}
	for _, j := range pl.fact.Joins {
		d := pl.joins[j.Dataset]
		if c := d.Column(name); c != nil && (c.Role == "dimension" || c.Role == "attribute") {
			pl.used[j.Dataset] = true
			return resolved{expr: ident(j.Dataset, name)}, nil
		}
	}
	return resolved{}, refused("column %q is not one %s answers", name, pl.fact.Dataset)
}

// fiscal is a fiscal period of the fact's date, as the semantic layer's
// calendar derives it: with a start month other than January the year is
// named after the calendar year in which it ends. The start month is an
// integer the planner checked, so it is written as a literal.
func (pl *planner) fiscal(name string) string {
	date, s := ident(pl.fact.Dataset, pl.fact.Date), strconv.Itoa(pl.start)
	month := "((month(" + date + ") - " + s + " + 12) % 12 + 1)"
	switch name {
	case "fiscal_month":
		return month
	case "fiscal_quarter":
		return "((" + month + " - 1) // 3 + 1)"
	}
	if pl.start == 1 {
		return "year(" + date + ")"
	}
	return "(year(" + date + ") + CASE WHEN month(" + date + ") >= " + s + " THEN 1 ELSE 0 END)"
}

// derived is a name the fact answers by a rule over another dataset. The
// one the contract has is cleared: a line on a supplier reconciliation
// account that is no longer an open item.
func (pl *planner) derived(name string) (resolved, bool) {
	for _, d := range pl.fact.Derived {
		if d.Name != name || name != "cleared" || pl.joins["accounts"] == nil {
			continue
		}
		pl.used["accounts"], pl.used[d.From] = true, true
		f, o := pgx.Identifier{pl.fact.Dataset}.Sanitize(), pgx.Identifier{d.From}.Sanitize()
		return resolved{expr: "(COALESCE(" + ident("accounts", "vendor_recon") + ", false) AND NOT EXISTS (SELECT 1 FROM " + o +
			" WHERE " + o + ".document_id = " + f + ".document_id AND " + o + ".line_no = " + f + ".line_no))"}, true
	}
	return resolved{}, false
}

// joinSQL joins each dimension dataset the plan reads on its keys.
func (pl *planner) joinSQL() string {
	var b strings.Builder
	for _, j := range pl.fact.Joins {
		if !pl.used[j.Dataset] {
			continue
		}
		on := make([]string, len(j.On))
		for i, k := range j.On {
			on[i] = ident(pl.fact.Dataset, k) + " = " + ident(j.Dataset, k)
		}
		b.WriteString(" LEFT JOIN " + pgx.Identifier{j.Dataset}.Sanitize() + " ON " + strings.Join(on, " AND "))
	}
	return b.String()
}

// arg adds a parameter and returns its placeholder.
func (pl *planner) arg(v any) string {
	pl.args = append(pl.args, v)
	return "$" + strconv.Itoa(len(pl.args))
}

func ident(dataset, column string) string { return pgx.Identifier{dataset, column}.Sanitize() }

// text is a column as the semantic layer compares and groups it: a string,
// "" when null (a line without a partner).
func text(col string) string { return "COALESCE(CAST(" + col + " AS VARCHAR), '')" }
