package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

const defaultReports = "data/reports"

// analyticsFlags are shared by the analytics commands.
type analyticsFlags struct {
	model, companies *string
}

func addAnalyticsFlags(fs *flag.FlagSet) analyticsFlags {
	return analyticsFlags{
		model:     fs.String("semantic", "", "semantic model YAML (default: built-in governed model)"),
		companies: fs.String("companies", "", "comma-separated company codes to restrict to (default: all you may read)"),
	}
}

func loadModel(path string) (*analytics.Model, error) {
	if path != "" {
		return analytics.LoadModel(path)
	}
	return analytics.DefaultModel()
}

// session reads the facts through the API with the person's token: only
// the legal entities and data they may read.
func (f analyticsFlags) session(ctx context.Context) (*analytics.Session, error) {
	m, err := loadModel(*f.model)
	if err != nil {
		return nil, err
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return nil, err
	}
	facts, err := analytics.Source{API: c}.Facts(ctx, m, time.Now())
	if err != nil {
		return nil, err
	}
	p := analytics.Principal{ID: "token"}
	if *f.companies != "" {
		p.Companies = strings.Split(*f.companies, ",")
	}
	return analytics.NewSession(m, facts, p), nil
}

func metricsCmd(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	semantic := fs.String("semantic", "", "semantic model YAML (default: built-in governed model)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	m, err := loadModel(*semantic)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tTYPE\tDESCRIPTION\tDIMENSIONS")
	for _, met := range m.Metrics {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", met.Name, met.Type, met.Description, strings.Join(m.AllowedDimensions(met.Name), ", "))
	}
	return errs.Wrapf(tw.Flush(), "metrics: print")
}

// queryCmd runs a semantic query directly, without the agent.
//
//	erp query -measures revenue -by fiscal_year,fiscal_month -where "fiscal_month <= 9"
func queryCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	measures := fs.String("measures", "", "comma-separated metrics")
	by := fs.String("by", "", "comma-separated dimensions")
	var where multiFlag
	fs.Var(&where, "where", `filter, repeatable: "dim in a,b", "dim not_in a", "dim <= v", "dim >= v", "dim between a,b"`)
	csvOut := fs.String("csv", "", "also write the result as CSV to this file")
	if err := parseFlags(fs, args); err != nil {
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
	sess, err := af.session(ctx)
	if err != nil {
		return err
	}
	r, err := sess.Run(q)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', tabwriter.AlignRight)
	printResult(tw, r)
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("query: print: %w", err)
	}
	if _, err := fmt.Fprintf(out, "\n%s · %d rows · data through %s\n", r.ID, len(r.Rows), r.DataThrough); err != nil {
		return fmt.Errorf("query: print summary: %w", err)
	}
	if *csvOut != "" {
		return writeCSV(*csvOut, r)
	}
	return nil
}

// writeCSV writes r to the file name. A failed close can mean the file was
// truncated, so it is reported when the write itself succeeded.
func writeCSV(name string, r *analytics.Result) (err error) {
	f, err := createFile(name)
	if err != nil {
		return fmt.Errorf("query: create CSV: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("query: close CSV %s: %w", name, cerr)
		}
	}()
	return analytics.WriteCSV(f, r)
}

// createFile creates a file the command writes; tests replace it.
var createFile = func(name string) (io.WriteCloser, error) { return os.Create(name) }

// newAgent makes the analytics agent; tests replace it.
var newAgent = analytics.NewAgent

// askCmd answers a plain-language question with an on-demand dashboard.
func askCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	outDir := fs.String("out", defaultReports, "directory for generated reports")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return fmt.Errorf(`ask a question, e.g. erp ask "monthly sales vs last year this time"`)
	}
	sess, err := af.session(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(errOut, "Working on it...")
	ans, err := newAgent().Ask(ctx, sess, question)
	if err != nil {
		return err
	}
	path, err := ans.Report.Save(*outDir, sess.Results)
	if err != nil {
		return err
	}
	return printf("ask", "%s\n\nDashboard: %s\nRerun later with: erp report -id %s\n", ans.Summary, path, ans.Report.ID)
}

// reportCmd reruns a saved report (or a hand-written report file) on current data.
func reportCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	id := fs.String("id", "", "saved report id (from erp ask)")
	file := fs.String("file", "", "report definition JSON (instead of -id)")
	outDir := fs.String("out", defaultReports, "directory for generated reports")
	if err := parseFlags(fs, args); err != nil {
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
	sess, err := af.session(ctx)
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
	return printf("report", "Refreshed %q with data through %s\nDashboard: %s\n", fresh.Spec.Title, sess.Facts.DataThrough, html)
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

// printResult writes a query result as a table: a header with each
// column's currency, then the rows.
func printResult(tw *tabwriter.Writer, r *analytics.Result) {
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
			case float64: //nolint:forbidigo // analytics results carry measures as float64 for display (internal/analytics is exempt); this only prints them
				fmt.Fprintf(tw, "%.2f\t", x)
			default:
				fmt.Fprint(tw, "—\t")
			}
		}
		fmt.Fprintln(tw)
	}
}
