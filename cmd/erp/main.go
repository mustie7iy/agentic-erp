// Command erp drives the migration off SAP, configuration as code with its
// delivery agents, procure-to-pay, the agentic invoice flow and on-demand
// analytics. Run "erp" for the command list.
//
//	erp gen-mock-sap                     write a mock SAP extract
//	erp migrate                          extract, check, load and reconcile a scope
//	erp invoices [-agent]                process parked invoices
//	erp tasks                            list open human review tasks
//	erp approve|reject -task ID -as USER close a review task
//	erp tb -company 1000                 print a trial balance
//	erp audit-verify                     verify the audit hash chain
//	erp metrics                          list governed metrics
//	erp query -measures ... -by ...      run a semantic query directly
//	erp ask "question"                   answer with an on-demand dashboard
//	erp report -id ID                    rerun a saved report on current data
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

const (
	defaultExtract = "data/sap-extract.json"
	defaultStore   = "data/store.json"
	defaultAudit   = "data/audit.jsonl"
)

// out and errOut are where commands write; tests replace them.
var (
	out    io.Writer = os.Stdout
	errOut io.Writer = os.Stderr
)

// exit ends the process; tests replace it to observe the exit code.
var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:])
	stop()
	exit(code)
}

// run executes one command and returns the process exit code.
func run(ctx context.Context, args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	cmd, args := args[0], args[1:]
	handler, ok := commands[cmd]
	if !ok {
		usage()
		return 2
	}
	if err := handler(ctx, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(errOut, "error:", err)
		return 1
	}
	return 0
}

type command func(ctx context.Context, args []string) error

// commands maps each command name to its handler.
var commands map[string]command

func init() {
	plain := func(f func([]string) error) command { return func(_ context.Context, a []string) error { return f(a) } }
	commands = map[string]command{
		"gen-mock-sap": plain(genMock),
		"migrate":      migrate,
		"invoices":     invoices,
		"tasks":        plain(tasks),
		"approve":      plain(func(a []string) error { return decide(a, true) }),
		"reject":       plain(func(a []string) error { return decide(a, false) }),
		"tb":           plain(trialBalance),
		"audit-verify": plain(auditVerify),
		"metrics":      plain(metricsCmd),
		"query":        plain(queryCmd),
		"ask":          askCmd,
		"report":       plain(reportCmd),
	}
	for name, c := range platformCommands() {
		commands[name] = c
	}
}

func usage() {
	fmt.Fprintln(errOut, `usage: erp <command> [flags]

commands:
  gen-mock-sap   write a deterministic mock SAP extract
  migrate        extract -> data-quality gate -> transform -> load -> reconcile
  invoices       process parked invoices (rule posts clean ones, -agent investigates exceptions)
  tasks          list open human review tasks
  approve        approve a review task as a human (posts the invoice)
  reject         reject a review task as a human
  tb             print a trial balance
  audit-verify   verify the tamper-evident audit log
  metrics        list the governed metrics and dimensions
  query          run a semantic query directly (no agent)
  ask            ask a question; the analytics agent builds a dashboard on demand
  report         rerun a saved report on current data

configuration as code (Phase 1 delivery track):
  config generate|validate|bootstrap|show|propose|evidence|approve|reject|activate|rollback|list|history|diff
  regress capture|run       regression suites from real transactions
  agent config|migration|test   delivery agents (need Anthropic credentials)

procure-to-pay (Phase 1 business track):
  req create|approve|reject     purchase requisitions with configured approval routes
  po create                     order an approved requisition (sends PO output)
  contract create               outline agreements with agreed prices
  gr post                       goods receipts and reversals
  vendor create|bank|block|unblock|approve|reject   governed supplier master changes
  pay propose|release|reject    payment runs with ISO 20022 bank files
  capture                       read a supplier invoice PDF (Claude) and park it

run "erp <command> -h" for flags`)
}

