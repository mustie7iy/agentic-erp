package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// RoutesCertified checks the platform's default routes
// (agents.DefaultRoutes) before a release (ADR-0039): a change of them
// moves every tenant at once, so the model each suite's alias routes to
// must be certified for the suite's agent type by a report committed under
// evals/results/<type>/<model>/ that certifies a level (Report.Certifies).
// before names the models the platform routed to when certification
// began, which agents keep until their types' first runs, as agents from
// before keep their levels. evals is the evals directory; every gap is
// returned.
func RoutesCertified(evals string, routes map[string]string, before []string) error {
	suites, _ := filepath.Glob(filepath.Join(evals, "*", "suite.yaml")) // the pattern is well-formed
	if len(suites) == 0 {
		return fmt.Errorf("eval: no suites under %s", evals)
	}
	var gaps []error
	for _, path := range suites {
		s, err := Load(path)
		if err != nil {
			return err
		}
		model, routed := routes[s.Alias]
		switch {
		case !routed:
			gaps = append(gaps, fmt.Errorf("eval: %s runs on %s, which the routes do not route", s.Agent, s.Alias))
			continue
		case slices.Contains(before, model):
			continue
		}
		ok, err := certifiedBy(filepath.Join(evals, "results", s.Agent, model), s.Agent, model)
		switch {
		case err != nil:
			return err
		case !ok:
			gaps = append(gaps, fmt.Errorf("eval: %s routes to %s, on which no committed report certifies %s: run its suite there first", s.Alias, model, s.Agent))
		}
	}
	return errors.Join(gaps...)
}

// certifiedBy reports whether a report in dir certifies agent on model.
// A committed report that does not read is an error: a release is not
// checked against files it cannot read.
func certifiedBy(dir, agent, model string) (bool, error) {
	reports, _ := filepath.Glob(filepath.Join(dir, "*.json")) // the pattern is well-formed
	for _, path := range reports {
		raw, err := os.ReadFile(path) // #nosec G304 -- a committed report under the evals directory
		if err != nil {
			return false, fmt.Errorf("eval: read %s: %w", path, err)
		}
		var r Report
		if err := json.Unmarshal(raw, &r); err != nil {
			return false, fmt.Errorf("eval: %s is not a report: %w", path, err)
		}
		if _, m, err := r.Certifies(); err == nil && m == model && r.Agent == agent {
			return true, nil
		}
	}
	return false, nil
}
