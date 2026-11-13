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
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

const defaultReports = "data/reports"

// analyticsFlags are shared by the analytics commands.
type analyticsFlags struct {
	companies *string
}

func addAnalyticsFlags(fs *flag.FlagSet) analyticsFlags {
	return analyticsFlags{
		companies: fs.String("companies", "", "comma-separated company codes to restrict to (default: all you may read)"),
	}
}

// session runs its queries on erp-server's analytics endpoint with the
// person's token, which answers from the analytical store or the database
// over only the companies and data the person may read (ADR-0044). The
// model it shows the agent is the tenant's active one, read from the
// endpoint, so a metric a change request added is offered (T55n).
func (f analyticsFlags) session(ctx context.Context, c *apitools.Client) (*analytics.Session, error) {
	p := analytics.Principal{ID: "token"}
	if *f.companies != "" {
		p.Companies = strings.Split(*f.companies, ",")
	}
	remote := analytics.Remote{API: c, Companies: p.Companies}
	m, err := remote.Model(ctx)
	if err != nil {
		return nil, err
	}
	s := analytics.NewSession(m, nil, p)
	s.Runner = remote
	return s, nil
}

// metricsCmd lists the tenant's governed metrics as its queries are
// answered, or a semantic model file's.
func metricsCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ContinueOnError)
	semantic := fs.String("semantic", "", "semantic model YAML (default: the tenant's active model, read with ERP_TOKEN)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	m, err := tenantModel(ctx, *semantic)
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

// tenantModel is the model in the file path, or the tenant's active one.
func tenantModel(ctx context.Context, path string) (*analytics.Model, error) {
	if path != "" {
		return analytics.LoadModel(path)
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return nil, err
	}
	return analytics.Remote{API: c}.Model(ctx)
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
	documents := fs.String("documents", "", `list the documents behind a row instead: "dim=value,..." for each -by dimension ("" without -by), of -measure`)
	measure := fs.String("measure", "", "with -documents, the measure drilled (default: the only one)")
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
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	if flagSet(fs, "documents") {
		return drillCmd(ctx, c, af, analytics.Drill{Query: q, Measure: *measure}, *documents)
	}
	sess, err := af.session(ctx, c)
	if err != nil {
		return err
	}
	r, err := sess.Run(ctx, q)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', tabwriter.AlignRight)
	printResult(tw, r)
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("query: print: %w", err)
	}
	if _, err := fmt.Fprintf(out, "\n%s · %d rows · data through %s · from the %s as of %s\n", r.ID, len(r.Rows), r.DataThrough, r.Source,
		r.AsOf.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("query: print summary: %w", err)
	}
	if *csvOut != "" {
		return writeCSV(*csvOut, r)
	}
	return nil
}

// flagSet reports whether the flag was given, empty or not.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// drillCmd lists the documents behind one row of the query (T94b): the
// row named "dim=value,...", every page of them, and the row's figure.
//
//	erp query -measures revenue -by fiscal_month -where "fiscal_year in 2026" -documents fiscal_month=6
func drillCmd(ctx context.Context, c *apitools.Client, af analyticsFlags, d analytics.Drill, row string) error {
	d.Row = map[string]string{}
	for _, pair := range splitList(row) {
		dim, value, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("-documents %q: want dim=value pairs separated by commas", row)
		}
		d.Row[strings.TrimSpace(dim)] = strings.TrimSpace(value)
	}
	remote := analytics.Remote{API: c}
	if *af.companies != "" {
		remote.Companies = strings.Split(*af.companies, ",")
	}
	got, err := remote.Documents(ctx, d)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tNUMBER\tCOMPANY\tAMOUNT\tCURRENCY\tID")
	for _, doc := range got.Documents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", doc.Kind, doc.Number, doc.Company, analytics.Value{Units: doc.Amount, Scale: doc.Exponent}, doc.Currency, doc.ID)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("query: print: %w", err)
	}
	total := "none"
	if got.Total != nil {
		total = strings.TrimSpace(got.Total.String() + " " + got.Total.Currency)
	}
	return printf("query", "\n%s of the row: %s · %d documents · from the %s as of %s\n", got.Measure, total, len(got.Documents), got.Source,
		got.AsOf.UTC().Format(time.RFC3339))
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

