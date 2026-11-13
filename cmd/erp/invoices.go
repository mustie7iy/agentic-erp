package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/cashagent"
	"github.com/mustie7iy/agentic-erp/internal/closeagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/configagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/migrationagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/testagent"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/reconagent"
	"github.com/mustie7iy/agentic-erp/internal/taxagent"
)

type parkedInvoice struct {
	ID              string
	DocumentNumber  string `json:"document_number"`
	PurchaseOrderID string `json:"purchase_order_id"`
	// WorkflowRequestID is set on an invoice parked on an approved
	// exception (or, after an edit by the previous release, on one it no
	// longer covers).
	WorkflowRequestID string `json:"workflow_request_id"`
}

// decision is the server's decision on a matched invoice (F12b): it
// converts the gross amount into the company's currency and compares it
// with the auto-post limit, the rule its simulations and regression suites
// apply. Outcome is an open enumeration.
type decision struct {
	Outcome string
	Reason  string
}

// matched is the answer of a match: a server before F12b answers no
// decision.
type matched struct {
	IsClean  bool      `json:"is_clean"`
	Decision *decision `json:"decision"`
}

// settled is what the rule did with the invoices: posted, held for a
// person, or left parked for want of a decision it knows. refused are the
// parked invoices whose match the server refused, with the reason.
type settled struct{ posted, held, parked, refused []string }

// invoices is the matching rule as an API client: every parked invoice on
// a purchase order is matched, and the server's decision is acted on. A
// clean match it decides auto_post is posted, one it reviews or cannot
// evaluate is held for a person, and the match holds the others itself,
// opening their exception requests. Every page of parked invoices is
// settled.
//
// The rule never compares amounts itself. The invoice is left parked when:
//   - there is no decision it knows (an older server, a new outcome);
//   - the server refuses the match (422, e.g. no exchange rate);
//   - the invoice is gone (404);
//   - it changed between its read and its match, or between its match and
//     the post or hold (412: the server acts only on the matched version).
//
// An invoice parked with an exception request linked is neither matched
// nor posted but listed as left parked for a person to decide (F18).
// Normally it is an approved exception, but after an edit by the previous
// release the approval may no longer cover it, which :post then refuses
// (409 payables.match_required). Matching it again only finds the issues
// the approval covers, and posting it is a person's decision, as the rule
// cannot tell whether the server would decide review (a match with issues
// answers no decision, and :post does not check the auto-post limit).
//
// With -agent, the exceptions agent then investigates the held invoices.
//
// A run in which the server refused a match fails (exit 1) once every
// invoice is settled and the agent has run: the refusal (no exchange rate,
// no order) leaves the invoice parked until a person fixes its cause, which
// a scheduler running the rule must see. Invoices only left parked (gone,
// changed, an exception linked, no decision known) do not fail it; the
// next run or a person settles them.
func invoices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invoices", flag.ContinueOnError)
	withAgent := fs.Bool("agent", false, "let the exceptions agent investigate held invoices")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	parked, err := apitools.List[parkedInvoice](ctx, c, "/v1/supplier-invoices", url.Values{"status": {"parked"}})
	if err != nil {
		return err
	}
	w := printer()
	var done settled
	for _, inv := range parked {
		if inv.PurchaseOrderID == "" {
			continue // entered by hand without an order: a person posts it
		}
		if inv.WorkflowRequestID != "" {
			done.parked = append(done.parked, inv.DocumentNumber+" (exception request linked: a person decides)")
			continue
		}
		if err := settle(ctx, c, w, inv, &done); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "Posted %d: %s\nHeld %d: %s\n", len(done.posted), strings.Join(done.posted, ", "), len(done.held), strings.Join(done.held, ", "))
	if len(done.parked) > 0 {
		fmt.Fprintf(w, "Left parked %d: %s\n", len(done.parked), strings.Join(done.parked, ", "))
	}
	if err := errs.Wrapf(w.Flush(), "invoices: print"); err != nil {
		return err
	}
	var agentErr error
	if *withAgent {
		agentErr = agentInvoices(ctx)
	}
	return errors.Join(agentErr, done.refusal())
}

