package analytics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Session holds the results of one conversation with the analytics agent,
// so dashboards can refer to them by query_id.
type Session struct {
	Model *Model
	// Facts are what a session without a Runner computes its answers
	// from.
	Facts     *Facts
	Principal Principal
	Results   map[string]*Result
	// Now is the clock reports are built and rendered with; nil reads the
	// wall clock (clock.Now).
	Now func() time.Time
	// Runner answers the session's queries elsewhere: erp-server's
	// analytics endpoint (Remote), which reads the analytical store or the
	// database (ADR-0044); nil computes them from Facts.
	Runner Runner
}

// Runner answers semantic queries.
type Runner interface {
	Query(ctx context.Context, q Query) (*Result, error)
}

func NewSession(m *Model, f *Facts, p Principal) *Session {
	return &Session{Model: m, Facts: f, Principal: p, Results: map[string]*Result{}}
}

// Run executes a semantic query for the session's principal, through its
// Runner or over its Facts; either way the data is what the principal's
// permissions and company access allow.
func (s *Session) Run(ctx context.Context, q Query) (*Result, error) {
	var r *Result
	var err error
	if s.Runner != nil {
		r, err = s.Runner.Query(ctx, q)
	} else {
		r, err = Execute(s.Model, s.Facts, s.Principal, q)
	}
	if err != nil {
		return nil, err
	}
	s.Results[r.ID] = r
	return r, nil
}

// DataThrough is the latest date in the data of the session's results.
func (s *Session) DataThrough() string {
	latest := ""
	for _, r := range s.Results {
		latest = max(latest, r.DataThrough)
	}
	return latest
}

// Report is a saved answer: the question, the dashboard spec and the queries
// behind it. Rerunning it recomputes every figure from current data.
type Report struct {
	ID        string           `json:"id"`
	Question  string           `json:"question"`
	Principal Principal        `json:"principal"`
	Spec      Spec             `json:"spec"`
	Queries   map[string]Query `json:"queries"`
	CreatedAt time.Time        `json:"created_at"`
	CreatedBy string           `json:"created_by"`
	// Exchange is the analysis's questions in the order asked, each with
	// the agent's answer and the queries it used (T94a); absent on a
	// report saved before it was kept.
	Exchange []Turn `json:"exchange,omitempty"`
	// Now is the clock the page is rendered with; nil reads the wall
	// clock (clock.Now). It is not saved with the report.
	Now func() time.Time `json:"-"`
}

// Build validates a spec against the session's results and turns it into a
// report. Problems are returned so the agent can fix the spec.
func (s *Session) Build(spec Spec, createdBy string) (*Report, []string) {
	used := s.usedResults(spec)
	if problems := spec.Validate(used); len(problems) > 0 {
		return nil, problems
	}
	rep := &Report{
		Question: spec.Question, Principal: s.Principal, Spec: spec, Queries: map[string]Query{},
		CreatedAt: clock.Now(s.Now).UTC(), CreatedBy: createdBy, Now: s.Now,
	}
	for id, r := range used {
		rep.Queries[id] = r.Query
	}
	b, _ := json.Marshal(rep) // plain data, as at save below: it always encodes
	sum := sha256.Sum256(b)
	rep.ID = "rpt_" + hex.EncodeToString(sum[:5])
	return rep, nil
}

// usedResults returns the session results the spec refers to, plus all
// results when commentary is present (commentary may cite any of them).
func (s *Session) usedResults(spec Spec) map[string]*Result {
	used := map[string]*Result{}
	add := func(id string) {
		if r := s.Results[id]; r != nil {
			used[id] = r
		}
	}
	for _, t := range spec.Tiles {
		add(t.QueryID)
	}
	for _, c := range spec.Charts {
		add(c.QueryID)
	}
	for _, t := range spec.Tables {
		add(t.QueryID)
	}
	if spec.Commentary != "" {
		for id := range s.Results {
			add(id)
		}
	}
	// Keep unknown ids visible to Validate so it can report them.
	for _, id := range specQueryIDs(spec) {
		if _, ok := used[id]; !ok {
			used[id] = nil
		}
	}
	for id, r := range used {
		if r == nil {
			delete(used, id)
		}
	}
	return used
}