// newAgent makes the analytics agent, calling models through the cell's
// gateway as c's principal (ADR-0036); tests replace it.
var newAgent = defaultAgent

func defaultAgent(c *apitools.Client) *analytics.Agent {
	r := gatewayRunner(c)
	if getenv("ERP_AGENT_MODEL") == "" {
		r.Model = analytics.Alias
	}
	return analytics.NewAgent(r.Client, r.Model)
}

// askCmd answers a plain-language question with an on-demand dashboard.
func askCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	af := addAnalyticsFlags(fs)
	outDir := fs.String("out", defaultReports, "directory for generated reports")
	cont := fs.String("continue", "", "a saved report's id: ask a follow-up question in its analysis")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return fmt.Errorf(`ask a question, e.g. erp ask "monthly sales vs last year this time"`)
	}
	var earlier *analytics.Report
	var err error
	if *cont != "" {
		if earlier, err = continued(af, *outDir, *cont); err != nil {
			return err
		}
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	sess, err := af.session(ctx, c)
	if err != nil {
		return err
	}
	fmt.Fprintln(errOut, "Working on it...")
	ans, err := askOrContinue(ctx, c, sess, earlier, question)
	if err != nil {
		return err
	}
	path, err := ans.Report.Save(*outDir, sess.Results)
	if err != nil {
		return err
	}
	return printf("ask", "%s\n\nDashboard: %s\nRerun later with: erp report -id %s\nAsk a follow-up with: erp ask -continue %s \"...\"\n",
		ans.Summary, path, ans.Report.ID, ans.Report.ID)
}

// continued is the saved report whose analysis a follow-up continues,
// with its companies: the follow-up keeps the analysis's scope, so
// -companies may be left out, and one naming other companies is refused
// rather than answered over a scope the earlier answers were not.
func continued(af analyticsFlags, outDir, id string) (*analytics.Report, error) {
	rep, err := analytics.LoadReport(filepath.Join(outDir, filepath.Base(id)+".json"))
	if err != nil {
		return nil, err
	}
	saved := strings.Join(rep.Principal.Companies, ",")
	switch *af.companies {
	case "":
		*af.companies = saved
	case saved:
	default:
		return nil, fmt.Errorf("report %s is an analysis of companies %q: continue it without -companies, or ask anew", rep.ID, saved)
	}
	return rep, nil
}

// askOrContinue asks the agent the question, as a follow-up in the
// analysis of the saved report earlier when there is one: its queries
// rerun first, so the agent sees the earlier exchange on current data
// (T94a).
func askOrContinue(ctx context.Context, c *apitools.Client, sess *analytics.Session, earlier *analytics.Report, question string) (*analytics.Answer, error) {
	if earlier == nil {
		return newAgent(c).Ask(ctx, sess, question)
	}
	fresh, _, err := sess.Rerun(ctx, earlier)
	if err != nil {
		return nil, err
	}
	return newAgent(c).Continue(ctx, sess, fresh, question)
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
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	sess, err := af.session(ctx, c)
	if err != nil {
		return err
	}
	fresh, results, err := sess.Rerun(ctx, rep)
	if err != nil {
		return err
	}
	html, err := fresh.Save(*outDir, results)
	if err != nil {
		return err
	}
	return printf("report", "Refreshed %q with data through %s\nDashboard: %s\n", fresh.Spec.Title, sess.DataThrough(), html)
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
			switch v.(type) {
			case string, analytics.Value: // a Value prints exactly, with its own digits
				fmt.Fprintf(tw, "%s\t", v)
			default:
				fmt.Fprint(tw, "—\t")
			}
		}
		fmt.Fprintln(tw)
	}
}