// refusal is the run's error when the server refused any match.
func (done *settled) refusal() error {
	n := len(done.refused)
	if n == 0 {
		return nil
	}
	noun := "match"
	if n > 1 {
		noun = "matches"
	}
	return fmt.Errorf("invoices: %d %s refused: %s", n, noun, strings.Join(done.refused, ", "))
}

// settle matches one invoice, shows the server's decision and acts on it,
// on the version it matched: the server posts only the version its latest
// match evaluated (F13), and an invoice changed since is refused (412) and
// left parked.
func settle(ctx context.Context, c *apitools.Client, w io.Writer, inv parkedInvoice, done *settled) error {
	path := "/v1/supplier-invoices/" + inv.ID
	m, tag, err := matchInvoice(ctx, c, path, inv)
	if refused := (*apitools.Error)(nil); errors.As(err, &refused) && parkedOn[refused.Status] != "" {
		why := parkedOn[refused.Status]
		fmt.Fprintf(w, "%s: %s: %s: %s\n", inv.DocumentNumber, why, refused.Code, refused.Detail)
		done.parked = append(done.parked, inv.DocumentNumber+" ("+why+": "+refused.Code+")")
		if refused.Status == http.StatusUnprocessableEntity {
			done.refused = append(done.refused, inv.DocumentNumber+" ("+refused.Code+")")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !m.IsClean {
		done.held = append(done.held, inv.DocumentNumber+" (match)")
		return nil
	}
	if m.Decision == nil {
		fmt.Fprintf(w, "%s: decision unavailable (the server does not return it)\n", inv.DocumentNumber)
		done.parked = append(done.parked, inv.DocumentNumber)
		return nil
	}
	d := *m.Decision
	fmt.Fprintf(w, "%s: %s (%s)\n", inv.DocumentNumber, d.Outcome, d.Reason)
	act := func(list *[]string, listed, action string, body any) error {
		return done.listIfDone(w, list, inv.DocumentNumber, listed, settleInvoice(ctx, c, path, inv, action, body, tag))
	}
	switch d.Outcome {
	case "auto_post":
		return act(&done.posted, inv.DocumentNumber, "post", nil)
	case "review":
		return act(&done.held, inv.DocumentNumber+" (review)", "hold", map[string]string{"reason": "Above the auto-post limit; a person posts it."})
	case "not_evaluable":
		return act(&done.held, inv.DocumentNumber+" (not_evaluable: "+d.Reason+")", "hold",
			map[string]string{"reason": "The auto-post rule cannot evaluate it (" + d.Reason + "); a person posts it."})
	}
	done.parked = append(done.parked, inv.DocumentNumber)
	return nil
}

// parkedOn are the refusals of an invoice's read or match that leave it
// parked for a person or the next run, while the other invoices go on, by
// how they are listed.
var parkedOn = map[int]string{
	// The server refuses to match it: no exchange rate, no order.
	http.StatusUnprocessableEntity: "refused",
	// It is gone from the caller's view since the listing.
	http.StatusNotFound: "not found",
	// It changed between its read and its match; the next run reads it again.
	http.StatusPreconditionFailed: "changed before its match",
}

// listIfDone lists an invoice (as listed) once the action on it succeeded
// (err nil). An invoice changed since its match (412) is left parked for
// the next run, which matches it again; other errors stop the rule.
func (done *settled) listIfDone(w io.Writer, list *[]string, invoice, listed string, err error) error {
	var refused *apitools.Error
	if errors.As(err, &refused) && refused.Status == http.StatusPreconditionFailed {
		fmt.Fprintf(w, "%s: changed since its match; left parked\n", invoice)
		done.parked = append(done.parked, invoice+" (changed since its match)")
		return nil
	}
	if err == nil {
		*list = append(*list, listed)
	}
	return err
}

// matchInvoice matches the invoice at path against its order, on its
// current version, and returns the match with the server's decision and
// the ETag the match was sent with: the version the decision is for. A
// failed read is returned, never matched with the empty ETag it carries.
func matchInvoice(ctx context.Context, c *apitools.Client, path string, inv parkedInvoice) (matched, string, error) {
	cur, err := c.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return matched{}, "", err
	}
	if err := cur.Err(); err != nil {
		return matched{}, "", fmt.Errorf("invoice %s: %w", inv.DocumentNumber, err)
	}
	resp, err := c.Do(ctx, http.MethodPost, path+":match", nil, nil, cur.ETag)
	if err != nil {
		return matched{}, "", err
	}
	var m matched
	if err := resp.Decode(&m); err != nil {
		return matched{}, "", fmt.Errorf("invoice %s: %w", inv.DocumentNumber, err)
	}
	return m, cur.ETag, nil
}

// settleInvoice posts or holds (action) the matched invoice at path with
// tag, the ETag its match was sent with: a clean match of a parked invoice
// leaves its version unchanged, so the tag holds unless the invoice was
// changed since, which the server refuses (412) rather than act on what was
// not matched.
func settleInvoice(ctx context.Context, c *apitools.Client, path string, inv parkedInvoice, action string, body any, tag string) error {
	resp, err := c.Do(ctx, http.MethodPost, path+":"+action, nil, body, tag)
	if err != nil {
		return err
	}
	if err := resp.Err(); err != nil {
		return fmt.Errorf("invoice %s: %w", inv.DocumentNumber, err)
	}
	return nil
}

func agentInvoices(ctx context.Context) error {
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, err := (&agent.Agent{LLM: newRunner(c), API: c}).Review(ctx)
	if err != nil {
		return err
	}
	w := printer()
	fmt.Fprintf(w, "Agent reviewed %d held invoices and made %d recommendations.\n", len(res.Reviewed), res.Recommendations)
	for _, e := range res.Errors {
		fmt.Fprintln(w, "  "+e)
	}
	return errs.Wrapf(w.Flush(), "agent invoices: print")
}

// agentCmd runs a delivery agent with the agents' token.
func agentCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("agent invoices | config | migration | test | reconcile | close-accruals | close-variances | cash-variance | tax-draft | credit-score | access-roles | access-sod | access-review")
	}
	switch args[0] {
	case "invoices":
		return agentInvoices(ctx)
	case "config":
		return agentConfig(ctx, args[1:])
	case "migration":
		return agentMigration(ctx, args[1:])
	case "test":
		return agentTest(ctx, args[1:])
	case "reconcile":
		return agentReconcile(ctx, args[1:])
	case "close-accruals":
		return agentCloseAccruals(ctx, args[1:])
	case "close-variances":
		return agentCloseVariances(ctx, args[1:])
	case "cash-variance":
		return agentCashVariance(ctx, args[1:])
	case "tax-draft":
		return agentTaxDraft(ctx, args[1:])
	case "credit-score":
		return agentCreditScore(ctx, args[1:])
	case "access-roles":
		return agentAccessRoles(ctx, args[1:])
	case "access-sod":
		return agentAccessSod(ctx, args[1:])
	case "access-review":
		return agentAccessReview(ctx, args[1:])
	}
	return fmt.Errorf("unknown agent %q", args[0])
}

