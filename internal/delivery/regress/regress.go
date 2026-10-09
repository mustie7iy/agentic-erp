// Package regress builds regression suites from real transactions and
// replays them against proposed configurations. A suite stores a snapshot
// of the data and the outcome every case had when it was captured, so a
// later run shows exactly which business results a change would alter.
package regress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/delivery/simulate"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

type Case struct {
	InvoiceID string           `json:"invoiceId"`
	Why       string           `json:"why"` // the stratum that selected it
	Expected  simulate.Outcome `json:"expected"`
}

type Suite struct {
	ID            string          `json:"id"`
	CreatedAt     time.Time       `json:"createdAt"`
	ConfigVersion int             `json:"configVersion"`
	PostingDate   string          `json:"postingDate"`
	Snapshot      json.RawMessage `json:"snapshot"`
	Cases         []Case          `json:"cases"`
}

// Capture selects cases so every behavior is covered, not just the common
// one: each decision, each kind of match issue, amounts close to the
// auto-post limit, then the remaining invoices up to max.
func Capture(live *core.Store, cfg *config.Config, version, max int, now time.Time) (*Suite, error) {
	box := simulate.Clone(live)
	snapshot, _ := json.Marshal(box) // a cloned store always encodes (Clone just did it)
	posting := now.Format(time.DateOnly)
	ids := simulate.Candidates(box, "", "")
	if len(ids) == 0 {
		return nil, fmt.Errorf("no invoices to build a suite from")
	}
	outcomes, err := simulate.Evaluate(box, cfg, ids, posting)
	if err != nil {
		return nil, err
	}
	s := &Suite{ID: "RS-" + now.UTC().Format("20060102T150405"), CreatedAt: now.UTC(), ConfigVersion: version, PostingDate: posting, Snapshot: snapshot}
	picked := map[string]int{}
	add := func(id, why string) {
		if i, ok := picked[id]; ok {
			// A case can cover several strata; record every reason.
			if why != "sample" && !strings.Contains(s.Cases[i].Why, why) {
				s.Cases[i].Why += "; " + why
			}
			return
		}
		if max <= 0 || len(s.Cases) < max {
			picked[id] = len(s.Cases)
			s.Cases = append(s.Cases, Case{InvoiceID: id, Why: why, Expected: outcomes[id]})
		}
	}
	for _, decision := range []string{simulate.AutoPost, simulate.Review, simulate.Exception} {
		for _, id := range ids {
			if outcomes[id].Decision == decision {
				add(id, "decision "+decision)
				break
			}
		}
	}
	seenIssue := map[core.IssueKind]bool{}
	for _, id := range ids {
		for _, k := range outcomes[id].Issues {
			if !seenIssue[k] {
				seenIssue[k] = true
				add(id, "issue "+string(k))
			}
		}
	}
	for _, id := range ids {
		inv := box.Invoices[id]
		set, _ := cfg.Settings(inv.CompanyCode) // Evaluate above already resolved every company
		if near(inv.GrossAmount, set.AutoPostLimit) {
			add(id, "near the auto-post limit")
		}
	}
	for _, id := range ids {
		add(id, "sample")
	}
	return s, nil
}

// near is within 20% of the limit, either side.
func near(amount, limit money.Minor) bool {
	return limit > 0 && amount >= limit*8/10 && amount <= limit*12/10
}

// Failure is a case whose outcome differs from when it was captured.
type Failure struct {
	InvoiceID string           `json:"invoiceId"`
	Why       string           `json:"why"`
	Expected  simulate.Outcome `json:"expected"`
	Got       simulate.Outcome `json:"got"`
}

type Result struct {
	SuiteID string    `json:"suiteId"`
	Cases   int       `json:"cases"`
	Failed  []Failure `json:"failed"`
}

func (r *Result) Passed() bool { return len(r.Failed) == 0 }

// Run replays the suite under cfg on its own snapshot.
func (s *Suite) Run(cfg *config.Config) (*Result, error) {
	box := core.NewStore()
	if err := json.Unmarshal(s.Snapshot, box); err != nil {
		return nil, fmt.Errorf("suite snapshot: %w", err)
	}
	ids := simulate.Candidates(box, "", "")
	got, err := simulate.Evaluate(box, cfg, ids, s.PostingDate)
	if err != nil {
		return nil, err
	}
	r := &Result{SuiteID: s.ID, Cases: len(s.Cases)}
	for _, c := range s.Cases {
		g, ok := got[c.InvoiceID]
		if !ok {
			return nil, fmt.Errorf("case %s is not in the snapshot", c.InvoiceID)
		}
		if !c.Expected.Equal(g) {
			r.Failed = append(r.Failed, Failure{InvoiceID: c.InvoiceID, Why: c.Why, Expected: c.Expected, Got: g})
		}
	}
	return r, nil
}

func (r *Result) Summary() string {
	if r.Passed() {
		return fmt.Sprintf("Regression suite %s: all %d cases unchanged.", r.SuiteID, r.Cases)
	}
	parts := make([]string, 0, len(r.Failed))
	for _, f := range r.Failed {
		parts = append(parts, fmt.Sprintf("%s (%s) %s -> %s", f.InvoiceID, f.Why, f.Expected, f.Got))
	}
	return fmt.Sprintf("Regression suite %s: %d of %d cases change: %s.", r.SuiteID, len(r.Failed), r.Cases, strings.Join(parts, "; "))
}

func (r *Result) Evidence() config.Evidence {
	detail, _ := json.Marshal(r)
	return config.Evidence{Kind: "regression", Passed: r.Passed(), Summary: r.Summary(), Detail: detail}
}

// Strata lists why cases were selected, for reports.
func (s *Suite) Strata() []string {
	var out []string
	for _, c := range s.Cases {
		for _, why := range strings.Split(c.Why, "; ") {
			if !slices.Contains(out, why) {
				out = append(out, why)
			}
		}
	}
	return out
}

func (s *Suite) Save(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, _ := json.Marshal(s) // a suite is plain data
	path := filepath.Join(dir, s.ID+".json")
	return path, os.WriteFile(path, b, 0o644)
}

func Load(path string) (*Suite, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Suite
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("suite %s: %w", path, err)
	}
	return &s, nil
}

// Latest returns the newest suite in dir.
func Latest(dir string) (*Suite, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "RS-*.json"))
	if len(matches) == 0 {
		return nil, fmt.Errorf("no regression suite in %s (capture one first)", dir)
	}
	slices.Sort(matches)
	return Load(matches[len(matches)-1])
}
