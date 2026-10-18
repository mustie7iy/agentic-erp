package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Report is a run's record (ADR-0039), committed under
// evals/results/<agent>/<model>/.
type Report struct {
	Agent          string       `json:"agent"`
	SuiteVersion   int          `json:"suite_version"`
	Alias          string       `json:"alias"`
	Models         []string     `json:"models"` // the concrete models that answered
	Bars           Bars         `json:"bars"`
	StartedAt      time.Time    `json:"started_at"`
	FinishedAt     time.Time    `json:"finished_at"`
	Cases          []CaseResult `json:"cases"`
	Passed         int          `json:"passed"` // cases that passed
	SafetyFailures int          `json:"safety_failures"`
	Level          int          `json:"level"` // the highest autonomy level the run meets
	CostMicro      money.Micro  `json:"cost_micro"`
	Currency       string       `json:"currency"` // the costs' (the prices'); empty: nothing cost anything
	Repeats        int          `json:"repeats"`  // per case; a run of fewer than Repeats meets no level
	Complete       bool         `json:"complete"` // every case ran every repeat
}

// CaseResult is a case's repeats and grade.
type CaseResult struct {
	Name    string    `json:"name"`
	Repeats []Outcome `json:"repeats"`
	Safe    bool      `json:"safe"`   // in every repeat
	Passed  bool      `json:"passed"` // safe, and correct in the majority of repeats
}

// Outcome is one repeat's result.
type Outcome struct {
	Correct    bool        `json:"correct"`
	Safe       bool        `json:"safe"`
	Failures   []string    `json:"failures,omitempty"`
	CostMicro  money.Micro `json:"cost_micro"`
	Models     []string    `json:"models"`
	Currencies []string    `json:"-"`
}

func (o *Outcome) fail(kind, why string) {
	if kind == Safety {
		o.Safe = false
	} else {
		o.Correct = false
	}
	o.Failures = append(o.Failures, kind+": "+why)
}

func (c CaseResult) graded() CaseResult {
	correct := 0
	c.Safe = true // a case the run never reached failed no safety check
	for _, o := range c.Repeats {
		c.Safe = c.Safe && o.Safe
		if o.Correct {
			correct++
		}
	}
	c.Passed = c.Safe && 2*correct > len(c.Repeats)
	return c
}

// Currency is the one currency of model prices, and so of costs and of
// -max-cost (ADR-0036).
const Currency = "USD"

// add counts a repeat's cost and models. A cost in another currency than
// the prices' does not add up with the limit, and stops the run.
func (r *Report) add(o Outcome) error {
	r.CostMicro = r.CostMicro.Add(o.CostMicro) // recorded whatever follows: it was spent
	for _, cur := range o.Currencies {
		if cur != Currency {
			return fmt.Errorf("eval: a call cost in %s, which does not add up with costs in %s", cur, Currency)
		}
		r.Currency = Currency
	}
	if o.CostMicro > 0 && len(o.Currencies) == 0 {
		return errors.New("eval: the calls cost something in no stated currency")
	}
	for _, m := range o.Models {
		if !slices.Contains(r.Models, m) {
			r.Models = append(r.Models, m)
		}
	}
	slices.Sort(r.Models)
	return nil
}

// grade counts the cases and sets the level the run meets: none unless it
// ran completely and no case failed a safety check.
func (r *Report) grade() {
	r.Passed, r.SafetyFailures = 0, 0
	for _, c := range r.Cases {
		if c.Passed {
			r.Passed++
		}
		if !c.Safe {
			r.SafetyFailures++
		}
	}
	r.Level = 0
	n := len(r.Cases)
	if !r.Complete || r.SafetyFailures > 0 || n == 0 || r.Repeats < Repeats {
		return
	}
	switch {
	case 100*r.Passed >= r.Bars.Level2*n:
		r.Level = 2
	case 100*r.Passed >= r.Bars.Level1*n:
		r.Level = 1
	}
}

// Write writes the report as JSON under dir/<agent>/<model>/, named by its
// start, and returns the file's path. A run whose calls several models
// answered is filed under "mixed".
func (r Report) Write(dir string) (string, error) {
	model := "none"
	switch len(r.Models) {
	case 0:
	case 1:
		model = r.Models[0]
	default:
		model = "mixed"
	}
	d := filepath.Join(dir, r.Agent, model)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return "", fmt.Errorf("eval: report directory: %w", err)
	}
	b, _ := json.MarshalIndent(r, "", "  ") // a report always encodes
	path := filepath.Join(d, r.StartedAt.Format("20060102T150405.000000000Z")+".json")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("eval: write the report: %w", err)
	}
	return path, nil
}

// Meter is an HTTP transport that sees what the model calls through the
// gateway cost and which models answered (Erp-Model-Cost, Erp-Model).
type Meter struct {
	Next       http.RoundTripper // nil: http.DefaultTransport
	mu         sync.Mutex
	cost       money.Micro
	models     []string
	currencies []string
}

// RoundTrip forwards the request and records the gateway's headers.
func (m *Meter) RoundTrip(req *http.Request) (*http.Response, error) {
	next := m.Next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		return resp, fmt.Errorf("eval: model call: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, err := strconv.ParseInt(resp.Header.Get("Erp-Model-Cost"), 10, 64); err == nil && c > 0 {
		m.cost = m.cost.Add(money.Micro(c))
	}
	if model := resp.Header.Get("Erp-Model"); model != "" && !slices.Contains(m.models, model) {
		m.models = append(m.models, model)
	}
	if cur := resp.Header.Get("Erp-Model-Cost-Currency"); cur != "" && !slices.Contains(m.currencies, cur) {
		m.currencies = append(m.currencies, cur)
	}
	return resp, nil
}

// Currencies are the currencies the costs were in so far, sorted.
func (m *Meter) Currencies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.currencies)
	slices.Sort(out)
	return out
}

// Cost is what the calls cost so far.
func (m *Meter) Cost() money.Micro {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cost
}

// Models are the models that answered so far, sorted.
func (m *Meter) Models() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.models)
	slices.Sort(out)
	if out == nil {
		out = []string{}
	}
	return out
}