func agentConfig(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent config", flag.ContinueOnError)
	draftFile := fs.String("draft", "", "configuration draft (erp config generate) to start from")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		return errors.New(`say what to change, e.g. erp agent config "raise the price tolerance of 2000 to 3%"`)
	}
	var draft []byte
	if *draftFile != "" {
		var err error
		if draft, err = os.ReadFile(*draftFile); err != nil {
			return fmt.Errorf("agent config: read draft: %w", err)
		}
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, err := (&configagent.Agent{LLM: newRunner(c), API: c}).Handle(ctx, request, draft)
	if err != nil {
		return err
	}
	return printf("agent config", "%s\n\nChange requests: %s\n", res.Reply, strings.Join(res.ChangeRequests, ", "))
}

func agentMigration(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent migration", flag.ContinueOnError)
	connector := fs.String("connector", "", "source connector id")
	entities := fs.String("legal-entities", "", "comma-separated legal entity ids")
	year := fs.Int("year", 2026, "fiscal year")
	mode := fs.String("mode", "documents", "documents or balances")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	text, err := (&migrationagent.Agent{LLM: newRunner(c), API: c}).Assess(ctx, *connector, strings.Split(*entities, ","), *year, *mode)
	if err != nil {
		return err
	}
	return printf("agent migration", "%s\n", text)
}

func agentTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent test", flag.ContinueOnError)
	cr := fs.String("change-request", "", "change request id")
	outFile := fs.String("out", "", "also write the test scripts to this file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	rev, err := (&testagent.Agent{LLM: newRunner(c), API: c}).Review(ctx, *cr)
	if err != nil {
		return err
	}
	if err := printf("agent test", "Recommendation recorded: %t\n\n%s\n", rev.Recommended, rev.Scripts); err != nil {
		return err
	}
	if *outFile != "" {
		if err := os.WriteFile(*outFile, []byte(rev.Scripts), 0o600); err != nil {
			return fmt.Errorf("agent test: write scripts: %w", err)
		}
	}
	return nil
}

// agentReconcile has the reconciliation agent propose for the statement's
// lines the rules left open (T49g): a person accepts or rejects each.
func agentReconcile(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent reconcile", flag.ContinueOnError)
	statement := fs.String("statement", "", "bank statement id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, err := (&reconagent.Agent{LLM: newRunner(c), API: c}).Review(ctx, *statement)
	if err != nil {
		return err
	}
	if err := printf("agent reconcile", "Agent reviewed %d lines and made %d proposals; %d left to people.\n", len(res.Reviewed), res.Proposals, len(res.Declined)); err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("agent reconcile: %s", strings.Join(res.Errors, "; "))
	}
	return nil
}

// agentCloseAccruals has the close agent propose the close's accruals
// (ADR-0058 §12, T64g), on erp-close unless ERP_AGENT_MODEL names an
// alias: a person posts or cancels each proposal's parked entry.
func agentCloseAccruals(ctx context.Context, args []string) error {
	return agentClose(ctx, "close-accruals", args, func(a *closeagent.Agent, closeID, _ string) (closeagent.Result, error) {
		return a.Accruals(ctx, closeID)
	}, func(p closeagent.Proposal) string {
		return fmt.Sprintf("proposal %s, parked entry %s", p.ID, p.ParkedEntryID)
	})
}

// agentCloseVariances has the close agent propose comments on the close's
// variance rows (ADR-0058 §12, T64j), against -basis or every basis the
// company configures, on erp-close unless ERP_AGENT_MODEL names an alias:
// a person accepts or rejects each comment.
func agentCloseVariances(ctx context.Context, args []string) error {
	return agentClose(ctx, "close-variances", args, func(a *closeagent.Agent, closeID, basis string) (closeagent.Result, error) {
		return a.Variances(ctx, closeID, basis)
	}, func(p closeagent.Proposal) string {
		return fmt.Sprintf("comment %s on %s against %s", p.ID, p.AccountNumber, p.Basis)
	})
}

