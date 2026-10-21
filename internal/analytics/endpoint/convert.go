package endpoint

import (
	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
)

// fromAPI is the semantic query of a request.
func fromAPI(req *gen.AnalyticsQuery) analytics.Query {
	q := analytics.Query{Measures: req.Measures, By: req.By, OrderBy: req.OrderBy.Or(""), Desc: req.Desc.Or(false), Limit: req.Limit.Or(0)}
	for _, f := range req.Where {
		q.Where = append(q.Where, analytics.Filter{Dim: f.Dim, Op: f.Op, Values: f.Values})
	}
	if id, ok := req.Ledger.Get(); ok {
		q.Ledger = id.String()
	}
	return q
}

// toAPI is a result as the contract writes it: each measure an exact
// decimal string with its currency.
func toAPI(r *analytics.Result) gen.AnalyticsResult {
	out := gen.AnalyticsResult{QueryID: r.ID, ConfigVersion: r.ConfigVersion, Source: r.Source, AsOf: r.AsOf, DataThrough: r.DataThrough, Truncated: r.Truncated,
		Columns: make([]gen.AnalyticsColumn, 0, len(r.Columns)), Rows: make([][]gen.AnalyticsCell, 0, len(r.Rows)),
		Definitions: make([]gen.AnalyticsDefinition, 0, len(r.Definitions))}
	for _, c := range r.Columns {
		col := gen.AnalyticsColumn{Name: c.Name, Label: c.Label, Kind: c.Kind}
		if c.Type != "" {
			col.Type = gen.NewOptString(string(c.Type))
		}
		if c.Currency != "" {
			col.Currency = gen.NewOptString(c.Currency)
		}
		out.Columns = append(out.Columns, col)
	}
	for _, row := range r.Rows {
		cells := make([]gen.AnalyticsCell, len(row))
		for i, v := range row {
			cells[i] = toCell(v)
		}
		out.Rows = append(out.Rows, cells)
	}
	for _, d := range r.Definitions {
		def := gen.AnalyticsDefinition{Name: d.Name, Description: d.Description}
		if d.Source != "" {
			def.Source = gen.NewOptString(d.Source)
		}
		if d.Filter != nil {
			def.Filter = gen.NewOptAnalyticsDefinitionFilter(d.Filter)
		}
		if d.Derived != nil {
			def.Derived = gen.NewOptAnalyticsDerived(gen.AnalyticsDerived{Op: d.Derived.Op, Left: d.Derived.Left, Right: d.Derived.Right})
		}
		out.Definitions = append(out.Definitions, def)
	}
	return out
}

// toCell writes a cell as Execute filled it: a dimension's string, a
// measure's Value, or nil for a measure that cannot be computed.
func toCell(v any) gen.AnalyticsCell {
	switch x := v.(type) {
	case string:
		return gen.AnalyticsCell{Value: gen.NewNilString(x)}
	case analytics.Value:
		c := gen.AnalyticsCell{Value: gen.NewNilString(x.String())}
		if x.Currency != "" {
			c.Currency = gen.NewOptString(x.Currency)
		}
		return c
	}
	return gen.AnalyticsCell{Value: gen.NilString{Null: true}} // nil: Execute fills no other kind
}