func genMock(args []string) error {
	fs := flag.NewFlagSet("gen-mock-sap", flag.ContinueOnError)
	outFile := fs.String("out", defaultExtract, "output file")
	seed := fs.Uint64("seed", 42, "random seed")
	year := fs.String("year", "2026", "fiscal year")
	through := fs.Int("through", 12, "last month with revenue postings, for a year in progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Without an explicit seed, vary the random stream by year so that two
	// years of mock data differ realistically (2026 keeps seed 42).
	seedSet := false
	fs.Visit(func(f *flag.Flag) { seedSet = seedSet || f.Name == "seed" })
	if !seedSet {
		if y, err := strconv.Atoi(*year); err == nil {
			*seed = uint64(int64(42) + int64(y) - 2026)
		}
	}
	e := sap.GenerateMockThrough(*seed, *year, *through)
	if err := sap.WriteExtract(*outFile, e); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", *outFile)
	printCounts(e.Counts())
	return nil
}

func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	source := fs.String("source", "file:"+defaultExtract, `"file:PATH" or "odata:https://host" (credentials from SAP_USER, SAP_PASSWORD, SAP_CLIENT)`)
	companies := fs.String("company", "1000", "comma-separated company codes")
	year := fs.String("year", "2026", "fiscal year")
	mode := fs.String("mode", string(migration.ModeDocuments), `"documents" (replay every journal document) or "balances" (opening balances + open items)`)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	force := fs.Bool("force", false, "load despite data-quality errors (rehearsals only)")
	jsonOut := fs.String("json", "", "also write the full result as JSON to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	src, err := parseSource(*source)
	if err != nil {
		return err
	}
	store, err := openStore(*storePath)
	if err != nil {
		return err
	}
	scope := sap.Scope{CompanyCodes: strings.Split(*companies, ","), FiscalYear: *year}
	res, runErr := migration.Run(ctx, src, scope, migration.Options{Mode: migration.Mode(*mode), Force: *force}, store)

	fmt.Fprintf(out, "Source: %s   Scope: company %s, fiscal year %s   Mode: %s\n\n",
		res.Source, strings.Join(scope.CompanyCodes, ","), scope.FiscalYear, res.Mode)
	if res.ExtractCount != nil {
		printCounts(res.ExtractCount)
	}
	printIssues("Data-quality findings", res.Quality)
	printIssues("Transform issues", res.Transform)
	printIssues("Load rejections", res.Load)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ") // the result is plain data
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *jsonOut, err)
		}
	}
	if runErr != nil {
		if errors.Is(runErr, migration.ErrQualityGate) {
			fmt.Fprintln(out, "\nNothing was loaded.")
		}
		return runErr
	}
	fmt.Fprintln(out)
	res.Report.Print(out)
	if err := store.Save(*storePath); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nStore saved to %s\n", *storePath)
	if !res.Report.Passed || len(res.Load) > 0 {
		return fmt.Errorf("reconciliation did not pass")
	}
	return nil
}

func parseSource(s string) (sap.Source, error) {
	kind, target, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("invalid -source %q", s)
	}
	switch kind {
	case "file":
		return sap.FileSource{Path: target}, nil
	case "odata":
		return &sap.ODataSource{
			BaseURL: target, User: os.Getenv("SAP_USER"), Password: os.Getenv("SAP_PASSWORD"), Client: os.Getenv("SAP_CLIENT"),
		}, nil
	}
	return nil, fmt.Errorf("unknown source kind %q", kind)
}

func invoices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invoices", flag.ContinueOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	auditPath := fs.String("audit", defaultAudit, "audit log")
	useAgent := fs.Bool("agent", false, "let the Claude exception agent investigate failed matches")
	if err := fs.Parse(args); err != nil {
		return err
	}

	svc, err := openService(*storePath, *auditPath)
	if err != nil {
		return err
	}
	var resolver agent.Resolver
	if *useAgent {
		resolver = agent.NewClaudeResolver()
	}
	sum := agent.ProcessInvoices(ctx, svc, resolver, out)
	fmt.Fprintf(out, "\nprocessed %d: %d posted, %d sent to human review (%d with agent recommendation)\n",
		sum.Processed, len(sum.Posted), len(sum.Escalated), len(sum.AgentNotes))
	for _, e := range sum.Errors {
		fmt.Fprintln(errOut, "error:", e)
	}
	return svc.Store.Save(*storePath)
}