func specQueryIDs(spec Spec) []string {
	var ids []string
	for _, t := range spec.Tiles {
		ids = append(ids, t.QueryID)
	}
	for _, c := range spec.Charts {
		ids = append(ids, c.QueryID)
	}
	for _, t := range spec.Tables {
		ids = append(ids, t.QueryID)
	}
	return ids
}

// Rerun recomputes a saved report on current data. Query ids are re-derived
// for the current principal, so hand-written report files may use readable
// ids. If the commentary no longer matches the new figures it is removed
// rather than left stale.
func (s *Session) Rerun(ctx context.Context, rep *Report) (*Report, map[string]*Result, error) {
	results := map[string]*Result{}
	remap := map[string]string{}
	out := *rep
	out.Principal = s.Principal
	out.Queries = map[string]Query{}
	for id, q := range rep.Queries {
		r, err := s.Run(ctx, q)
		if err != nil {
			return nil, nil, fmt.Errorf("query %s: %w", id, err)
		}
		results[r.ID] = r
		remap[id] = r.ID
		out.Queries[r.ID] = q
	}
	fix := func(id string) string {
		if n, ok := remap[id]; ok {
			return n
		}
		return id
	}
	spec := remapSpec(rep.Spec, fix)
	out.Exchange = remapTurns(rep.Exchange, fix)
	if spec.Commentary != "" {
		var list []*Result
		for _, r := range results {
			list = append(list, r)
		}
		if bad := CheckNarrative(spec.Commentary, list); len(bad) > 0 {
			spec.Commentary = ""
			spec.Assumptions = append(spec.Assumptions,
				"The saved commentary was removed because the figures have changed since it was written; ask again for fresh commentary")
		}
	}
	if problems := spec.Validate(results); len(problems) > 0 {
		return nil, nil, fmt.Errorf("saved report no longer valid: %v", problems)
	}
	out.Spec = spec
	if out.ID == "" {
		out.ID = "rpt_" + QueryID(Query{Measures: []string{spec.Title}}, s.Principal)[2:]
	}
	return &out, results, nil
}

// remapSpec is a copy of spec whose tiles, charts and tables name their
// queries as fix maps them.
func remapSpec(in Spec, fix func(string) string) Spec {
	spec := in
	spec.Tiles = append([]Tile(nil), in.Tiles...)
	spec.Charts = append([]Chart(nil), in.Charts...)
	spec.Tables = append([]Table(nil), in.Tables...)
	spec.Assumptions = append([]string(nil), in.Assumptions...)
	for i := range spec.Tiles {
		spec.Tiles[i].QueryID = fix(spec.Tiles[i].QueryID)
	}
	for i := range spec.Charts {
		spec.Charts[i].QueryID = fix(spec.Charts[i].QueryID)
	}
	for i := range spec.Tables {
		spec.Tables[i].QueryID = fix(spec.Tables[i].QueryID)
	}
	return spec
}

// remapTurns is the exchange with its query ids as fix maps them: a rerun
// re-derives them for the current principal.
func remapTurns(turns []Turn, fix func(string) string) []Turn {
	out := make([]Turn, len(turns))
	for i, turn := range turns {
		turn.Queries = slices.Clone(turn.Queries)
		for j := range turn.Queries {
			turn.Queries[j] = fix(turn.Queries[j])
		}
		out[i] = turn
	}
	return out
}

// Save writes the report definition (JSON) and its rendered page (HTML).
func (rep *Report) Save(dir string, results map[string]*Result) (htmlPath string, err error) {
	// Rendered first, so a spec that does not validate against the results
	// leaves nothing behind.
	var page bytes.Buffer
	if err := Render(&page, &rep.Spec, results, rep.Principal, rep.Now); err != nil {
		return "", fmt.Errorf("analytics: save report %s: %w", rep.ID, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("analytics: save report %s: %w", rep.ID, err)
	}
	// A Report holds only strings, numbers and nested structs, so marshaling
	// cannot fail.
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, rep.ID+".json"), b, 0o600); err != nil {
		return "", fmt.Errorf("analytics: save report %s definition: %w", rep.ID, err)
	}
	htmlPath = filepath.Join(dir, rep.ID+".html")
	if err := os.WriteFile(htmlPath, page.Bytes(), 0o600); err != nil {
		return htmlPath, fmt.Errorf("analytics: save report %s page: %w", rep.ID, err)
	}
	return htmlPath, nil
}

func LoadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("analytics: load report: %w", err)
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("report %s: %w", path, err)
	}
	return &rep, nil
}
