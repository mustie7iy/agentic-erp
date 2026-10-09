package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/changes"
	"github.com/mustie7iy/agentic-erp/internal/delivery/configagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/migrationagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/regress"
	"github.com/mustie7iy/agentic-erp/internal/delivery/sapconfig"
	"github.com/mustie7iy/agentic-erp/internal/delivery/testagent"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/p2p"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

const (
	defaultConfigDir = "data/config"
	defaultSuiteDir  = "data/suites"
	defaultUATDir    = "data/uat"
	defaultOutbox    = "data/outbox"
	defaultBankDir   = "data/bankfiles"
)

// clock is replaceable so end-to-end tests are deterministic.
var clock = time.Now

// env is the state a platform command works on.
type env struct {
	storePath string
	store     *core.Store
	audit     *governance.AuditLog
	repo      *config.Repo
}

type envFlags struct{ store, audit, config *string }

func addEnvFlags(fs *flag.FlagSet) envFlags {
	return envFlags{
		store:  fs.String("store", defaultStore, "core store snapshot"),
		audit:  fs.String("audit", defaultAudit, "audit log"),
		config: fs.String("config-dir", defaultConfigDir, "configuration repository"),
	}
}

func (f envFlags) open() (*env, error) {
	audit, err := governance.NewAuditLog(*f.audit)
	if err != nil {
		return nil, err
	}
	repo, err := config.OpenRepo(*f.config, audit)
	if err != nil {
		return nil, err
	}
	repo.SetClock(clock)
	s, err := openStore(*f.store)
	if err != nil {
		return nil, err
	}
	return &env{storePath: *f.store, store: s, audit: audit, repo: repo}, nil
}

func (e *env) save() error { return e.store.Save(e.storePath) }

func (e *env) config() (*config.Config, error) {
	c, _, err := e.repo.Active()
	return c, err
}

func (e *env) p2p() *p2p.Service {
	return &p2p.Service{Store: e.store, Config: e.config, Audit: e.audit, Now: clock}
}

// subcommands dispatches "erp <group> <sub> ...".
func subcommands(group string, subs map[string]func([]string) error) command {
	return func(_ context.Context, args []string) error {
		if len(args) == 0 || subs[args[0]] == nil {
			names := make([]string, 0, len(subs))
			for n := range subs {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("usage: erp %s <%s>", group, strings.Join(names, "|"))
		}
		return subs[args[0]](args[1:])
	}
}

func platformCommands() map[string]command {
	return map[string]command{
		"config": subcommands("config", map[string]func([]string) error{
			"generate": configGenerate, "bootstrap": configBootstrap, "show": configShow, "validate": configValidate,
			"propose": configPropose, "evidence": configEvidence, "approve": configDecide(true), "reject": configDecide(false),
			"activate": configActivate, "rollback": configRollback, "list": configList, "history": configHistory, "diff": configDiff,
		}),
		"regress": subcommands("regress", map[string]func([]string) error{"capture": regressCapture, "run": regressRun}),
		"req": subcommands("req", map[string]func([]string) error{
			"create": reqCreate, "approve": reqDecide(true), "reject": reqDecide(false),
		}),
		"po":       subcommands("po", map[string]func([]string) error{"create": poCreate}),
		"contract": subcommands("contract", map[string]func([]string) error{"create": contractCreate}),
		"gr":       subcommands("gr", map[string]func([]string) error{"post": grPost}),
		"vendor": subcommands("vendor", map[string]func([]string) error{
			"create": vendorCreate, "bank": vendorBank, "block": vendorBlock, "unblock": vendorUnblock,
			"approve": vendorDecide(true), "reject": vendorDecide(false),
		}),
		"pay": subcommands("pay", map[string]func([]string) error{
			"propose": payPropose, "release": payDecide(true), "reject": payDecide(false),
		}),
		"capture": captureCmd,
		"agent": func(ctx context.Context, args []string) error {
			return subcommands("agent", map[string]func([]string) error{
				"config":    func(a []string) error { return agentConfig(ctx, a) },
				"migration": func(a []string) error { return agentMigration(ctx, a) },
				"test":      func(a []string) error { return agentTest(ctx, a) },
			})(ctx, args)
		},
	}
}

func required(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			return fmt.Errorf("-%s is required", pairs[i])
		}
	}
	return nil
}

