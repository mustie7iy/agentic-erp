// Command erp is the platform's command line, a thin client of the public
// API (01-requirements §1.1; agents as in 04-api-design §4.13): everything
// it reads or changes goes through erp-server with the caller's token, under
// the same permissions, company access and audit as any other client. The
// only local work is preparing input (a mock SAP extract, a configuration
// draft from SAP customizing) and running agents and analytics, which
// themselves call the API.
//
//	erp call OPERATION [-p name=value]... [-body JSON] [-file field=@F]... [-if-match ETAG] [-out F]
//	erp migrate -connector ID -legal-entities ID,... -year 2026 [-rehearsal]
//	erp invoices [-agent]          match parked invoices, post clean ones
//	erp capture -legal-entity ID -file F   capture an invoice document (large ones through an upload)
//	erp agent invoices|config|migration|test|reconcile|cash-variance|credit-score
//	erp metrics | query | ask | report
//	erp gen-mock-sap | config generate | config validate
//	erp tenants list|get|create|suspend|resume|offboard   (operators: the control plane)
//
// The API is ERP_API_URL (default http://localhost:8080); ERP_TOKEN is the
// person's bearer token and ERP_AGENT_TOKEN the agents' token. The control
// plane is ERP_CONTROL_URL (default http://localhost:9090) with
// ERP_OPERATOR_TOKEN.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/delivery/sapconfig"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

const defaultExtract = "data/sap-extract.json"

// out and errOut are where commands write; tests replace them.
var (
	out    io.Writer = os.Stdout
	errOut io.Writer = os.Stderr
)

// printf writes a command's output and returns a failed write, so a full
// disk or a closed pipe fails the command rather than losing the output (a
// token a script captures) behind exit 0. Output of several writes goes
// through printer instead.
func printf(what, format string, args ...any) error {
	_, err := fmt.Fprintf(out, format, args...)
	return errs.Wrapf(err, "%s: print", what)
}

// printer buffers a command's output of several writes: a bufio.Writer
// keeps the first failed write and refuses the rest, so the error of its
// one Flush, which the command returns, reports any of them.
func printer() *bufio.Writer { return bufio.NewWriter(out) }

// exit ends the process, getenv reads the environment and newRunner makes
// the agents' model client; tests replace them.
var (
	exit      = os.Exit
	getenv    = os.Getenv
	newRunner = gatewayRunner
)

// gatewayRunner calls models through the cell's model gateway as c's
// principal: agents hold no provider key (ADR-0036).
func gatewayRunner(c *apitools.Client) *llm.Runner { return llm.Gateway(c.BaseURL, c.Token, c.HTTP) }

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
	handler, ok := commands[args[0]]
	if !ok {
		usage()
		return 2
	}
	if err := handler(ctx, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(errOut, "error:", err)
		return 1
	}
	return 0
}

type command func(ctx context.Context, args []string) error

var commands map[string]command

func init() {
	commands = map[string]command{
		"call":         call,
		"capture":      captureCmd,
		"directory":    directoryCmd,
		"migrate":      migrate,
		"invoices":     invoices,
		"agent":        agentCmd,
		"metrics":      metricsCmd,
		"query":        queryCmd,
		"ask":          askCmd,
		"report":       reportCmd,
		"gen-mock-sap": genMock,
		"config":       configCmd,
		"token":        token,
		"tenants":      tenantsCmd,
		"version":      versionCmd,
		"eval":         evalCmd,
	}
}

