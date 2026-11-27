package controllingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// maxReason is the longest reason a proposal gives: it is the parked
// entry's header text, which holds 200 characters (JournalEntryCreate).
const maxReason = 200

// maxReference is the longest external reference an entry holds
// (JournalEntryCreate reference).
const maxReference = 40

// The refusals of the proposer, which the model is told and may correct.
var (
	// ErrInput refuses a proposal not in the proposer's form.
	ErrInput = errors.New("controllingagent: a proposal names a journal entry by its id, a line by its number, exactly one of a cost center or a profit center by its id, " +
		"and a reason of 1 to 200 characters")
	// ErrSource refuses a line the agent does not repost: one that is not a
	// line of a journal entry of the run's company and period in the
	// leading ledger, or of a reversed or reversing entry, or without a
	// center, on an internal order, or of no amount.
	ErrSource = errors.New("controllingagent: the line reposted is a line of a journal entry of this run's company and fiscal period in the leading ledger, " +
		"neither reversed nor a reversal, with a cost center or a profit center, no internal order and an amount")
	// ErrTarget refuses a center the line cannot move to: a line with a
	// cost center moves to another cost center; a line with a profit center
	// alone to another profit center, or to a cost center.
	ErrTarget = errors.New("controllingagent: a line with a cost center is reposted to another cost center (cost_center_id); a line with a profit center alone to another " +
		"profit center (profit_center_id) or to a cost center")
	// ErrTwice refuses a line proposed before: a parked entry of the
	// company carries its reference, whatever became of it.
	ErrTwice = errors.New("controllingagent: the line has had a reposting proposed (a parked entry carries its reference); it is not proposed again")
	// ErrLedgers refuses a line whose posting is in some of the company's
	// ledgers but not all, nor the leading ledger alone: which ledgers a
	// reposting of it belongs in is a person's to say.
	ErrLedgers = errors.New("controllingagent: the line's posting is in some of the company's ledgers only; a person reposts it")
)

// proposal is the model's input to the proposer.
type proposal struct {
	entry, cost, profit, reason string
	line                        int
}

// proposalOf reads the proposer's input.
func proposalOf(input map[string]any) (proposal, error) {
	text := func(key string) string {
		s, _ := input[key].(string) // not text: empty, which the checks below refuse where it is required
		return strings.TrimSpace(s)
	}
	p := proposal{entry: text("journal_entry_id"), cost: text("cost_center_id"), profit: text("profit_center_id"), reason: text("reason")}
	n, err := strconv.Atoi(fmt.Sprint(input["line_no"]))
	if err != nil || n < 1 || p.entry == "" || (p.cost == "") == (p.profit == "") || p.reason == "" || utf8.RuneCountInString(p.reason) > maxReason {
		return proposal{}, ErrInput
	}
	p.line = n
	return p, nil
}

// entry is a journal entry as the agent's code reads it.
type entry struct {
	ID             string `json:"id"`
	LegalEntityID  string `json:"legal_entity_id"`
	LedgerID       string `json:"ledger_id"`
	Leading        bool   `json:"leading_ledger"`
	FiscalYear     int    `json:"fiscal_year"`
	FiscalPeriod   int    `json:"fiscal_period"`
	DocumentType   string `json:"document_type"`
	DocumentNumber string `json:"document_number"`
	PostingDate    string `json:"posting_date"`
	LocalCurrency  string `json:"local_currency"`
	PostingGroupID string `json:"posting_group_id"`
	ReversalOfID   string `json:"reversal_of_id"`
	ReversedByID   string `json:"reversed_by_id"`
	Lines          []line `json:"lines"`
}

