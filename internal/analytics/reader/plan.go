package reader

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/analytics/datasets"
)

// Plan is a semantic query plan: the semantic layer generates it from a
// metric (T55e), and the reader turns it into SQL over the contract's
// columns alone, so a request never carries SQL and cannot name a column
// the contract does not list.
type Plan struct {
	Dataset   string   `json:"dataset"`   // a fact dataset
	Aggregate string   `json:"aggregate"` // sum (of the amount, per currency) | count_distinct (of the document id)
	By        []string `json:"by,omitempty"`
	Where     []Filter `json:"where,omitempty"`
}

// Filter restricts a column: in and not_in on any column the plan may
// name, >=, <= and between on the dataset's date (ISO dates).
type Filter struct {
	Column string   `json:"column"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// compiled is a plan's SQL over the views and the datasets it reads.
type compiled struct {
	sql      string
	args     []any
	datasets []string // the fact dataset, then each dimension it joins
	sum      bool
}

// planner resolves a plan's names against the contract.
type planner struct {
	fact  *datasets.Dataset
	joins map[string]*datasets.Dataset // the fact's dimension datasets, by name
	used  map[string]bool              // the dimension datasets the plan reads
	args  []any
}

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
	pl := &planner{fact: fact, joins: map[string]*datasets.Dataset{}, used: map[string]bool{}}
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
	where, err := pl.where(p.Where)
	if err != nil {
		return compiled{}, err
	}
	sum := p.Aggregate == "sum"
	if sum {
		groups = append(groups, ident(fact.Dataset, "currency"), ident(fact.Dataset, "exponent"))
	}
	cols := append(slices.Clone(groups), measure)
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + pgx.Identifier{fact.Dataset}.Sanitize() + pl.joinSQL() + where
	if len(groups) > 0 {
		q += " GROUP BY " + strings.Join(groups, ", ") + " ORDER BY " + strings.Join(groups, ", ")
	}
	c := compiled{sql: q, args: pl.args, datasets: []string{fact.Dataset}, sum: sum}
	for _, j := range fact.Joins {
		if pl.used[j.Dataset] {
			c.datasets = append(c.datasets, j.Dataset)
		}
	}
	return c, nil
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
		col, _, err := pl.resolve(name)
		if err != nil {
			return nil, err
		}
		out = append(out, text(col))
	}
	return out, nil
}

// where is the plan's filters, each value a parameter.
func (pl *planner) where(filters []Filter) (string, error) {
	var conds []string
	for _, f := range filters {
		col, c, err := pl.resolve(f.Column)
		if err != nil {
			return "", err
		}
		cond, err := pl.condition(text(col), c.Role == "date", f)
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
func (pl *planner) condition(col string, date bool, f Filter) (string, error) {
	want := map[string]int{">=": 1, "<=": 1, "between": 2}
	if n, ranged := want[f.Op]; ranged {
		if !date || len(f.Values) != n || !isoDates(f.Values) {
			return "", refused("filter %s %s: a range takes the date column and %d ISO dates", f.Column, f.Op, n)
		}
		if f.Op == "between" {
			return col + " BETWEEN " + pl.arg(f.Values[0]) + " AND " + pl.arg(f.Values[1]), nil
		}
		return col + " " + f.Op + " " + pl.arg(f.Values[0]), nil
	}
	if (f.Op != "in" && f.Op != "not_in") || len(f.Values) == 0 {
		return "", refused("filter %s: op %q with %d values", f.Column, f.Op, len(f.Values))
	}
	list := make([]string, len(f.Values))
	for i, v := range f.Values {
		list[i] = pl.arg(v)
	}
	not := map[bool]string{true: " NOT IN (", false: " IN ("}[f.Op == "not_in"]
	return col + not + strings.Join(list, ", ") + ")", nil
}

// resolve finds a name among the fact's dimensions and date, then among
// the attributes of the dimensions it joins; a key, an amount or a column
// the contract does not list is not one a plan may name.
func (pl *planner) resolve(name string) (string, *datasets.Column, error) {
	if c := pl.fact.Column(name); c != nil && (c.Role == "dimension" || c.Role == "date") {
		return ident(pl.fact.Dataset, name), c, nil
	}
	for _, j := range pl.fact.Joins {
		d := pl.joins[j.Dataset]
		if c := d.Column(name); c != nil && (c.Role == "dimension" || c.Role == "attribute") {
			pl.used[j.Dataset] = true
			return ident(j.Dataset, name), c, nil
		}
	}
	return "", nil, refused("column %q is not one %s answers", name, pl.fact.Dataset)
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
func (pl *planner) arg(v string) string {
	pl.args = append(pl.args, v)
	return "$" + strconv.Itoa(len(pl.args))
}

func ident(dataset, column string) string { return pgx.Identifier{dataset, column}.Sanitize() }

// text is a column as the semantic layer compares and groups it: a string,
// "" when null (a line without a partner).
func text(col string) string { return "COALESCE(CAST(" + col + " AS VARCHAR), '')" }

func isoDates(values []string) bool {
	for _, v := range values {
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			return false
		}
	}
	return true
}