func usage() {
	fmt.Fprintln(errOut, `usage: erp <command> [flags]

the API (ERP_API_URL, ERP_TOKEN):
  call           call any API operation: erp call getPaymentRun -p id=...
  migrate        run a migration scope from a connector: start, reconcile, print the report
  invoices       match parked supplier invoices and post the clean ones (-agent: investigate held ones)
  capture        capture a supplier invoice from a document file (large ones through an upload)
  metrics        list the tenant's governed metrics
  query          run a semantic query over the data you may read
  ask            ask a question; the analytics agent builds a dashboard on demand
  report         rerun a saved report on current data

agents (ERP_AGENT_TOKEN; models through the cell's gateway, ERP_AGENT_MODEL an alias):
  agent invoices | config | migration | test | reconcile -statement ID | close-accruals -close ID
                 | close-variances -close ID [-basis B]
                 | cash-variance -company CODE -period FROM..TO [-lag 7]
                 | tax-draft -registration CODE -period YYYY-MM-DD
                 | credit-score -company CODE -customer ID

agent evaluations (ADR-0039; live, on demand: they spend model money):
  eval run -suite evals/<agent>/suite.yaml [-repeats 3] [-max-cost 5.00] [-out evals/results]
                 throwaway tenants on the local cell (ERP_CONTROL_URL, ERP_API_URL, ERP_ADMIN_URL with ERP_DEV_ISSUER)

operators (ERP_CONTROL_URL, default localhost:9090; ERP_OPERATOR_TOKEN):
  tenants list | get | create | suspend | resume | offboard | rotate-key | erase   the control plane: the tenants of the cell
  directory cells | tenants | locate | create | freeze | unfreeze | move   the global control plane (ERP_DIRECTORY_API_URL, default localhost:9091): tenants across cells

local preparation:
  gen-mock-sap   write a deterministic mock SAP extract
  config generate | validate   draft a configuration from SAP customizing, or check one
  token          mint a token from a server running with ERP_DEV_ISSUER (ERP_ADMIN_URL, default localhost:9090);
                 without -tenant it is an operator token
  version        print this build's version and source revision

run "erp <command> -h" for flags`)
}

// versionCmd prints the build (ADR-0032) as the servers serve it on their
// admin listener's /version.
func versionCmd(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("version takes no arguments")
	}
	b, _ := json.Marshal(buildinfo.Get()) // strings and a bool always encode
	return printf("version", "%s\n", b)
}

// client is an API client with the token in the named variable.
func client(tokenVar string) (*apitools.Client, error) {
	token := getenv(tokenVar)
	if token == "" {
		return nil, fmt.Errorf("%s is not set", tokenVar)
	}
	base := getenv("ERP_API_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	return &apitools.Client{BaseURL: base, Token: token}, nil
}

// parseFlags parses a command's flags, naming the command in the error. The
// flag package has already printed the problem and the usage; a -h request
// stays flag.ErrHelp underneath, which run treats as success.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%s: %w", fs.Name(), err)
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, "; ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// callInput is the operation's input: the parameters (a comma makes a
// list), the ETag and the body, read from a file when it starts with @.
func callInput(operation string, params multiFlag, ifMatch, body string) (map[string]any, error) {
	var err error
	input := map[string]any{}
	for _, p := range params {
		name, value, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("parameter %q: want name=value", p)
		}
		input[name] = value
		if strings.Contains(value, ",") {
			var list []any // as a list decoded from JSON, which the tool takes
			for _, v := range strings.Split(value, ",") {
				list = append(list, v)
			}
			input[name] = list
		}
	}
	if ifMatch != "" {
		input["if_match"] = ifMatch
	}
	if body != "" {
		raw := []byte(body)
		if path, ok := strings.CutPrefix(body, "@"); ok {
			if raw, err = os.ReadFile(path); err != nil { // #nosec G703 -- a file the user names on the command line
				return nil, fmt.Errorf("call %s: read body: %w", operation, err)
			}
		}
		var b any
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
		input["body"] = b
	}
	return input, nil
}

// call performs one API operation, named by its operationId, and prints the
// answer. Parameters are name=value; a body is JSON, or @file; -file
// field=@path adds a file's bytes to the body as that field, which a
// multipart operation (a bank statement upload) sends as a part of its
// form. A JSON answer is printed indented, any other (a pain.001 file)
// as it came; -out writes the answer's body to a file instead.
func call(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("name the operation: erp call OPERATION [flags]")
	}
	fs := flag.NewFlagSet("call "+args[0], flag.ContinueOnError)
	var params, files multiFlag
	fs.Var(&params, "p", "parameter name=value, repeatable (lists are comma-separated)")
	body := fs.String("body", "", "request body: JSON, or @file")
	fs.Var(&files, "file", "body field=@file, repeatable: the file's bytes as that field (a multipart upload's file)")
	ifMatch := fs.String("if-match", "", "ETag of the resource as last read")
	outFile := fs.String("out", "", "write the answer's body to this file")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	op, err := apitools.Operation(c, args[0])
	if err != nil {
		return err
	}
	input, err := callInput(args[0], params, *ifMatch, *body)
	if err == nil {
		err = addFiles(args[0], input, files)
	}
	if err != nil {
		return err
	}
	r, err := op.Call(ctx, input)
	if err != nil {
		return err
	}
	if err := printAnswer(r, *outFile); err != nil {
		return err
	}
	if r.Status >= 400 {
		return fmt.Errorf("the API answered %d", r.Status)
	}
	return nil
}

