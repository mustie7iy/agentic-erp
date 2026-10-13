package analytics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Session holds the results of one conversation with the analytics agent,
// so dashboards can refer to them by query_id.
type Session struct {
	Model     *Model
	Facts     *Facts
	Principal Principal
	Results   map[string]*Result
}

func NewSession(m *Model, f *Facts, p Principal) *Session {
	return &Session{Model: m, Facts: f, Principal: p, Results: map[string]*Result{}}
}

// Run executes a semantic query for the session's principal. The data was
// read through the API with the principal's permissions and company access.
func (s *Session) Run(q Query) (*Result, error) {
	r, err := Execute(s.Model, s.Facts, s.Principal, q)
	if err != nil {
		return nil, err
	}
	s.Results[r.ID] = r
	return r, nil
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
		CreatedAt: time.Now().UTC(), CreatedBy: createdBy,
	}
	for id, r := range used {
		rep.Queries[id] = r.Query
	}
	b, _ := json.Marshal(rep)
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
func (s *Session) Rerun(rep *Report) (*Report, map[string]*Result, error) {
	results := map[string]*Result{}
	remap := map[string]string{}
	out := *rep
	out.Principal = s.Principal
	out.Queries = map[string]Query{}
	for id, q := range rep.Queries {
		r, err := s.Run(q)
		if err != nil {
			return nil, nil, fmt.Errorf("query %s: %w", id, err)
		}
		results[r.ID] = r
		remap[id] = r.ID
		out.Queries[r.ID] = q
	}
	spec := rep.Spec
	spec.Tiles = append([]Tile(nil), rep.Spec.Tiles...)
	spec.Charts = append([]Chart(nil), rep.Spec.Charts...)
	spec.Tables = append([]Table(nil), rep.Spec.Tables...)
	spec.Assumptions = append([]string(nil), rep.Spec.Assumptions...)
	fix := func(id string) string {
		if n, ok := remap[id]; ok {
			return n
		}
		return id
	}
	for i := range spec.Tiles {
		spec.Tiles[i].QueryID = fix(spec.Tiles[i].QueryID)
	}
	for i := range spec.Charts {
		spec.Charts[i].QueryID = fix(spec.Charts[i].QueryID)
	}
	for i := range spec.Tables {
		spec.Tables[i].QueryID = fix(spec.Tables[i].QueryID)
	}
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

// Save writes the report definition (JSON) and its rendered page (HTML).
func (rep *Report) Save(dir string, results map[string]*Result) (htmlPath string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("analytics: save report %s: %w", rep.ID, err)
	}
	// A Report holds only strings, numbers and nested structs, so marshaling
	// cannot fail; rendering into memory with the fixed page template cannot
	// fail either (Render only reports errors from its writer).
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, rep.ID+".json"), b, 0o600); err != nil {
		return "", fmt.Errorf("analytics: save report %s definition: %w", rep.ID, err)
	}
	var page bytes.Buffer
	_ = Render(&page, &rep.Spec, results, rep.Principal) // into memory: cannot fail, see above
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