// line is a journal line as the agent's code reads it.
type line struct {
	LineNo   int `json:"line_no"`
	AmountLC struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"amount_lc"`
	AccountID       string `json:"account_id"`
	CostCenterID    string `json:"cost_center_id"`
	ProfitCenterID  string `json:"profit_center_id"`
	InternalOrderID string `json:"internal_order_id"`
}

// reposting checks a proposal against the line the agent's code reads
// itself, parks the reposting it builds from that line and keeps it in
// the run's result.
func (r *run) reposting(ctx context.Context, input map[string]any) (any, error) {
	p, err := proposalOf(input)
	if err != nil {
		return nil, err
	}
	var e entry
	if err := r.get(ctx, "/v1/journal-entries/"+url.PathEscape(p.entry), nil, &e); err != nil {
		return nil, fmt.Errorf("%w: journal entry %s: %w", ErrSource, p.entry, err)
	}
	l, err := r.source(e, p.line)
	if err != nil {
		return nil, err
	}
	if err := target(l, p); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("RP %d %s %s/%d", e.FiscalYear, e.DocumentType, e.DocumentNumber, l.LineNo)
	if len(ref) > maxReference {
		return nil, fmt.Errorf("%w: its document number is too long to name it in a reference", ErrSource)
	}
	if err := r.unproposed(ctx, ref); err != nil {
		return nil, err
	}
	ledgerID, err := r.postedTo(ctx, e)
	if err != nil {
		return nil, err
	}
	body, err := repost(e, l, p, ref, ledgerID)
	if err != nil {
		return nil, err
	}
	return r.parked(ctx, body, Proposal{JournalEntryID: e.ID, LineNo: l.LineNo, Reference: ref})
}

// source is the entry's line numbered n, if the agent reposts it: a line
// of the run's company and period in the leading ledger, of an entry
// neither reversed nor reversing (a reversal undoes, and a reversed
// line's cost is gone), with a center to move from, no internal order
// (its cost is the order's, settled to its receiver: ADR-0064) and an
// amount in the entry's local currency.
func (r *run) source(e entry, n int) (line, error) {
	if e.LegalEntityID != r.company || e.FiscalYear != r.period.Year || e.FiscalPeriod != r.period.Period || !e.Leading ||
		e.ReversalOfID != "" || e.ReversedByID != "" {
		return line{}, ErrSource
	}
	i := slices.IndexFunc(e.Lines, func(l line) bool { return l.LineNo == n })
	if i < 0 {
		return line{}, fmt.Errorf("%w: the entry has no line %d", ErrSource, n)
	}
	l := e.Lines[i]
	if (l.CostCenterID == "" && l.ProfitCenterID == "") || l.InternalOrderID != "" || l.AmountLC.Currency != e.LocalCurrency {
		return line{}, ErrSource
	}
	return l, nil
}

// target refuses a center the line cannot move to.
func target(l line, p proposal) error {
	switch {
	case l.CostCenterID != "" && (p.cost == "" || p.cost == l.CostCenterID):
		return ErrTarget
	case l.CostCenterID == "" && p.profit == l.ProfitCenterID:
		return ErrTarget
	}
	return nil
}

// unproposed refuses a line this run, or any parked entry of the company,
// has proposed before: one with its reference, in any status (posted, it
// moved the line; rejected or cancelled, a person decided against it).
func (r *run) unproposed(ctx context.Context, ref string) error {
	if r.proposed[ref] {
		return fmt.Errorf("%w: %s, in this run", ErrTwice, ref)
	}
	var page struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := r.get(ctx, "/v1/parked-journal-entries", url.Values{"legal_entity_id": {r.company}, "reference": {ref}, "page_size": {"1"}}, &page); err != nil {
		return fmt.Errorf("controllingagent: the parked entries of %s: %w", ref, err)
	}
	if len(page.Items) > 0 {
		return fmt.Errorf("%w: %s, parked entry %s, %s", ErrTwice, ref, page.Items[0].ID, page.Items[0].Status)
	}
	return nil
}

// copyOf is an entry of another ledger that may be a copy of the line's
// entry: a copy shares its posting group.
type copyOf struct {
	PostingGroupID string `json:"posting_group_id"`
}

// postedTo is the ledger a reposting of the entry's line posts to: none
// named (every ledger the company keeps) when the entry's posting is in
// each of them, the entry's own when it is in that ledger alone (a
// ledger-specific posting, ADR-0045); a posting in some of them but not
// all is ErrLedgers. A copy is the entry of another ledger with its
// document number and its posting group.
func (r *run) postedTo(ctx context.Context, e entry) (string, error) {
	copies, others := 0, 0
	for _, l := range r.ledgers {
		if l.ID == e.LedgerID {
			continue
		}
		others++
		var page struct {
			Items []copyOf `json:"items"`
		}
		q := url.Values{"legal_entity_id": {e.LegalEntityID}, "ledger_id": {l.ID}, "fiscal_year": {strconv.Itoa(e.FiscalYear)},
			"document_number": {e.DocumentNumber}, "page_size": {"100"}}
		if err := r.get(ctx, "/v1/journal-entries", q, &page); err != nil {
			return "", fmt.Errorf("controllingagent: the copies of entry %s in ledger %s: %w", e.DocumentNumber, l.ID, err)
		}
		if slices.ContainsFunc(page.Items, func(c copyOf) bool { return c.PostingGroupID == e.PostingGroupID }) {
			copies++
		}
	}
	switch copies {
	case others:
		return "", nil
	case 0:
		return e.LedgerID, nil
	}
	return "", ErrLedgers
}

// repost is the reposting of the line as parkJournalEntry takes it: on
// the line's account, its whole local amount off its center and onto the
// one proposed, dated as the line was posted (into its special period,
// if it was: ADR-0060 §3), in the company's currency, with the reference
// naming the line and the reason as its header text. The lines carry no
// tax code, partner or quantity: a reposting moves cost between centers
// and nothing else (ADR-0050 §5).
func repost(e entry, l line, p proposal, ref, ledgerID string) (map[string]any, error) {
	amount := l.AmountLC.Amount
	_, frac, _ := strings.Cut(amount, ".")
	minor, err := money.ParseAmount(amount, len(frac))
	if err != nil || minor == 0 {
		return nil, ErrSource
	}
	text := fmt.Sprintf("Reposting of %s line %d", e.DocumentNumber, l.LineNo)
	from := map[string]any{"account_id": l.AccountID, "amount": map[string]any{"amount": (-minor).Plain(len(frac)), "currency": e.LocalCurrency}, "line_text": text}
	to := map[string]any{"account_id": l.AccountID, "amount": map[string]any{"amount": minor.Plain(len(frac)), "currency": e.LocalCurrency}, "line_text": text}
	if l.CostCenterID != "" {
		from["cost_center_id"] = l.CostCenterID
	} else {
		from["profit_center_id"] = l.ProfitCenterID
	}
	if p.cost != "" {
		to["cost_center_id"] = p.cost
	} else {
		to["profit_center_id"] = p.profit
	}
	body := map[string]any{"legal_entity_id": e.LegalEntityID, "document_date": e.PostingDate, "posting_date": e.PostingDate, "currency": e.LocalCurrency,
		"reference": ref, "header_text": p.reason, "lines": []any{from, to}}
	if ledgerID != "" {
		body["ledger_id"] = ledgerID
	}
	if e.FiscalPeriod > 12 {
		body["fiscal_period"] = e.FiscalPeriod
	}
	return body, nil
}

// parked parks the reposting and keeps it in the result: an entry the API
// took is the run's even when the session failed to record the call
// (apitools.ErrNotRecorded), so the line is not proposed again. The
// model is answered with the parked entry, or with the API's problem.
func (r *run) parked(ctx context.Context, body map[string]any, p Proposal) (any, error) {
	resp, err := r.park.Invoke(ctx, map[string]any{"body": body})
	var entry struct {
		ID string `json:"id"`
	}
	if resp.OK() && resp.Decode(&entry) == nil && entry.ID != "" {
		p.ParkedEntryID = entry.ID
		r.proposed[p.Reference] = true
		r.res.Proposals = append(r.res.Proposals, p)
	}
	switch {
	case err != nil:
		return nil, fmt.Errorf("controllingagent: park the reposting %s: %w", p.Reference, err)
	case p.ParkedEntryID == "":
		return nil, fmt.Errorf("controllingagent: the ledger refused the reposting %s: %w", p.Reference, refusal(resp))
	}
	return json.RawMessage(resp.Body), nil
}

// refusal is the problem of an answer that parked nothing: its error, or,
// for a 2xx answer without an entry, one saying so.
func refusal(resp apitools.Response) error {
	if err := resp.Err(); err != nil {
		return err
	}
	return errors.New("the answer names no parked entry")
}