// addFiles puts each -file field=@path's bytes into the body as the field.
func addFiles(operation string, input map[string]any, files multiFlag) error {
	if len(files) == 0 {
		return nil
	}
	fields, ok := input["body"].(map[string]any)
	if _, has := input["body"]; has && !ok {
		return fmt.Errorf("call %s: -file needs a body that is a JSON object", operation)
	}
	if fields == nil {
		fields = map[string]any{}
	}
	for _, f := range files {
		name, path, ok := strings.Cut(f, "=")
		if !ok || name == "" {
			return fmt.Errorf("-file %q: want field=@path", f)
		}
		b, err := os.ReadFile(strings.TrimPrefix(path, "@")) // #nosec G304 G703 -- a file the user names on the command line
		if err != nil {
			return fmt.Errorf("call %s: read file of %s: %w", operation, name, err)
		}
		fields[name] = string(b)
	}
	input["body"] = fields
	return nil
}

// printAnswer prints the status and ETag, then the body: written to outFile
// when one is named, indented when it is JSON, else as it came.
func printAnswer(r apitools.Response, outFile string) error {
	if outFile != "" {
		if err := os.WriteFile(outFile, r.Body, 0o600); err != nil { // #nosec G703 -- a file the user names on the command line
			return fmt.Errorf("call: write %s: %w", outFile, err)
		}
		return printf("call", "%d %s\nwrote %d bytes to %s\n", r.Status, r.ETag, len(r.Body), outFile)
	}
	shown := []byte(r.Body)
	var indented bytes.Buffer
	switch {
	case len(r.Body) == 0:
		shown = []byte("null")
	case json.Indent(&indented, r.Body, "", "  ") == nil:
		shown = indented.Bytes()
	}
	return printf("call", "%d %s\n%s\n", r.Status, r.ETag, bytes.TrimRight(shown, "\n"))
}

// token asks a development server for a bearer token: erp token -tenant ID
// -subject SUBJECT prints it, for ERP_TOKEN or ERP_AGENT_TOKEN.
func token(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant id; without it the token is an operator's")
	subject := fs.String("subject", "", "the principal's (or operator's) subject at the identity provider")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *subject == "" {
		return errors.New("-subject is required")
	}
	tok, err := eval.DevIssuer(orDefault(getenv("ERP_ADMIN_URL"), "http://localhost:9090"), nil)(ctx, *tenant, *subject)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	return printf("token", "%s\n", tok)
}

func genMock(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("gen-mock-sap", flag.ContinueOnError)
	outFile := fs.String("out", defaultExtract, "output file")
	seed := fs.Uint64("seed", 42, "random seed")
	year := fs.String("year", "2026", "fiscal year")
	through := fs.Int("through", 12, "last month with revenue postings, for a year in progress")
	if err := parseFlags(fs, args); err != nil {
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
	w := printer()
	fmt.Fprintf(w, "wrote %s\n", *outFile)
	counts := e.Counts()
	tables := make([]string, 0, len(counts))
	for t := range counts {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, t := range tables {
		fmt.Fprintf(tw, "  %s\t%d\n", t, counts[t])
	}
	_ = tw.Flush() // its only error is a failed write, which w keeps
	return errs.Wrapf(w.Flush(), "gen-mock-sap: print counts")
}

func configCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("config generate | validate")
	}
	switch args[0] {
	case "generate":
		return configGenerate(ctx, args[1:])
	case "validate":
		return configValidate(args[1:])
	}
	return fmt.Errorf("unknown config command %q", args[0])
}

func parseSource(s string) (sap.Source, error) {
	kind, target, ok := strings.Cut(s, ":")
	switch {
	case ok && kind == "file":
		return sap.FileSource{Path: target}, nil
	case ok && kind == "odata":
		return &sap.ODataSource{BaseURL: target, User: getenv("SAP_USER"), Password: getenv("SAP_PASSWORD"), Client: getenv("SAP_CLIENT")}, nil
	}
	return nil, fmt.Errorf("invalid -source %q: want file:PATH or odata:URL", s)
}