// --- configuration ---------------------------------------------------------

func configGenerate(args []string) error {
	fs := flag.NewFlagSet("config generate", flag.ContinueOnError)
	source := fs.String("source", "file:"+defaultExtract, `SAP source: "file:PATH" or "odata:URL"`)
	companies := fs.String("company", "1000", "comma-separated company codes")
	year := fs.String("year", "2026", "fiscal year (for the extract)")
	outPath := fs.String("out", "data/config-draft.yaml", "where to write the draft")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src, err := parseSource(*source)
	if err != nil {
		return err
	}
	e, err := src.Extract(context.Background(), sap.Scope{CompanyCodes: strings.Split(*companies, ","), FiscalYear: *year})
	if err != nil {
		return err
	}
	cfg, notes := sapconfig.Generate(e, sapconfig.DefaultOptions())
	doc, _ := cfg.Marshal() // Config.Marshal cannot fail
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*outPath, doc, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Draft configuration written to %s\n\nNotes for reviewers:\n", *outPath)
	for _, n := range notes {
		fmt.Fprintln(out, "  "+n.String())
	}
	return printFindings(config.Validate(cfg))
}

func printFindings(fs []config.Finding) error {
	if len(fs) == 0 {
		fmt.Fprintln(out, "\nValidation: no findings")
		return nil
	}
	fmt.Fprintln(out, "\nValidation:")
	for _, f := range fs {
		fmt.Fprintln(out, "  "+f.String())
	}
	if config.HasErrors(fs) {
		return errors.New("the configuration has validation errors")
	}
	return nil
}

