package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
)

const defaultReports = "data/reports"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, "; ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// analyticsFlags are shared by the analytics commands.
type analyticsFlags struct {
	store, audit, model, as, companies *string
}

func addAnalyticsFlags(fs *flag.FlagSet) analyticsFlags {
	return analyticsFlags{
		store:     fs.String("store", defaultStore, "core store snapshot"),
		audit:     fs.String("audit", defaultAudit, "audit log"),
		model:     fs.String("semantic", "", "semantic model YAML (default: built-in governed model)"),
		as:        fs.String("as", "analyst", "user asking (recorded in the audit log)"),
		companies: fs.String("companies", "", "comma-separated company codes the user may see (default: all)"),
	}
}

func (f analyticsFlags) session() (*analytics.Session, error) {
	s, err := core.LoadStore(*f.store)
	if err != nil {
		return nil, fmt.Errorf("%w (run \"erp migrate\" first)", err)
	}
	var m *analytics.Model
	if *f.model != "" {
		m, err = analytics.LoadModel(*f.model)
	} else {
		m, err = analytics.DefaultModel()
	}
	if err != nil {
		return nil, err
	}
	facts, err := analytics.BuildFacts(s, m, time.Now())
	if err != nil {
		return nil, err
	}
	audit, err := governance.NewAuditLog(*f.audit)
	if err != nil {
		return nil, err
	}
	p := analytics.Principal{ID: "user:" + *f.as}
	if *f.companies != "" {
		p.Companies = strings.Split(*f.companies, ",")
	}
	return analytics.NewSession(m, facts, p, audit), nil
}

func metricsCmd(args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	semantic := fs.String("semantic", "", "semantic model YAML (default: built-in governed model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var m *analytics.Model
	var err error
	if *semantic != "" {
		m, err = analytics.LoadModel(*semantic)
	} else {
		m, err = analytics.DefaultModel()
	}
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tTYPE\tDESCRIPTION\tDIMENSIONS")
	for _, met := range m.Metrics {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", met.Name, met.Type, met.Description, strings.Join(m.AllowedDimensions(met.Name), ", "))
	}
	tw.Flush()
	fmt.Fprintln(out, "\nEveryday words:")
	for word, metric := range m.Defaults {
		fmt.Fprintf(out, "  %q means %s\n", word, metric)
	}
	return nil
}

// queryCmd runs a semantic query directly, without the agent.
//
//	erp query -measures revenue -by fiscal_year,fiscal_month \
//	  -where "fiscal_year in 2025,2026" -where "fiscal_month <= 9"
func queryCmd(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	measures := fs.String("measures", "", "comma-separated metrics")
	by := fs.String("by", "", "comma-separated dimensions")
	var where multiFlag
	fs.Var(&where, "where", `filter, repeatable: "dim in a,b", "dim not_in a", "dim <= v", "dim >= v", "dim between a,b"`)
	csvOut := fs.String("csv", "", "also write the result as CSV to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q := analytics.Query{Measures: splitList(*measures), By: splitList(*by)}
	for _, w := range where {
		f, err := parseFilter(w)
		if err != nil {
			return err
		}
		q.Where = append(q.Where, f)
	}
	sess, err := af.session()
	if err != nil {
		return err
	}
	r, err := sess.Run(q)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', tabwriter.AlignRight)
	for _, c := range r.Columns {
		h := c.Name
		if c.Currency != "" {
			h += " (" + c.Currency + ")"
		}
		fmt.Fprintf(tw, "%s\t", h)
	}
	fmt.Fprintln(tw)
	for _, row := range r.Rows {
		for _, v := range row {
			switch x := v.(type) {
			case string:
				fmt.Fprintf(tw, "%s\t", x)
			case float64:
				fmt.Fprintf(tw, "%.2f\t", x)
			default:
				fmt.Fprint(tw, "—\t")
			}
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()
	fmt.Fprintf(out, "\n%s · %d rows · data through %s\n", r.ID, len(r.Rows), r.DataThrough)
	if *csvOut != "" {
		f, err := os.Create(*csvOut)
		if err != nil {
			return err
		}
		defer f.Close()
		return analytics.WriteCSV(f, r)
	}
	return nil
}

// askCmd answers a plain-language question with an on-demand dashboard.
func askCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	outDir := fs.String("out", defaultReports, "directory for generated reports")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return fmt.Errorf(`ask a question, e.g. erp ask "monthly sales vs last year this time"`)
	}
	sess, err := af.session()
	if err != nil {
		return err
	}
	fmt.Fprintln(errOut, "Working on it...")
	ans, err := analytics.NewAgent().Ask(ctx, sess, question)
	if err != nil {
		return err
	}
	path, err := ans.Report.Save(*outDir, sess.Results)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, ans.Summary)
	fmt.Fprintf(out, "\nDashboard: %s\nRerun later with: erp report -id %s\n", path, ans.Report.ID)
	return nil
}

// reportCmd reruns a saved report (or a hand-written report file) on current data.
func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	id := fs.String("id", "", "saved report id (from erp ask)")
	file := fs.String("file", "", "report definition JSON (instead of -id)")
	outDir := fs.String("out", defaultReports, "directory for generated reports")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *file
	if path == "" {
		if *id == "" {
			return fmt.Errorf("-id or -file is required")
		}
		path = filepath.Join(*outDir, *id+".json")
	}
	rep, err := analytics.LoadReport(path)
	if err != nil {
		return err
	}
	sess, err := af.session()
	if err != nil {
		return err
	}
	fresh, results, err := sess.Rerun(rep)
	if err != nil {
		return err
	}
	html, err := fresh.Save(*outDir, results)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Refreshed %q with data through %s\nDashboard: %s\n", fresh.Spec.Title, sess.Facts.DataThrough, html)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseFilter(s string) (analytics.Filter, error) {
	parts := strings.Fields(s)
	if len(parts) < 3 {
		return analytics.Filter{}, fmt.Errorf("filter %q: want \"dim op values\"", s)
	}
	return analytics.Filter{Dim: parts[0], Op: parts[1], Values: splitList(strings.Join(parts[2:], ""))}, nil
}