func tasks(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := core.LoadStore(*storePath)
	if err != nil {
		return err
	}
	n := 0
	for _, id := range core.SortedKeys(s.Tasks) {
		t := s.Tasks[id]
		if t.Status != core.TaskOpen {
			continue
		}
		n++
		inv := s.Invoices[t.InvoiceID]
		fmt.Fprintf(out, "%s  invoice %s  vendor %s  gross %s %s\n  reason: %s\n", t.ID, t.InvoiceID, inv.VendorID, inv.GrossAmount, inv.Currency, t.Reason)
		if t.Recommendation != "" {
			fmt.Fprintf(out, "  recommendation (%s):\n    %s\n", t.CreatedBy, strings.ReplaceAll(t.Recommendation, "\n", "\n    "))
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "%d open review tasks\n", n)
	return nil
}

func decide(args []string, approve bool) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	auditPath := fs.String("audit", defaultAudit, "audit log")
	taskID := fs.String("task", "", "review task id")
	user := fs.String("as", "", "approver user id (recorded in the audit log)")
	comment := fs.String("comment", "", "decision comment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" || *user == "" {
		return fmt.Errorf("-task and -as are required")
	}
	svc, err := openService(*storePath, *auditPath)
	if err != nil {
		return err
	}
	// With an active configuration the approver is a configured user with
	// their configured permissions; without one, any named person may decide.
	human := governance.Identity{
		ID: "user:" + *user, Kind: "human", Autonomy: governance.FullyAutonomous,
		Permissions: []string{governance.ActInvoicePost, governance.ActInvoiceReview},
	}
	if svc.Config != nil {
		if human, err = svc.Config.Identity(*user); err != nil {
			return err
		}
	}
	res, err := svc.Decide(human, *taskID, approve, *comment)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: %s %s\n", res.Outcome, res.Reason, res.JournalEntryID)
	return svc.Store.Save(*storePath)
}

func trialBalance(args []string) error {
	fs := flag.NewFlagSet("tb", flag.ContinueOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	company := fs.String("company", "1000", "company code")
	asOf := fs.String("as-of", "", "posting date YYYY-MM-DD (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := core.LoadStore(*storePath)
	if err != nil {
		return err
	}
	tb := s.TrialBalance(*company, *asOf)
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "ACCOUNT\tNAME\tBALANCE\t")
	var total int64
	for _, acct := range core.SortedKeys(tb) {
		name := ""
		if a := s.Account(*company, acct); a != nil {
			name = a.Name
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t\n", acct, name, tb[acct])
		total += int64(tb[acct])
	}
	fmt.Fprintf(tw, "\tTOTAL (must be 0)\t%d\t\n", total)
	return tw.Flush()
}

func auditVerify(args []string) error {
	fs := flag.NewFlagSet("audit-verify", flag.ContinueOnError)
	auditPath := fs.String("audit", defaultAudit, "audit log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log, err := governance.NewAuditLog(*auditPath) // loading verifies the chain
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "audit log OK: %d events, chain intact\n", len(log.Events()))
	return nil
}

func openStore(path string) (*core.Store, error) {
	s, err := core.LoadStore(path)
	if errors.Is(err, os.ErrNotExist) {
		return core.NewStore(), nil
	}
	return s, err
}

func openService(storePath, auditPath string) (*agent.Service, error) {
	s, err := core.LoadStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("%w (run \"erp migrate\" first)", err)
	}
	audit, err := governance.NewAuditLog(auditPath)
	if err != nil {
		return nil, err
	}
	svc := agent.NewService(s, audit)
	// Use the active configuration's tolerances, tax accounts and limits when there is one.
	if repo, err := config.OpenRepo(defaultConfigDir, nil); err == nil {
		if cfg, _, err := repo.Active(); err == nil {
			svc.Config = cfg
		}
	}
	return svc, nil
}

func printCounts(c map[string]int) {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	parts := make([]string, 0, len(keys))
	for _, k := range core.SortedKeys(c) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c[k]))
	}
	fmt.Fprintln(out, "Extracted rows:", strings.Join(parts, " "))
}

func printIssues(title string, issues []migration.Issue) {
	if len(issues) == 0 {
		return
	}
	fmt.Fprintf(out, "\n%s (%d):\n", title, len(issues))
	for _, i := range issues {
		fmt.Fprintln(out, "  "+i.String())
	}
}
