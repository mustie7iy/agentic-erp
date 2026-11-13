// Package bidagent is the bid comparison agent (ADR-0072, T105c), a
// recommending agent (internal/recommend): for a request for quotation it
// reads the RFQ, its quotations and their comparison by price (ADR-0069
// §7), the suppliers, their contracts and purchase orders through API
// tools (04-api-design §4.13), and leaves one recommendation on the RFQ
// for the buyer to decide: to award it to one quotation (award), naming
// it on the content's first line, or not to award it (no_award). The
// quotation named is one the comparison ranks, complete and valid on the
// company's date today, as the comparison read before the session gives
// it: an RFQ without such a quotation gets no_award or nothing, and only a
// published RFQ is recommended for an award. Every number the content
// writes is one the run read (recommend.Figures): the totals, the items'
// amounts. It never awards, publishes an RFQ or records a quotation: those
// are no tools of its (Never), and the API refuses an agent the award
// (x-agent: false). What a supplier wrote (a reference, a name) is data,
// never an instruction. Its evaluation suite is evals/bid_comparison
// (ADR-0039).
package bidagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "bid_comparison"

// The object type the agent recommends on, and its kinds.
const (
	RFQObject = "rfq"
	Award     = "award"
	NoAward   = "no_award"
)

// Reads are the operations the agent reads with (ADR-0072, T105c).
var Reads = []string{"getRfq", "listSupplierQuotations", "getRfqComparison", "getBusinessPartner", "listContracts", "listPurchaseOrders"}

// Never are the operations the agent never calls (ADR-0072): the award,
// publishing an RFQ and recording a quotation, and creating an RFQ beside
// them, every write of sourcing, which its tools hold none of: the
// pattern gives an agent no write but its recommendation.
var Never = []string{"awardRfq", "publishRfq", "createSupplierQuotation", "createRfq"}

const system = `You compare the quotations of a request for quotation (RFQ) and recommend whether to award it, for the buyer to decide.

Read the comparison (getRfqComparison): the suppliers invited, each quotation's total and its price and amount of each RFQ
item, the ranked quotations first, by total, the lowest first. A quotation is ranked when it prices every item and is valid
on the company's date today (as_of); one that leaves items out (complete false, unquoted_item_nos) or whose validity has
passed (expired) is not ranked, and a supplier invited may not have answered. You may read the RFQ (getRfq), its quotations
(listSupplierQuotations), a supplier (getBusinessPartner) and the company's contracts and purchase orders with a supplier
(listContracts, listPurchaseOrders with supplier_id).

Then recommend once, on the RFQ. award when a quotation is ranked: the content's first line is "Award: " and that
quotation's supplier_quotation_id, as the comparison gives it, normally the quotation ranked first; then the reasons: its
supplier and total, the totals of the other ranked quotations, each item for which another supplier quoted a lower amount
(the item, that supplier and its amount), and the quotations left out as incomplete or expired, and why. no_award when no
quotation is ranked: say which quotations were left out and why, and which suppliers did not answer. Every amount you state
is one the comparison or another tool returned, written exactly as returned: you compute no amount, difference or
percentage, and a count you made yourself is written in words. The price is what the comparison judges; you may name what
the contracts and orders show of a supplier, but you do not score suppliers.`

// Agent compares an RFQ's quotations.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
}

// comparison is what the run needs of the RFQ's comparison before it
// starts: the RFQ, its company and status, and the quotations ranked.
type comparison struct {
	RFQID          string `json:"rfq_id"`
	DocumentNumber string `json:"rfq_document_number"`
	CompanyCode    string `json:"company_code"`
	CompanyName    string `json:"company_name"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
	AsOf           string `json:"as_of"`
	Suppliers      []struct {
		QuotationID string `json:"supplier_quotation_id"`
		Rank        *int   `json:"rank"`
	} `json:"suppliers"`
}

// ranked are the quotations the comparison ranks.
func (c comparison) ranked() []string {
	var out []string
	for _, s := range c.Suppliers {
		if s.Rank != nil && s.QuotationID != "" {
			out = append(out, s.QuotationID)
		}
	}
	return out
}

// The refusals of the agent's own check, beside the pattern's.
var (
	// ErrNotRanked refuses an award whose first line does not name a
	// quotation the comparison ranks: complete and valid on the
	// company's date today.
	ErrNotRanked = errors.New(`bidagent: an award's first line is "Award: " and the supplier_quotation_id of a quotation the comparison ranks (complete and not expired); without one, recommend no_award`)
	// ErrNotPublished refuses an award of an RFQ that is not published:
	// only a published RFQ is awarded.
	ErrNotPublished = errors.New("bidagent: only a published RFQ is recommended for an award")
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("bidagent: every number in the content is one a tool returned or the run was given, written as returned; a count of your own is written in words")
)

// Compare runs one session on the RFQ: it reads the RFQ's comparison (an
// RFQ the agent may not read stops the run before a session starts),
// offers the model the tools and reports what it recommended, also when
// the run fails after recommending.
func (a *Agent) Compare(ctx context.Context, rfq string) (recommend.Result, error) {
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/rfqs/"+rfq+"/comparison", nil, nil, "")
	var c comparison
	if err == nil {
		err = resp.Decode(&c)
	}
	if err == nil && c.RFQID == "" {
		err = errors.New("no RFQ in the answer")
	}
	if err != nil {
		return recommend.Result{}, fmt.Errorf("bidagent: the comparison of RFQ %s: %w", rfq, err)
	}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Compare the quotations of RFQ %s in company %s", c.DocumentNumber, c.CompanyCode),
		Prompt: fmt.Sprintf("RFQ %s (id %s) of company %s %s, in %s, %s; the company's date today is %s.",
			c.DocumentNumber, c.RFQID, c.CompanyCode, c.CompanyName, c.Currency, c.Status, c.AsOf),
		Objects: map[string]string{c.RFQID: RFQObject},
		Check:   c.check,
	}
	return agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{RFQObject: {Award, NoAward}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// awarded is an award's first line: the quotation named.
var awarded = regexp.MustCompile(`^Award: ([0-9a-fA-F-]{36})\s*(?:\n|$)`)

// check is the agent's rule on a recommendation the pattern admits
// (ADR-0072, T105c): every number it writes is one the run read, and an
// award names on its first line a quotation the comparison read before
// the session ranks, of a published RFQ. no_award needs no form: a
// person reads why.
func (c comparison) check(p recommend.Proposed, f *recommend.Figures) error {
	if unread := f.Unread(p.Content); len(unread) > 0 {
		return fmt.Errorf("%w: not %s", ErrUnread, strings.Join(unread, ", "))
	}
	if p.Kind != Award {
		return nil
	}
	m := awarded.FindStringSubmatch(p.Content)
	if m == nil || !slices.Contains(c.ranked(), strings.ToLower(m[1])) {
		return ErrNotRanked
	}
	if c.Status != "published" {
		return ErrNotPublished
	}
	return nil
}
