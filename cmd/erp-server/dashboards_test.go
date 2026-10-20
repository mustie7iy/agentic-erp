package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
)

// dashboard is what the lint reads of a Grafana dashboard's JSON.
type dashboard struct {
	UID    string `json:"uid"`
	Title  string `json:"title"`
	Panels []struct {
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

// grafanaVars are the interval variables Grafana substitutes before it
// sends a query; the lint parses the query Prometheus would get.
var grafanaVars = strings.NewReplacer("$__rate_interval", "2m", "$__interval", "1m", "$__range", "6h")

// datasource is a dashboard naming its datasource.
var datasource = regexp.MustCompile(`"datasource"`)

// labelValues are the catalog labels whose values are a closed list: a
// panel matching any other value would show nothing.
func labelValues() map[string][]string {
	contexts := make([]string, 0, len(outbox.Contexts))
	for _, c := range outbox.Contexts {
		contexts = append(contexts, outbox.PublicContext(c))
	}
	return map[string][]string{
		"context":  contexts,
		"class":    api.Classes,
		"status":   agents.CallStatuses,
		"kind":     telemetry.TokenKinds,
		"decision": telemetry.Decisions,
	}
}

// TestDashboardsUseCatalog: every Grafana dashboard of the cell
// (deploy/helm/erp-cell/dashboards, ADR-0043, T54f) has a stable UID of
// its file's name, names no datasource (Grafana's default, the cell's
// Prometheus, serves it), and each panel's PromQL parses and names only
// series the cell exports or the SLO rules record, matches only labels
// those series carry (and values the catalog knows), and groups by only
// catalog labels.
func TestDashboardsUseCatalog(t *testing.T) {
	files, err := filepath.Glob("../../deploy/helm/erp-cell/dashboards/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Errorf("dashboards: got %d, want 8 (overview, SLOs, five contexts and agents)", len(files))
	}
	series := newMetrics(pgtest.Pool(t), config.Config{}, quiet()).Series()
	for _, name := range recorded(t) {
		series[name] = nil // a recorded series: its labels are checked against the catalog
	}
	uids := map[string]string{}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file) //nolint:gosec // G304: the glob's own files
			if err != nil {
				t.Fatal(err)
			}
			if datasource.Match(raw) {
				t.Errorf("names a datasource: panels use Grafana's default")
			}
			var d dashboard
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			if want := "erp-" + strings.TrimSuffix(filepath.Base(file), ".json"); d.UID != want {
				t.Errorf("uid: got %q, want %q", d.UID, want)
			}
			if other, ok := uids[d.UID]; ok {
				t.Errorf("uid %s: also %s's", d.UID, other)
			}
			uids[d.UID] = file
			if len(d.Panels) == 0 {
				t.Errorf("no panels")
			}
			for _, p := range d.Panels {
				if len(p.Targets) == 0 {
					t.Errorf("%s: no query", p.Title)
				}
				for _, target := range p.Targets {
					for _, problem := range lintQuery(grafanaVars.Replace(target.Expr), series) {
						t.Errorf("%s: %s", p.Title, problem)
					}
				}
			}
		})
	}
}

// TestDashboardLintRefuses: the lint finds each way a query can go wrong.
func TestDashboardLintRefuses(t *testing.T) {
	series := map[string][]string{
		"erp_events_published_total": {"context", "event"},
		"erp:recorded":               nil,
	}
	for name, query := range map[string]string{
		"not PromQL":         `sum(rate(erp_events_published_total[5m])`,
		"unknown series":     `erp_no_such_total`,
		"label not carried":  `erp_events_published_total{operation="x"}`,
		"value not known":    `erp_events_published_total{context="ledgers"}`,
		"grouping label":     `sum by (tenant) (erp_events_published_total)`,
		"matching label":     `erp_events_published_total * on (tenant) erp_events_published_total`,
		"included label":     `erp_events_published_total * on (event) group_left (tenant) erp_events_published_total`,
		"recorded, no label": `erp:recorded{tenant="a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := lintQuery(query, series); len(got) != 1 {
				t.Errorf("%s: got %q, want one problem", query, got)
			}
		})
	}
	if got := lintQuery(`sum by (event) (rate(erp_events_published_total{context=~"ledger|payments"}[5m]))`, series); len(got) != 0 {
		t.Errorf("a query in the catalog: got %q, want no problem", got)
	}
}

// recorded is the series the SLO rules record.
func recorded(t *testing.T) []string {
	t.Helper()
	rules, err := os.ReadFile("../../deploy/helm/erp-cell/rules/erp.rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^\s*- record: (\S+)$`).FindAllStringSubmatch(string(rules), -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatal("the SLO rules record no series")
	}
	return names
}

// lintQuery is what is wrong with one panel query, against the series and
// the catalog.
func lintQuery(query string, series map[string][]string) []string {
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.VectorSelector:
			problems = append(problems, lintSelector(n, series)...)
		case *parser.AggregateExpr:
			problems = append(problems, lintLabels(n.Grouping)...)
		case *parser.BinaryExpr:
			if n.VectorMatching != nil {
				problems = append(problems, lintLabels(n.VectorMatching.MatchingLabels)...)
				problems = append(problems, lintLabels(n.VectorMatching.Include)...)
			}
		}
		return nil
	})
	return problems
}

// lintSelector: a selector names a known series and matches only labels
// it carries, with values the catalog knows. A recorded series carries
// catalog labels alone.
func lintSelector(sel *parser.VectorSelector, series map[string][]string) []string {
	carried, ok := series[sel.Name]
	if !ok {
		return []string{sel.Name + " is not a series the cell exports or the SLO rules record"}
	}
	var problems []string
	values := labelValues()
	for _, m := range sel.LabelMatchers {
		if m.Name == model.MetricNameLabel {
			continue
		}
		if carried != nil && !slices.Contains(carried, m.Name) {
			problems = append(problems, fmt.Sprintf("%s matches label %s, which it does not carry", sel.Name, m.Name))
		}
		problems = append(problems, lintLabels([]string{m.Name})...)
		known, closed := values[m.Name]
		if !closed {
			continue
		}
		// A closed label is matched by literal alternatives only: a pattern
		// such as "pay.*" is refused, which errs on the strict side.
		for _, v := range strings.Split(m.Value, "|") {
			if !slices.Contains(known, v) {
				problems = append(problems, fmt.Sprintf("%s{%s}: value %q is not one the catalog knows %v", sel.Name, m.Name, v, known))
			}
		}
	}
	return problems
}

// lintLabels: each label is in the catalog (le is a histogram's own).
func lintLabels(names []string) []string {
	var problems []string
	for _, l := range names {
		if _, ok := telemetry.Labels[l]; !ok && l != "le" {
			problems = append(problems, "label "+l+" is not in the catalog")
		}
	}
	return problems
}
