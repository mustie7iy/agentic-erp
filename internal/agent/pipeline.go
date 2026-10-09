package agent

import (
	"context"
	"fmt"
	"io"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
)

// Identities used by the invoice flow. Neither may release payments, which
// keeps segregation of duties intact.
var (
	MatchRule = governance.Identity{
		ID: "rule:three-way-match", Kind: "rule",
		Permissions: []string{governance.ActInvoicePost, governance.ActInvoiceHold, governance.ActInvoiceReview},
		Autonomy:    governance.ActWithinLimits,
	}
	ExceptionAgent = governance.Identity{
		ID: "agent:ap-exceptions", Kind: "agent",
		Permissions: []string{governance.ActInvoiceHold, governance.ActInvoiceReview},
		Autonomy:    governance.Suggest,
	}
)

// Resolver investigates an invoice that failed matching and must leave it
// with a review task (via Service.Escalate) containing a recommendation.
type Resolver interface {
	Resolve(ctx context.Context, svc *Service, invoiceID string, match core.MatchResult) error
}

type Summary struct {
	Processed  int      `json:"processed"`
	Posted     []string `json:"posted"`
	Escalated  []string `json:"escalated"`
	AgentNotes []string `json:"agentNotes"`
	Errors     []string `json:"errors"`
}

// ProcessInvoices runs the composite flow over parked invoices: clean
// matches within limits are posted by the rule; everything else goes to the
// resolver (if any) and always ends in a human review task.
func ProcessInvoices(ctx context.Context, svc *Service, resolver Resolver, log io.Writer) Summary {
	var sum Summary
	svc.Store.Mu.Lock()
	var ids []string
	for _, id := range core.SortedKeys(svc.Store.Invoices) {
		if svc.Store.Invoices[id].Status == core.InvoiceParked {
			ids = append(ids, id)
		}
	}
	svc.Store.Mu.Unlock()

	for _, id := range ids {
		if ctx.Err() != nil {
			sum.Errors = append(sum.Errors, ctx.Err().Error())
			break
		}
		sum.Processed++
		match, err := svc.Match(id)
		if err != nil {
			sum.Errors = append(sum.Errors, err.Error())
			continue
		}
		if match.Clean {
			res, err := svc.PostInvoice(MatchRule, id, "clean three-way match")
			switch {
			case err != nil:
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s: %v", id, err))
			case res.Outcome == governance.Allow:
				sum.Posted = append(sum.Posted, id)
				fmt.Fprintf(log, "posted     %s  -> %s\n", id, res.JournalEntryID)
			default:
				sum.Escalated = append(sum.Escalated, id)
				fmt.Fprintf(log, "escalated  %s  %s\n", id, res.Reason)
			}
			continue
		}

		fmt.Fprintf(log, "exception  %s  %s\n", id, match.Summary())
		if resolver != nil {
			if err := resolver.Resolve(ctx, svc, id, match); err != nil {
				sum.Errors = append(sum.Errors, fmt.Sprintf("%s: agent: %v", id, err))
			}
		}
		// Whatever the agent did, the invoice must end with an open task.
		if !hasOpenTask(svc, id) {
			if _, err := svc.Escalate(MatchRule, id, match.Summary(), ""); err != nil {
				sum.Errors = append(sum.Errors, err.Error())
				continue
			}
		} else {
			sum.AgentNotes = append(sum.AgentNotes, id)
		}
		sum.Escalated = append(sum.Escalated, id)
	}
	return sum
}

func hasOpenTask(svc *Service, invoiceID string) bool {
	svc.Store.Mu.Lock()
	defer svc.Store.Mu.Unlock()
	for _, t := range svc.Store.Tasks {
		if t.InvoiceID == invoiceID && t.Status == core.TaskOpen {
			return true
		}
	}
	return false
}