func configBootstrap(args []string) error {
	fs := flag.NewFlagSet("config bootstrap", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	file := fs.String("file", "", "configuration YAML")
	as := fs.String("as", "", "person installing the first configuration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("file", *file, "as", *as); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	doc, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	n, findings, err := e.repo.Bootstrap(doc, "user:"+*as)
	if err != nil {
		printFindings(findings)
		return err
	}
	fmt.Fprintf(out, "Configuration version %d is active.\n", n)
	return nil
}

func configShow(args []string) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	version := fs.Int("version", 0, "version to show (default: active)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	v := *version
	if v == 0 {
		if _, v, err = e.repo.Active(); err != nil {
			return err
		}
	}
	doc, err := e.repo.VersionYAML(v)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "# version %d\n%s", v, doc)
	return nil
}

func configValidate(args []string) error {
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	file := fs.String("file", "", "configuration YAML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("file", *file); err != nil {
		return err
	}
	cfg, err := config.Load(*file)
	if err != nil {
		return err
	}
	return printFindings(config.Validate(cfg))
}

func latestSuite() (*regress.Suite, error) { return regress.Latest(defaultSuiteDir) }

func configPropose(args []string) error {
	fs := flag.NewFlagSet("config propose", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	file := fs.String("file", "", "proposed configuration YAML")
	title := fs.String("title", "", "change title")
	rationale := fs.String("rationale", "", "why")
	as := fs.String("as", "", "configured user or agent proposing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("file", *file, "title", *title, "as", *as); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	doc, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	cr, err := e.repo.Propose(*as, *title, *rationale, doc)
	if err != nil {
		return err
	}
	if cr.Status != config.CRInvalid {
		if suite, serr := latestSuite(); serr == nil {
			if cr, err = changes.AttachEvidence(e.repo, cr.ID, e.store, suite, clock().AddDate(0, 0, -90).Format(time.DateOnly), "", clock()); err != nil {
				return err
			}
		} else if len(config.RequiredEvidence(cr.Tier)) > 0 {
			fmt.Fprintf(out, "Note: %v; run \"erp regress capture\" then \"erp config evidence -cr %s\".\n", serr, cr.ID)
		}
	}
	printCR(cr)
	return nil
}

func printCR(cr *config.ChangeRequest) {
	fmt.Fprintf(out, "%s  %s\n  status: %s   %s   based on v%d   by %s\n", cr.ID, cr.Title, cr.Status, cr.Tier, cr.BaseVersion, cr.Author)
	if cr.Rationale != "" {
		fmt.Fprintf(out, "  rationale: %s\n", cr.Rationale)
	}
	fmt.Fprintln(out, "  changes:")
	for _, c := range cr.Changes {
		fmt.Fprintf(out, "    %s  [%s]\n", c, c.Tier)
	}
	for _, f := range cr.Findings {
		fmt.Fprintln(out, "  finding: "+f.String())
	}
	for _, ev := range cr.Evidence {
		fmt.Fprintf(out, "  %s (passed=%v): %s\n", ev.Kind, ev.Passed, ev.Summary)
	}
	if cr.Workflow != nil && cr.Status == config.CRPendingApproval {
		fmt.Fprintf(out, "  approval: %s\n", cr.Workflow.Waiting())
	}
}

func configEvidence(args []string) error {
	fs := flag.NewFlagSet("config evidence", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	crID := fs.String("cr", "", "change request")
	days := fs.Int("days", 90, "replay invoices from the last N days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("cr", *crID); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	suite, err := latestSuite()
	if err != nil {
		return err
	}
	cr, err := changes.AttachEvidence(e.repo, *crID, e.store, suite, clock().AddDate(0, 0, -*days).Format(time.DateOnly), "", clock())
	if err != nil {
		return err
	}
	printCR(cr)
	return nil
}

func configDecide(approve bool) func([]string) error {
	return func(args []string) error {
		fs := flag.NewFlagSet("config approve", flag.ContinueOnError)
		ef := addEnvFlags(fs)
		crID := fs.String("cr", "", "change request")
		as := fs.String("as", "", "configured approver")
		comment := fs.String("comment", "", "comment (reason when rejecting)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if err := required("cr", *crID, "as", *as); err != nil {
			return err
		}
		e, err := ef.open()
		if err != nil {
			return err
		}
		var cr *config.ChangeRequest
		if approve {
			cr, err = e.repo.Approve(*crID, *as, *comment)
		} else {
			cr, err = e.repo.Reject(*crID, *as, *comment)
		}
		if err != nil {
			return err
		}
		printCR(cr)
		return nil
	}
}

func configActivate(args []string) error {
	fs := flag.NewFlagSet("config activate", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	crID := fs.String("cr", "", "approved change request")
	as := fs.String("as", "", "configured user with config.activate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("cr", *crID, "as", *as); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	n, err := e.repo.Activate(*crID, *as)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s is active as configuration version %d.\n", *crID, n)
	return nil
}

func configRollback(args []string) error {
	fs := flag.NewFlagSet("config rollback", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	to := fs.Int("to", 0, "version to restore")
	as := fs.String("as", "", "configured user with config.activate")
	reason := fs.String("reason", "", "why")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("as", *as, "reason", *reason); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	n, err := e.repo.Rollback(*to, *as, *reason)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Version %d restored as version %d.\n", *to, n)
	return nil
}

func configList(args []string) error {
	fs := flag.NewFlagSet("config list", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTIER\tAUTHOR\tTITLE")
	for _, cr := range e.repo.ChangeRequests() {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", cr.ID, cr.Status, cr.Tier, cr.Author, cr.Title)
	}
	return tw.Flush()
}

func configHistory(args []string) error {
	fs := flag.NewFlagSet("config history", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	_, active, _ := e.repo.Active()
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "VERSION\tACTIVATED\tBY\tSOURCE\t")
	for _, v := range e.repo.Versions() {
		mark := ""
		if v.Number == active {
			mark = "active"
		}
		fmt.Fprintf(tw, "v%d\t%s\t%s\t%s\t%s\n", v.Number, v.ActivatedAt.Format("2006-01-02 15:04"), v.ActivatedBy, v.Source, mark)
	}
	return tw.Flush()
}

func configDiff(args []string) error {
	fs := flag.NewFlagSet("config diff", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	crID := fs.String("cr", "", "change request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("cr", *crID); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	cr, err := e.repo.Get(*crID)
	if err != nil {
		return err
	}
	printCR(cr)
	return nil
}

// --- regression ------------------------------------------------------------

func regressCapture(args []string) error {
	fs := flag.NewFlagSet("regress capture", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	max := fs.Int("max", 50, "maximum cases")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	cfg, v, err := e.repo.Active()
	if err != nil {
		return err
	}
	s, err := regress.Capture(e.store, cfg, v, *max, clock())
	if err != nil {
		return err
	}
	path, err := s.Save(defaultSuiteDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Suite %s: %d cases (%s), saved to %s\n", s.ID, len(s.Cases), strings.Join(s.Strata(), ", "), path)
	return nil
}

func regressRun(args []string) error {
	fs := flag.NewFlagSet("regress run", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	crID := fs.String("cr", "", "change request to test (default: the active configuration)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	cfg, _, err := e.repo.Active()
	if err != nil {
		return err
	}
	if *crID != "" {
		cr, err := e.repo.Get(*crID)
		if err != nil {
			return err
		}
		if cfg, err = config.Parse([]byte(cr.Proposed)); err != nil {
			return err
		}
	}
	suite, err := latestSuite()
	if err != nil {
		return err
	}
	res, err := suite.Run(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, res.Summary())
	return nil
}

// --- procurement -----------------------------------------------------------

type multi []string

func (m *multi) String() string     { return strings.Join(*m, "; ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// parseReqLine reads "description;quantity;unit price;G/L account[;material]".
func parseReqLine(s string) (core.RequisitionLine, error) {
	p := strings.Split(s, ";")
	if len(p) < 4 {
		return core.RequisitionLine{}, fmt.Errorf("line %q: want description;quantity;unit price;account[;material]", s)
	}
	qty, err := money.ParseQty(p[1])
	if err != nil {
		return core.RequisitionLine{}, err
	}
	price, err := money.ParseDecimal(p[2], 2)
	if err != nil {
		return core.RequisitionLine{}, err
	}
	l := core.RequisitionLine{Description: p[0], Quantity: qty, UnitPrice: price, GLAccount: p[3]}
	if len(p) > 4 {
		l.Material = p[4]
	}
	return l, nil
}

func reqCreate(args []string) error {
	fs := flag.NewFlagSet("req create", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "requester")
	company := fs.String("company", "1000", "company code")
	vendor := fs.String("vendor", "", "suggested vendor")
	contract := fs.String("contract", "", "contract to order against")
	var lines multi
	fs.Var(&lines, "line", `repeatable: "description;quantity;unit price;G/L account[;material]"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("as", *as); err != nil {
		return err
	}
	in := p2p.RequisitionInput{CompanyCode: *company, VendorID: *vendor, ContractID: *contract}
	for _, l := range lines {
		rl, err := parseReqLine(l)
		if err != nil {
			return err
		}
		in.Lines = append(in.Lines, rl)
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	req, err := e.p2p().CreateRequisition(*as, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s created: %s %s, %s (%s)\n", req.ID, req.Total(), req.Currency, req.Status, e.store.Workflows[req.WorkflowID].Waiting())
	return e.save()
}

func reqDecide(approve bool) func([]string) error {
	return func(args []string) error {
		fs := flag.NewFlagSet("req approve", flag.ContinueOnError)
		ef := addEnvFlags(fs)
		id := fs.String("id", "", "requisition")
		as := fs.String("as", "", "approver")
		comment := fs.String("comment", "", "comment (reason when rejecting)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if err := required("id", *id, "as", *as); err != nil {
			return err
		}
		e, err := ef.open()
		if err != nil {
			return err
		}
		req, err := e.p2p().DecideRequisition(*as, *id, approve, *comment)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s (%s)\n", req.ID, req.Status, e.store.Workflows[req.WorkflowID].Waiting())
		return e.save()
	}
}

func poCreate(args []string) error {
	fs := flag.NewFlagSet("po create", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	req := fs.String("req", "", "approved requisition")
	vendor := fs.String("vendor", "", "vendor (default: the requisition's)")
	as := fs.String("as", "", "buyer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("req", *req, "as", *as); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	po, doc, err := e.p2p().CreatePurchaseOrder(*as, *req, *vendor)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(defaultOutbox, 0o755); err != nil {
		return err
	}
	path := filepath.Join(defaultOutbox, doc.ID+".txt")
	if err := os.WriteFile(path, []byte(doc.Body), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s created for vendor %s; sent by %s (%s)\n", po.ID, po.VendorID, doc.Channel, path)
	return e.save()
}

// parseContractItem reads "material;description;unit price;G/L account".
func parseContractItem(s string) (core.ContractItem, error) {
	p := strings.Split(s, ";")
	if len(p) != 4 {
		return core.ContractItem{}, fmt.Errorf("item %q: want material;description;unit price;account", s)
	}
	price, err := money.ParseDecimal(p[2], 2)
	if err != nil {
		return core.ContractItem{}, err
	}
	return core.ContractItem{Material: p[0], Description: p[1], UnitPrice: price, PriceUnit: 1, GLAccount: p[3]}, nil
}

func contractCreate(args []string) error {
	fs := flag.NewFlagSet("contract create", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "buyer")
	company := fs.String("company", "1000", "company code")
	vendor := fs.String("vendor", "", "vendor")
	from := fs.String("from", "", "valid from (YYYY-MM-DD)")
	to := fs.String("to", "", "valid to (YYYY-MM-DD)")
	target := fs.String("target", "", "target value")
	var items multi
	fs.Var(&items, "item", `repeatable: "material;description;unit price;G/L account"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("as", *as, "vendor", *vendor, "target", *target); err != nil {
		return err
	}
	tv, err := money.ParseDecimal(*target, 2)
	if err != nil {
		return err
	}
	in := p2p.ContractInput{CompanyCode: *company, VendorID: *vendor, ValidFrom: *from, ValidTo: *to, TargetValue: tv}
	for _, it := range items {
		ci, err := parseContractItem(it)
		if err != nil {
			return err
		}
		in.Items = append(in.Items, ci)
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	c, err := e.p2p().CreateContract(*as, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s created with vendor %s: target %s %s, valid %s to %s\n", c.ID, c.VendorID, c.TargetValue, c.Currency, c.ValidFrom, c.ValidTo)
	return e.save()
}

func grPost(args []string) error {
	fs := flag.NewFlagSet("gr post", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	po := fs.String("po", "", "purchase order")
	line := fs.Int("line", 0, "PO line number")
	qty := fs.String("qty", "", "quantity")
	reverse := fs.Bool("reverse", false, "reverse a receipt (movement 102)")
	as := fs.String("as", "", "warehouse user")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("po", *po, "qty", *qty, "as", *as); err != nil {
		return err
	}
	q, err := money.ParseQty(*qty)
	if err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	gr, err := e.p2p().PostGoodsReceipt(*as, *po, *line, q, *reverse)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s posted: movement %s, %s units on %s line %d\n", gr.ID, gr.MovementType, gr.Quantity, gr.POID, gr.POLineNo)
	return e.save()
}

// --- supplier master ------------------------------------------------------

func vendorCreate(args []string) error {
	fs := flag.NewFlagSet("vendor create", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "requester")
	id := fs.String("id", "", "vendor id")
	name := fs.String("name", "", "name")
	tax := fs.String("tax", "", "tax number")
	bank := fs.String("bank", "", "bank account (IBAN or domestic)")
	company := fs.String("company", "1000", "company code")
	ap := fs.String("ap", "211000", "AP reconciliation account")
	terms := fs.Int("terms", 30, "payment terms, net days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("as", *as, "id", *id, "name", *name); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	ch, err := e.p2p().RequestVendor(*as, core.Vendor{ID: *id, Name: *name, TaxID: *tax, BankAccount: *bank,
		APAccount: map[string]string{*company: *ap}, PaymentTermsDays: *terms})
	if err != nil {
		return err
	}
	printChange(ch)
	return e.save()
}

func printChange(ch *core.MasterDataChange) {
	fmt.Fprintf(out, "%s: %s for vendor %s is %s\n", ch.ID, ch.Kind, ch.VendorID, ch.Status)
	for _, w := range ch.Warnings {
		fmt.Fprintln(out, "  warning: "+w)
	}
}

func vendorBank(args []string) error {
	fs := flag.NewFlagSet("vendor bank", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "requester")
	id := fs.String("id", "", "vendor id")
	bank := fs.String("bank", "", "new bank account")
	reason := fs.String("reason", "", "evidence for the change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("as", *as, "id", *id); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	ch, err := e.p2p().RequestBankChange(*as, *id, *bank, *reason)
	if err != nil {
		return err
	}
	printChange(ch)
	return e.save()
}

func vendorBlock(args []string) error {
	fs := flag.NewFlagSet("vendor block", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "user")
	id := fs.String("id", "", "vendor id")
	reason := fs.String("reason", "", "why")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	if err := e.p2p().BlockVendor(*as, *id, *reason); err != nil {
		return err
	}
	fmt.Fprintf(out, "Vendor %s is blocked.\n", *id)
	return e.save()
}

func vendorUnblock(args []string) error {
	fs := flag.NewFlagSet("vendor unblock", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "requester")
	id := fs.String("id", "", "vendor id")
	reason := fs.String("reason", "", "why")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	ch, err := e.p2p().RequestUnblock(*as, *id, *reason)
	if err != nil {
		return err
	}
	printChange(ch)
	return e.save()
}

func vendorDecide(approve bool) func([]string) error {
	return func(args []string) error {
		fs := flag.NewFlagSet("vendor approve", flag.ContinueOnError)
		ef := addEnvFlags(fs)
		id := fs.String("change", "", "master data change id")
		as := fs.String("as", "", "approver")
		comment := fs.String("comment", "", "comment (reason when rejecting)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if err := required("change", *id, "as", *as); err != nil {
			return err
		}
		e, err := ef.open()
		if err != nil {
			return err
		}
		ch, err := e.p2p().DecideMasterData(*as, *id, approve, *comment)
		if err != nil {
			return err
		}
		printChange(ch)
		return e.save()
	}
}

// --- payments -------------------------------------------------------------

func payPropose(args []string) error {
	fs := flag.NewFlagSet("pay propose", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	as := fs.String("as", "", "treasury user")
	company := fs.String("company", "1000", "company code")
	date := fs.String("date", "", "pay items due by this date (default: today)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("as", *as); err != nil {
		return err
	}
	if *date == "" {
		*date = clock().Format(time.DateOnly)
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	run, err := e.p2p().ProposePaymentRun(*as, *company, *date)
	if run != nil {
		for _, x := range run.Excluded {
			fmt.Fprintf(out, "  excluded %s (%s): %s\n", x.VendorID, x.Amount, x.Reason)
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s proposed: %d vendors, %s %s; awaiting release (%s)\n", run.ID, len(run.Payments), run.Total, e.store.Companies[*company].Currency, e.store.Workflows[run.WorkflowID].Waiting())
	return e.save()
}

func payDecide(approve bool) func([]string) error {
	return func(args []string) error {
		fs := flag.NewFlagSet("pay release", flag.ContinueOnError)
		ef := addEnvFlags(fs)
		id := fs.String("run", "", "payment run")
		as := fs.String("as", "", "releaser")
		comment := fs.String("comment", "", "comment (reason when rejecting)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if err := required("run", *id, "as", *as); err != nil {
			return err
		}
		e, err := ef.open()
		if err != nil {
			return err
		}
		run, err := e.p2p().DecidePaymentRun(*as, *id, approve, *comment)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s\n", run.ID, run.Status)
		if run.BankFile != "" {
			if err := os.MkdirAll(defaultBankDir, 0o755); err != nil {
				return err
			}
			path := filepath.Join(defaultBankDir, run.ID+".xml")
			if err := os.WriteFile(path, []byte(run.BankFile), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(out, "Posted %d payments; ISO 20022 pain.001 file: %s\n", len(run.JournalEntryIDs), path)
		}
		return e.save()
	}
}

// --- capture and agents ---------------------------------------------------

// newExtractor is replaceable in tests.
var newExtractor = func() capture.Extractor { return &capture.ClaudeExtractor{LLM: llm.New()} }

func captureCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	file := fs.String("file", "", "invoice PDF")
	as := fs.String("as", "agent:invoice-capture", "capturing identity (needs invoice.capture)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("file", *file); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	cfg, err := e.config()
	if err != nil {
		return err
	}
	id, err := cfg.Identity(*as)
	if err != nil {
		return err
	}
	if !id.Can(governance.ActInvoiceCapture) {
		return fmt.Errorf("%s lacks %s", *as, governance.ActInvoiceCapture)
	}
	pdf, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	ext, err := newExtractor().Extract(ctx, pdf)
	if err != nil {
		return err
	}
	res, err := capture.Capture(e.store, cfg, pdf, ext, id.ID, clock())
	if err != nil {
		return err
	}
	e.audit.Append(id.ID, governance.ActInvoiceCapture, res.Invoice.ID, string(res.Invoice.Status), map[string]any{"file": filepath.Base(*file), "warnings": res.Warnings})
	fmt.Fprintf(out, "%s captured: vendor %s, reference %s, gross %s %s, %s\n", res.Invoice.ID, res.Invoice.VendorID, res.Invoice.VendorReference, res.Invoice.GrossAmount, res.Invoice.Currency, res.Invoice.Status)
	for _, w := range res.Warnings {
		fmt.Fprintln(out, "  warning: "+w)
	}
	return e.save()
}

// newRunner is replaceable in tests.
var newRunner = llm.New

func agentConfig(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent config", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	extract := fs.String("extract", "", "SAP extract with customizing tables (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		return errors.New(`describe the change, e.g. erp agent config "raise the price tolerance in 1000 to 3%"`)
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	a := &configagent.Agent{LLM: newRunner(), Repo: e.repo, Store: e.store, Suite: latestSuite, Now: clock}
	if *extract != "" {
		x, err := (sap.FileSource{Path: *extract}).Extract(ctx, sap.Scope{CompanyCodes: companyCodes(e.store), FiscalYear: strconv.Itoa(clock().Year())})
		if err != nil {
			return err
		}
		a.Extract = x
	}
	res, err := a.Handle(ctx, request)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, res.Reply)
	for _, id := range res.Changes {
		fmt.Fprintf(out, "Filed %s; review with: erp config diff -cr %s\n", id, id)
	}
	return nil
}

func companyCodes(s *core.Store) []string {
	codes := core.SortedKeys(s.Companies)
	if len(codes) == 0 {
		return []string{"1000"}
	}
	return codes
}

func agentMigration(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent migration", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	source := fs.String("source", "file:"+defaultExtract, `SAP source`)
	company := fs.String("company", "1000", "company code")
	year := fs.String("year", "2026", "fiscal year")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src, err := parseSource(*source)
	if err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	a := &migrationagent.Agent{LLM: newRunner(), Source: src, Store: e.store}
	summary, err := a.Assess(ctx, *company, *year)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, summary)
	return nil
}

func agentTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent test", flag.ContinueOnError)
	ef := addEnvFlags(fs)
	crID := fs.String("cr", "", "change request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required("cr", *crID); err != nil {
		return err
	}
	e, err := ef.open()
	if err != nil {
		return err
	}
	a := &testagent.Agent{LLM: newRunner(), Repo: e.repo, Store: e.store, SuiteDir: defaultSuiteDir, UATDir: defaultUATDir, Now: clock}
	rev, err := a.Review(ctx, *crID)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, rev.Recommendation)
	for _, p := range rev.UATScripts {
		fmt.Fprintln(out, "UAT script: "+p)
	}
	return nil
}
