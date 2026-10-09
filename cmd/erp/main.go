// Command erp drives the migration off SAP and the agentic invoice flow.
//
//	erp gen-mock-sap                     write a mock SAP extract
//	erp migrate                          extract, check, load and reconcile a scope
//	erp invoices [-agent]                process parked invoices
//	erp tasks                            list open human review tasks
//	erp approve|reject -task ID -as USER close a review task
//	erp tb -company 1000                 print a trial balance
//	erp audit-verify                     verify the audit hash chain
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/mustie7iy/agentic-erp/internal/agent"
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

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "gen-mock-sap":
		err = genMock(args)
	case "migrate":
		err = migrate(ctx, args)
	case "invoices":
		err = invoices(ctx, args)
	case "tasks":
		err = tasks(args)
	case "approve", "reject":
		err = decide(args, cmd == "approve")
	case "tb":
		err = trialBalance(args)
	case "audit-verify":
		err = auditVerify(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: erp <command> [flags]

commands:
  gen-mock-sap   write a deterministic mock SAP extract
  migrate        extract -> data-quality gate -> transform -> load -> reconcile
  invoices       process parked invoices (rule posts clean ones, -agent investigates exceptions)
  tasks          list open human review tasks
  approve        approve a review task as a human (posts the invoice)
  reject         reject a review task as a human
  tb             print a trial balance
  audit-verify   verify the tamper-evident audit log

run "erp <command> -h" for flags`)
}

func genMock(args []string) error {
	fs := flag.NewFlagSet("gen-mock-sap", flag.ExitOnError)
	out := fs.String("out", defaultExtract, "output file")
	seed := fs.Uint64("seed", 42, "random seed")
	year := fs.String("year", "2026", "fiscal year")
	fs.Parse(args)
	e := sap.GenerateMock(*seed, *year)
	if err := sap.WriteExtract(*out, e); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	printCounts(e.Counts())
	return nil
}

func migrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	source := fs.String("source", "file:"+defaultExtract, `"file:PATH" or "odata:https://host" (credentials from SAP_USER, SAP_PASSWORD, SAP_CLIENT)`)
	companies := fs.String("company", "1000", "comma-separated company codes")
	year := fs.String("year", "2026", "fiscal year")
	mode := fs.String("mode", string(migration.ModeDocuments), `"documents" (replay every journal document) or "balances" (opening balances + open items)`)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	force := fs.Bool("force", false, "load despite data-quality errors (rehearsals only)")
	jsonOut := fs.String("json", "", "also write the full result as JSON to this file")
	fs.Parse(args)

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

	fmt.Printf("Source: %s   Scope: company %s, fiscal year %s   Mode: %s\n\n",
		res.Source, strings.Join(scope.CompanyCodes, ","), scope.FiscalYear, res.Mode)
	if res.ExtractCount != nil {
		printCounts(res.ExtractCount)
	}
	printIssues("Data-quality findings", res.Quality)
	printIssues("Transform issues", res.Transform)
	printIssues("Load rejections", res.Load)
	if *jsonOut != "" {
		if b, err := json.MarshalIndent(res, "", "  "); err == nil {
			_ = os.WriteFile(*jsonOut, b, 0o644)
		}
	}
	if runErr != nil {
		if errors.Is(runErr, migration.ErrQualityGate) {
			fmt.Println("\nNothing was loaded.")
		}
		return runErr
	}
	fmt.Println()
	res.Report.Print(os.Stdout)
	if err := store.Save(*storePath); err != nil {
		return err
	}
	fmt.Printf("\nStore saved to %s\n", *storePath)
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
	fs := flag.NewFlagSet("invoices", flag.ExitOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	auditPath := fs.String("audit", defaultAudit, "audit log")
	useAgent := fs.Bool("agent", false, "let the Claude exception agent investigate failed matches")
	fs.Parse(args)

	svc, err := openService(*storePath, *auditPath)
	if err != nil {
		return err
	}
	var resolver agent.Resolver
	if *useAgent {
		resolver = agent.NewClaudeResolver()
	}
	sum := agent.ProcessInvoices(ctx, svc, resolver, os.Stdout)
	fmt.Printf("\nprocessed %d: %d posted, %d sent to human review (%d with agent recommendation)\n",
		sum.Processed, len(sum.Posted), len(sum.Escalated), len(sum.AgentNotes))
	for _, e := range sum.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	return svc.Store.Save(*storePath)
}

func tasks(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	fs.Parse(args)
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
		fmt.Printf("%s  invoice %s  vendor %s  gross %s %s\n  reason: %s\n", t.ID, t.InvoiceID, inv.VendorID, inv.GrossAmount, inv.Currency, t.Reason)
		if t.Recommendation != "" {
			fmt.Printf("  recommendation (%s):\n    %s\n", t.CreatedBy, strings.ReplaceAll(t.Recommendation, "\n", "\n    "))
		}
		fmt.Println()
	}
	fmt.Printf("%d open review tasks\n", n)
	return nil
}

func decide(args []string, approve bool) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	auditPath := fs.String("audit", defaultAudit, "audit log")
	taskID := fs.String("task", "", "review task id")
	user := fs.String("as", "", "approver user id (recorded in the audit log)")
	comment := fs.String("comment", "", "decision comment")
	fs.Parse(args)
	if *taskID == "" || *user == "" {
		return fmt.Errorf("-task and -as are required")
	}
	svc, err := openService(*storePath, *auditPath)
	if err != nil {
		return err
	}
	human := governance.Identity{
		ID: "user:" + *user, Kind: "human", Autonomy: governance.FullyAutonomous,
		Permissions: []string{governance.ActInvoicePost, governance.ActInvoiceReview},
	}
	res, err := svc.Decide(human, *taskID, approve, *comment)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s %s\n", res.Outcome, res.Reason, res.JournalEntryID)
	return svc.Store.Save(*storePath)
}

func trialBalance(args []string) error {
	fs := flag.NewFlagSet("tb", flag.ExitOnError)
	storePath := fs.String("store", defaultStore, "core store snapshot")
	company := fs.String("company", "1000", "company code")
	asOf := fs.String("as-of", "", "posting date YYYY-MM-DD (default: all)")
	fs.Parse(args)
	s, err := core.LoadStore(*storePath)
	if err != nil {
		return err
	}
	tb := s.TrialBalance(*company, *asOf)
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', tabwriter.AlignRight)
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
	fs := flag.NewFlagSet("audit-verify", flag.ExitOnError)
	auditPath := fs.String("audit", defaultAudit, "audit log")
	fs.Parse(args)
	log, err := governance.NewAuditLog(*auditPath) // loading verifies the chain
	if err != nil {
		return err
	}
	fmt.Printf("audit log OK: %d events, chain intact\n", len(log.Events()))
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
	return agent.NewService(s, audit), nil
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
	fmt.Println("Extracted rows:", strings.Join(parts, " "))
}

func printIssues(title string, issues []migration.Issue) {
	if len(issues) == 0 {
		return
	}
	fmt.Printf("\n%s (%d):\n", title, len(issues))
	for _, i := range issues {
		fmt.Println("  " + i.String())
	}
}
