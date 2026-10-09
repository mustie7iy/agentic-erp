// Package simulate replays business transactions in a sandbox copy of the
// store under two configurations and reports what changes: which invoices
// get a different decision, and how ledger balances move. Nothing touches
// the live store.
package simulate

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/agent"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Clone deep-copies a store; the copy is the sandbox.
func Clone(s *core.Store) *core.Store {
	s.Mu.Lock()
	b, err := json.Marshal(s)
	s.Mu.Unlock()
	must(err) // the store holds only plain data
	c := core.NewStore()
	must(json.Unmarshal(b, c)) // decoding what was just encoded
	return c
}

// must panics on errors that only a programming mistake could cause.
func must(err error) {
	if err != nil {
		panic("simulate: " + err.Error())
	}
}

// Decision values.
const (
	AutoPost  = "auto_post" // clean match within the limit: posted by the rule
	Review    = "review"    // clean but needs a person (above the limit)
	Exception = "exception" // the match found issues
)

// Outcome is what the invoice flow decides for one invoice.
type Outcome struct {
	InvoiceID string             `json:"invoiceId"`
	Decision  string             `json:"decision"`
	Issues    []core.IssueKind   `json:"issues,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Lines     []core.JournalLine `json:"lines,omitempty"`
}

// Equal compares decisions, issues and posted lines.
func (o Outcome) Equal(p Outcome) bool {
	if o.Decision != p.Decision || !slices.Equal(o.Issues, p.Issues) || len(o.Lines) != len(p.Lines) {
		return false
	}
	for i := range o.Lines {
		a, b := o.Lines[i], p.Lines[i]
		if a.Account != b.Account || a.Amount != b.Amount || a.VendorID != b.VendorID {
			return false
		}
	}
	return true
}

func (o Outcome) String() string {
	if len(o.Issues) > 0 {
		kinds := make([]string, len(o.Issues))
		for i, k := range o.Issues {
			kinds[i] = string(k)
		}
		return o.Decision + " (" + strings.Join(kinds, ", ") + ")"
	}
	return o.Decision
}

// Candidates are invoices the platform decides on: anything not already
// posted in SAP (SAP postings are history, replayed by migration instead).
func Candidates(s *core.Store, from, to string) []string {
	var out []string
	for _, id := range core.SortedKeys(s.Invoices) {
		inv := s.Invoices[id]
		if inv.Status == core.InvoiceRejected || (inv.Status == core.InvoicePosted && strings.HasPrefix(inv.Source, "sap:")) {
			continue
		}
		if (from == "" || inv.InvoiceDate >= from) && (to == "" || inv.InvoiceDate <= to) {
			out = append(out, id)
		}
	}
	return out
}

// Evaluate resets the given invoices in the sandbox to parked (removing any
// native posting) and runs the deterministic invoice decision for each, in
// order, posting the auto-posted ones. The sandbox is modified.
func Evaluate(sandbox *core.Store, cfg *config.Config, ids []string, postingDate string) (map[string]Outcome, error) {
	for _, id := range ids {
		inv := sandbox.Invoices[id]
		if inv == nil {
			return nil, fmt.Errorf("invoice %s not found", id)
		}
		if inv.JournalEntryID != "" {
			delete(sandbox.Journal, inv.JournalEntryID)
			inv.JournalEntryID = ""
		}
		inv.Status, inv.HoldReason = core.InvoiceParked, ""
	}
	out := map[string]Outcome{}
	for _, id := range ids {
		inv := sandbox.Invoices[id]
		set, err := cfg.Settings(inv.CompanyCode)
		if err != nil {
			return nil, err
		}
		pol, _ := cfg.Policy(inv.CompanyCode) // cannot fail once Settings succeeded
		m := sandbox.ThreeWayMatch(inv, set.Tolerance)
		o := Outcome{InvoiceID: id}
		if !m.Clean {
			o.Decision, o.Reason = Exception, m.Summary()
			for _, is := range m.Issues {
				if !slices.Contains(o.Issues, is.Kind) {
					o.Issues = append(o.Issues, is.Kind)
				}
			}
			out[id] = o
			continue
		}
		d := pol.Decide(agent.MatchRule, governance.ActInvoicePost, inv.GrossAmount)
		if d.Outcome != governance.Allow {
			o.Decision, o.Reason = Review, d.Reason
			out[id] = o
			continue
		}
		je, err := sandbox.PostInvoice(id, set.InputTax, postingDate)
		if err != nil {
			o.Decision, o.Reason = Exception, err.Error()
			out[id] = o
			continue
		}
		o.Decision, o.Lines = AutoPost, je.Lines
		out[id] = o
	}
	return out, nil
}

// Diff is an invoice whose outcome changes.
type Diff struct {
	InvoiceID string  `json:"invoiceId"`
	Before    Outcome `json:"before"`
	After     Outcome `json:"after"`
}

// Report is the result of replaying a window under two configurations.
type Report struct {
	From         string                            `json:"from,omitempty"`
	To           string                            `json:"to,omitempty"`
	Cases        int                               `json:"cases"`
	Changed      []Diff                            `json:"changed"`
	LedgerImpact map[string]map[string]money.Minor `json:"ledgerImpact"` // company -> account -> after minus before
	Before       map[string]int                    `json:"before"`       // decision counts
	After        map[string]int                    `json:"after"`
}

// Replay runs the window's invoices under before and after in two sandboxes.
func Replay(live *core.Store, before, after *config.Config, from, to, postingDate string) (*Report, error) {
	live.Mu.Lock()
	ids := Candidates(live, from, to)
	live.Mu.Unlock()
	run := func(cfg *config.Config) (map[string]Outcome, *core.Store, error) {
		box := Clone(live)
		out, err := Evaluate(box, cfg, ids, postingDate)
		return out, box, err
	}
	a, boxA, err := run(before)
	if err != nil {
		return nil, fmt.Errorf("replay under current configuration: %w", err)
	}
	b, boxB, err := run(after)
	if err != nil {
		return nil, fmt.Errorf("replay under proposed configuration: %w", err)
	}
	r := &Report{From: from, To: to, Cases: len(ids), LedgerImpact: map[string]map[string]money.Minor{}, Before: map[string]int{}, After: map[string]int{}}
	for _, id := range ids {
		r.Before[a[id].Decision]++
		r.After[b[id].Decision]++
		if !a[id].Equal(b[id]) {
			r.Changed = append(r.Changed, Diff{InvoiceID: id, Before: a[id], After: b[id]})
		}
	}
	for code := range boxA.Companies {
		ta, tb := boxA.TrialBalance(code, ""), boxB.TrialBalance(code, "")
		for _, acct := range core.SortedKeys(union(ta, tb)) {
			if d := tb[acct] - ta[acct]; d != 0 {
				if r.LedgerImpact[code] == nil {
					r.LedgerImpact[code] = map[string]money.Minor{}
				}
				r.LedgerImpact[code][acct] = d
			}
		}
	}
	return r, nil
}

func union(a, b map[string]money.Minor) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// Summary is a one-paragraph description for approvers.
func (r *Report) Summary() string {
	if len(r.Changed) == 0 {
		return fmt.Sprintf("Replayed %d invoices: no decision or posting changes.", r.Cases)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Replayed %d invoices: %d change outcome", r.Cases, len(r.Changed))
	for i, d := range r.Changed {
		if i == 5 {
			fmt.Fprintf(&b, "; and %d more", len(r.Changed)-5)
			break
		}
		fmt.Fprintf(&b, "; %s %s -> %s", d.InvoiceID, d.Before, d.After)
	}
	b.WriteString(".")
	for _, code := range core.SortedKeys(r.LedgerImpact) {
		for _, acct := range core.SortedKeys(r.LedgerImpact[code]) {
			fmt.Fprintf(&b, " Company %s account %s moves by %s.", code, acct, r.LedgerImpact[code][acct])
		}
	}
	return b.String()
}

// Evidence packages the report for a change request.
func (r *Report) Evidence() config.Evidence {
	detail, _ := json.Marshal(r)
	return config.Evidence{Kind: "simulation", Passed: len(r.Changed) == 0, Summary: r.Summary(), Detail: detail}
}
