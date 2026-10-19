package eval_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/eval"
)

// routedBeforeCertification is the models the platform's routes named when
// certification began (ADR-0039): its agents keep them until their types'
// first runs. The list only shrinks; a new model is certified, not added.
var routedBeforeCertification = []string{"claude-opus-5-5"}

// TestDefaultRoutesAreCertified is the release check of ADR-0039: a change
// of agents.DefaultRoutes to a model needs, for every suite's agent type
// on the alias, a committed report under evals/results that certifies it.
func TestDefaultRoutesAreCertified(t *testing.T) {
	if err := eval.RoutesCertified("../../evals", agents.DefaultRoutes, routedBeforeCertification); err != nil {
		t.Errorf("default routes: %v", err)
	}
}

// evalsWith is an evals directory with the test suite (alias erp-default)
// and the given files under it.
func evalsWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{"test/suite.yaml": "../../evals/test/suite.yaml", "configuration.yaml": "../../evals/configuration.yaml"} {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(raw)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRoutesCertified: a route to a model is released when a committed
// report certifies the suite's type there, or the model is one from
// before certification; a report that does not certify, another type's or
// another model's, an unrouted alias, or an unreadable report is refused.
func TestRoutesCertified(t *testing.T) {
	report := func(agent, model string, certifies bool) string {
		r := certifiable(20)
		r.Agent, r.Models = agent, []string{model}
		if !certifies {
			r.Complete = false
		}
		dir := t.TempDir()
		path, err := r.Write(dir)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	to := map[string]string{"erp-default": "claude-next"}
	for _, c := range []struct {
		name   string
		files  map[string]string
		routes map[string]string
		before []string
		want   string // in the error; empty: none
	}{
		{"certified", map[string]string{"results/test/claude-next/a.json": report("test", "claude-next", true)}, to, nil, ""},
		{"from before", map[string]string{}, to, []string{"claude-next"}, ""},
		{"no report", map[string]string{}, to, nil, "no committed report certifies test"},
		{"not certifying", map[string]string{"results/test/claude-next/a.json": report("test", "claude-next", false)}, to, nil, "no committed report"},
		{"another type's", map[string]string{"results/test/claude-next/a.json": report("migration", "claude-next", true)}, to, nil, "no committed report"},
		{"another model's", map[string]string{"results/test/claude-next/a.json": report("test", "claude-other", true)}, to, nil, "no committed report"},
		{"unrouted alias", map[string]string{}, map[string]string{"erp-analytics": "claude-next"}, nil, "which the routes do not route"},
		{"not a report", map[string]string{"results/test/claude-next/a.json": "{"}, to, nil, "is not a report"},
		{"unreadable", map[string]string{"results/test/claude-next/a.json/x": ""}, to, nil, "read "},
		{"bad suite", map[string]string{"bad/suite.yaml": "agent: ["}, to, nil, "suite"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := eval.RoutesCertified(evalsWith(t, c.files), c.routes, c.before)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("got %v, want no error", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
	if err := eval.RoutesCertified(t.TempDir(), to, nil); err == nil || !strings.Contains(err.Error(), "no suites") {
		t.Errorf("no suites: got %v, want an error", err)
	}
}