// externalAmounts says whether a source's amounts are in SAP's external
// form: an OData source's always, a file extract's when -amount-format
// says so (docs/migration.md, "Amounts in SAP's internal form").
func externalAmounts(src sap.Source, form string) (bool, error) {
	_, odata := src.(*sap.ODataSource)
	switch {
	case form == "":
		return odata, nil
	case form == "external":
		return true, nil
	case form == "internal" && !odata:
		return false, nil
	}
	return false, fmt.Errorf(`invalid -amount-format %q: want "internal" or "external" for a file extract; OData's amounts are external`, form)
}

// sapDate is a -cutover date in SAP's form, YYYYMMDD ("" when none).
func sapDate(d string) (string, error) {
	if d == "" {
		return "", nil
	}
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return "", fmt.Errorf("invalid -cutover %q: want YYYY-MM-DD", d)
	}
	return t.Format("20060102"), nil
}

// configGenerate drafts a configuration document from SAP customizing, for
// a change request (erp agent config -draft, or erp call createChangeRequest).
func configGenerate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config generate", flag.ContinueOnError)
	source := fs.String("source", "file:"+defaultExtract, `SAP source: "file:PATH" or "odata:URL"`)
	companies := fs.String("company", "1000", "comma-separated company codes")
	year := fs.String("year", "2026", "fiscal year (for the extract)")
	outPath := fs.String("out", "data/config-draft.yaml", "where to write the draft")
	zones := fs.String("time-zone", "", "comma-separated CODE=ZONE, each company's IANA time zone (e.g. 3000=America/Toronto); SAP's extract has none")
	form := fs.String("amount-format", "", `the form of a file extract's amounts: "internal" (the default, SAP's stored form, converted with the extract's TCURX) or "external"; OData's are external`)
	cutover := fs.String("cutover", "", "the cutover date (YYYY-MM-DD), at which each account's cost element is read from SAP's CSKB slice valid then; without it, the latest slice")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	src, err := parseSource(*source)
	if err != nil {
		return err
	}
	opts := sapconfig.DefaultOptions()
	if opts.ExternalAmounts, err = externalAmounts(src, *form); err != nil {
		return err
	}
	if opts.TimeZones, err = parseZones(*zones); err != nil {
		return err
	}
	if opts.Cutover, err = sapDate(*cutover); err != nil {
		return err
	}
	e, err := src.Extract(ctx, sap.Scope{CompanyCodes: strings.Split(*companies, ","), FiscalYear: *year})
	if err != nil {
		return err
	}
	cfg, notes := sapconfig.Generate(e, opts)
	doc, _ := cfg.Marshal() // Config.Marshal cannot fail
	if err := os.WriteFile(*outPath, doc, 0o600); err != nil {
		return fmt.Errorf("config generate: write draft %s: %w", *outPath, err)
	}
	w := printer()
	fmt.Fprintf(w, "Draft configuration written to %s\n\nNotes for reviewers:\n", *outPath)
	for _, n := range notes {
		fmt.Fprintln(w, "  "+n.String())
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("config generate: print notes: %w", err)
	}
	return printFindings(config.Validate(cfg, currency.Seeded()))
}

// parseZones reads -time-zone: the zones are checked by the draft's
// validation, which names the company of one that does not load.
func parseZones(s string) (map[string]string, error) {
	zones := map[string]string{}
	if s == "" {
		return zones, nil
	}
	for _, pair := range strings.Split(s, ",") {
		code, zone, ok := strings.Cut(pair, "=")
		if !ok || code == "" || zone == "" {
			return nil, fmt.Errorf("invalid -time-zone %q: want CODE=ZONE, e.g. 3000=America/Toronto", pair)
		}
		zones[code] = zone
	}
	return zones, nil
}

func configValidate(args []string) error {
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	file := fs.String("file", "", "configuration YAML")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := config.Load(*file)
	if err != nil {
		return err
	}
	return printFindings(config.Validate(c, currency.Seeded())) // offline: the exponents the migrations seed
}

func printFindings(fs []config.Finding) error {
	if len(fs) == 0 {
		return printf("config", "\nValidation: no findings\n")
	}
	w := printer()
	fmt.Fprintln(w, "\nValidation:")
	for _, f := range fs {
		fmt.Fprintln(w, "  "+f.String())
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("config: print findings: %w", err)
	}
	if config.HasErrors(fs) {
		return errors.New("the configuration has validation errors")
	}
	return nil
}