// agentClose runs one of the close agent's runs on -close (and, for the
// variances, -basis) and prints its session, what it proposed, each as
// line says, and the model's reply.
func agentClose(ctx context.Context, name string, args []string, run func(*closeagent.Agent, string, string) (closeagent.Result, error),
	line func(closeagent.Proposal) string) error {
	fs := flag.NewFlagSet("agent "+name, flag.ContinueOnError)
	closeID := fs.String("close", "", "period close id")
	basis := new(string)
	if name == "close-variances" {
		basis = fs.String("basis", "", "variance basis (default: every basis the company configures)")
	}
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *closeID == "" {
		return fmt.Errorf("agent %s: -close names the period close", name)
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := run(&closeagent.Agent{LLM: closeRunner(c), API: c}, *closeID, *basis)
	if res.Session == "" {
		return runErr
	}
	// What the run made is printed even when it failed afterwards: its
	// proposals wait for a person either way.
	w := printer()
	fmt.Fprintf(w, "Session %s: proposals made %d, refused %d.\n", res.Session, len(res.Proposals), res.Refused)
	for _, p := range res.Proposals {
		fmt.Fprintf(w, "  %s\n", line(p))
	}
	if res.Reply != "" {
		fmt.Fprintln(w, res.Reply)
	}
	return errors.Join(runErr, errs.Wrapf(w.Flush(), "agent %s: print", name))
}

// closeRunner is newRunner on the close agent's alias, unless
// ERP_AGENT_MODEL names another.
func closeRunner(c *apitools.Client) *llm.Runner {
	r := newRunner(c)
	if getenv("ERP_AGENT_MODEL") == "" {
		r.Model = closeagent.Alias
	}
	return r
}

// agentCashVariance has the forecast agent explain a company's forecast
// variance over a period (ADR-0051 §13, T53i): it proposes comments citing
// the documents behind the attribution and memo records for flows the
// statements show recurring, and prints its session, each proposal and
// its reply; a person accepts or rejects each.
func agentCashVariance(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent cash-variance", flag.ContinueOnError)
	company := fs.String("company", "", "company code")
	period := fs.String("period", "", "the period, FROM..TO (YYYY-MM-DD..YYYY-MM-DD)")
	lag := fs.Int("lag", cashagent.DefaultLag, "the snapshot's lag in days: 0, 7 or 30")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	from, to, ok := strings.Cut(*period, "..")
	if *company == "" || !ok || from == "" || to == "" {
		return errors.New("agent cash-variance: want -company CODE -period FROM..TO [-lag 7]")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&cashagent.Agent{LLM: newRunner(c), API: c}).Explain(ctx, cashagent.Period{Company: *company, From: from, To: to, Lag: *lag})
	if res.Session != "" {
		out := fmt.Sprintf("Session %s: comments proposed %d, memo records proposed %d, refused %d.\n", res.Session, len(res.Comments), len(res.Memos), res.Refused)
		for _, id := range res.Comments {
			out += "  comment " + id + "\n"
		}
		for _, id := range res.Memos {
			out += "  memo record " + id + "\n"
		}
		if res.Reply != "" {
			out += res.Reply + "\n"
		}
		if err := printf("agent cash-variance", "%s", out); err != nil {
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

// agentTaxDraft has the tax agent review a registration's draft return
// of a period (ADR-0049 §13, T61l1, T61l2): a person reads its
// explanation and prepares the return, or not, decides its code
// corrections, and posts or cancels the adjustment entries it parked.
func agentTaxDraft(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent tax-draft", flag.ContinueOnError)
	reg := fs.String("registration", "", "tax registration id or code")
	period := fs.String("period", "", "the period's first day (YYYY-MM-DD)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *reg == "" || *period == "" {
		return errors.New("agent tax-draft: -registration and -period name the draft return")
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, runErr := (&taxagent.Agent{LLM: newRunner(c), API: c}).Explain(ctx, *reg, *period)
	if res.Session == "" {
		return runErr
	}
	// What the run proposed is printed even when it failed afterwards:
	// its recommendations and parked entries wait for a person either way.
	w := printer()
	fmt.Fprintf(w, "Session %s: recommendations made %d, entries parked %d, refused %d.\n", res.Session, len(res.Recommendations), len(res.Parked), res.Refused)
	for _, id := range res.Recommendations {
		fmt.Fprintf(w, "  recommendation %s\n", id)
	}
	for _, id := range res.Parked {
		fmt.Fprintf(w, "  parked entry %s\n", id)
	}
	if res.Reply != "" {
		fmt.Fprintln(w, res.Reply)
	}
	return errors.Join(runErr, errs.Wrapf(w.Flush(), "agent tax-draft: print"))
}
