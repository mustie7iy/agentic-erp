package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/delivery/configagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/migrationagent"
	"github.com/mustie7iy/agentic-erp/internal/delivery/testagent"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

type parkedInvoice struct {
	ID              string
	DocumentNumber  string                  `json:"document_number"`
	LegalEntityID   string                  `json:"legal_entity_id"`
	PurchaseOrderID string                  `json:"purchase_order_id"`
	Gross           struct{ Amount string } `json:"gross_amount"`
}

// minor reads a decimal amount in the minor units its decimals imply.
func minor(s string) int64 {
	decimals := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		decimals = len(s) - i - 1
	}
	m, _ := money.ParseDecimal(s, decimals) // amounts come from the API or the configuration
	return int64(m)
}

// limits are each legal entity's auto-post limit from the active
// configuration, in minor units.
func limits(ctx context.Context, c *apitools.Client) (map[string]int64, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/v1/config-versions", url.Values{"is_active": {"true"}}, nil, "")
	if err != nil {
		return nil, err
	}
	var versions struct {
		Items []struct {
			Document struct {
				Companies []struct {
					Code          string
					AutoPostLimit string `json:"auto_post_limit"`
				}
			}
		}
	}
	if err := resp.Decode(&versions); err != nil {
		return nil, err
	}
	if len(versions.Items) == 0 {
		return nil, errors.New("the tenant has no active configuration")
	}
	resp, err = c.Do(ctx, http.MethodGet, "/v1/legal-entities", url.Values{"page_size": {"500"}}, nil, "")
	if err != nil {
		return nil, err
	}
	var les struct{ Items []struct{ ID, Code string } }
	if err := resp.Decode(&les); err != nil {
		return nil, err
	}
	byCode := map[string]int64{}
	for _, co := range versions.Items[0].Document.Companies {
		byCode[co.Code] = minor(co.AutoPostLimit)
	}
	out := map[string]int64{}
	for _, le := range les.Items {
		out[le.ID] = byCode[le.Code]
	}
	return out, nil
}

// invoices is the matching rule as an API client: every parked invoice on
// a purchase order is matched; a clean match within the legal entity's
// auto-post limit is posted, one above it is held for a person, and the
// match holds the others itself, opening their exception requests. With
// -agent, the exceptions agent then investigates the held invoices.
func invoices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invoices", flag.ContinueOnError)
	withAgent := fs.Bool("agent", false, "let the exceptions agent investigate held invoices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := client("ERP_TOKEN")
	if err != nil {
		return err
	}
	limit, err := limits(ctx, c)
	if err != nil {
		return err
	}
	resp, err := c.Do(ctx, http.MethodGet, "/v1/supplier-invoices", url.Values{"status": {"parked"}, "page_size": {"500"}}, nil, "")
	if err != nil {
		return err
	}
	var page struct{ Items []parkedInvoice }
	if err := resp.Decode(&page); err != nil {
		return err
	}
	var posted, held []string
	for _, inv := range page.Items {
		if inv.PurchaseOrderID == "" {
			continue // entered by hand without an order: a person posts it
		}
		path := "/v1/supplier-invoices/" + inv.ID
		got, err := c.Do(ctx, http.MethodGet, path, nil, nil, "")
		if err != nil {
			return err
		}
		resp, err := c.Do(ctx, http.MethodPost, path+":match", nil, nil, got.ETag)
		if err != nil {
			return err
		}
		var match struct {
			IsClean bool `json:"is_clean"`
		}
		if err := resp.Decode(&match); err != nil {
			return fmt.Errorf("invoice %s: %w", inv.DocumentNumber, err)
		}
		action, body := "post", any(nil)
		switch {
		case !match.IsClean:
			held = append(held, inv.DocumentNumber+" (match)")
			continue
		case minor(inv.Gross.Amount) > limit[inv.LegalEntityID]:
			action, body = "hold", map[string]string{"reason": "Above the auto-post limit; a person posts it."}
		}
		got, err = c.Do(ctx, http.MethodGet, path, nil, nil, "")
		if err != nil {
			return err
		}
		resp, err = c.Do(ctx, http.MethodPost, path+":"+action, nil, body, got.ETag)
		if err != nil {
			return err
		}
		if err := resp.Err(); err != nil {
			return fmt.Errorf("invoice %s: %w", inv.DocumentNumber, err)
		}
		if action == "hold" {
			held = append(held, inv.DocumentNumber+" (limit)")
		} else {
			posted = append(posted, inv.DocumentNumber)
		}
	}
	fmt.Fprintf(out, "Posted %d: %s\nHeld %d: %s\n", len(posted), strings.Join(posted, ", "), len(held), strings.Join(held, ", "))
	if *withAgent {
		return agentInvoices(ctx)
	}
	return nil
}

func agentInvoices(ctx context.Context) error {
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, err := (&agent.Agent{LLM: newRunner(), API: c}).Review(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Agent reviewed %d held invoices and made %d recommendations.\n", len(res.Reviewed), res.Recommendations)
	for _, e := range res.Errors {
		fmt.Fprintln(out, "  "+e)
	}
	return nil
}

// agentCmd runs a delivery agent with the agents' token.
func agentCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("agent invoices | config | migration | test")
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
	}
	return fmt.Errorf("unknown agent %q", args[0])
}

func agentConfig(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent config", flag.ContinueOnError)
	draftFile := fs.String("draft", "", "configuration draft (erp config generate) to start from")
	if err := fs.Parse(args); err != nil {
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
			return err
		}
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	res, err := (&configagent.Agent{LLM: newRunner(), API: c}).Handle(ctx, request, draft)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\n\nChange requests: %s\n", res.Reply, strings.Join(res.ChangeRequests, ", "))
	return nil
}

func agentMigration(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent migration", flag.ContinueOnError)
	connector := fs.String("connector", "", "source connector id")
	entities := fs.String("legal-entities", "", "comma-separated legal entity ids")
	year := fs.Int("year", 2026, "fiscal year")
	mode := fs.String("mode", "documents", "documents or balances")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	text, err := (&migrationagent.Agent{LLM: newRunner(), API: c}).Assess(ctx, *connector, strings.Split(*entities, ","), *year, *mode)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, text)
	return nil
}

func agentTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent test", flag.ContinueOnError)
	cr := fs.String("change-request", "", "change request id")
	outFile := fs.String("out", "", "also write the test scripts to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := client("ERP_AGENT_TOKEN")
	if err != nil {
		return err
	}
	rev, err := (&testagent.Agent{LLM: newRunner(), API: c}).Review(ctx, *cr)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Recommendation recorded: %t\n\n%s\n", rev.Recommended, rev.Scripts)
	if *outFile != "" {
		return os.WriteFile(*outFile, []byte(rev.Scripts), 0o600)
	}
	return nil
}
