// Package docs holds the checks on the design documents and the tracker.
//
// docs/tracker.json is the only plan (07-transition §7.5). Scope may grow only
// by detailing what an ADR, the product owner, a review or a failed check asked
// for, so every leaf item names where it came from (PO 2026-10-25).
package docs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// baselineItems is the number of items carrying "baseline": the 966 items of
// 2026-10-25, when sources were first recorded, less the eight whose references
// already named the step they were split from or found in. The others' sources
// are in their references, ADRs and history. The count must match exactly, so
// removing a baseline item lowers it here and no new item can take its place.
const baselineItems = 958

// accepted lists the sources an item may name, several joined by " · ":
//
//	adr:ADR-0052           an accepted ADR asks for it
//	po:2026-10-25          a product owner decision of that day
//	review:T73j            a reviewer finding in that step
//	ci:batch6, ci:T73j     a failed or measured check in that batch or step
//	walkthrough:2026-10-22 the pilot walkthrough of that day
//	split:T73j             split from that approved step
//	design:01 §1.4         a design document's section
const accepted = `adr:ADR-NNNN, po:YYYY-MM-DD, review:Tn, ci:batchN or ci:Tn, walkthrough:YYYY-MM-DD, split:Tn or design:0N §n.n, joined by " · "`

var (
	sourcePattern = regexp.MustCompile(`^(adr:ADR-(\d{4})|(po|walkthrough):(\d{4}-\d{2}-\d{2})|(review|split|ci):(T\d+[a-z0-9]*)|ci:batch\d+|design:(0[1-7]) §(\d+(\.\d+)*))$`)
	stepPattern   = regexp.MustCompile(`\bT\d+[a-z0-9]*\b`)
	statusPattern = regexp.MustCompile(`(?m)^\*\*Status:\*\* accepted`)
)

type item struct {
	Title    string `json:"t"`
	Ref      string `json:"r"`
	Source   string `json:"src"`
	Children []item `json:"c"`
}

type leaf struct {
	path   string
	source string
}

// vouch counts, for each step a leaf names in its title or reference, one leaf
// that shows the step exists: a leaf whose own source cites no step. A group
// has no source of its own, and leaves citing steps (review:, split:, ci:Tn)
// could vouch for each other's invented steps, so neither counts. A step thus
// traces back to a leaf from the baseline, an ADR, the product owner, the
// walkthrough, a design section or a batch's check.
func vouch(it item, steps map[string]int) {
	for _, part := range strings.Split(it.Source, " · ") {
		if m := sourcePattern.FindStringSubmatch(part); m != nil && m[6] != "" {
			return
		}
	}
	names := map[string]bool{}
	for _, s := range stepPattern.FindAllString(it.Title+" "+it.Ref, -1) {
		names[s] = true
	}
	for s := range names {
		steps[s]++
	}
}

// walk collects the leaf items and, per step, the leaves that vouch for it.
func walk(items []item, path string, out []leaf, steps map[string]int) []leaf {
	for _, it := range items {
		p := it.Title
		if path != "" {
			p = path + " / " + it.Title
		}
		if len(it.Children) == 0 {
			vouch(it, steps)
			out = append(out, leaf{path: p, source: it.Source})
			continue
		}
		out = walk(it.Children, p, out, steps)
	}
	return out
}

// sourceExists says why a well-formed source names nothing real, or "".
func sourceExists(t *testing.T, m []string, steps map[string]int) string {
	t.Helper()
	switch {
	case m[2] != "":
		return acceptedADR(t, m[2])
	case m[4] != "":
		if _, err := time.Parse(time.DateOnly, m[4]); err != nil {
			return "is not a date"
		}
	case m[6] != "":
		if steps[m[6]] < 1 {
			return "names a step no independently sourced leaf names (one whose own src cites no step)"
		}
	case m[7] != "":
		return designSection(t, m[7], m[8])
	}
	return ""
}

// only reads the one file glob finds, or says why there is none.
func only(t *testing.T, glob, none string) ([]byte, string) {
	t.Helper()
	files, err := filepath.Glob(glob)
	if err != nil || len(files) != 1 {
		return nil, none
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read %s: %v", files[0], err)
	}
	return raw, ""
}

func acceptedADR(t *testing.T, n string) string {
	t.Helper()
	raw, why := only(t, filepath.Join("design", "adr", n+"-*.md"), "names no ADR in docs/design/adr")
	if why == "" && !statusPattern.Match(raw) {
		why = "names an ADR that is not accepted"
	}
	return why
}

func designSection(t *testing.T, doc, section string) string {
	t.Helper()
	raw, why := only(t, filepath.Join("design", doc+"-*.md"), "names no design document")
	if why == "" && !regexp.MustCompile(`(?m)^#+ `+regexp.QuoteMeta(section)+`\s`).Match(raw) {
		why = "names no section of design document " + doc
	}
	return why
}

func TestTrackerItemsNameTheirSource(t *testing.T) {
	raw, err := os.ReadFile("tracker.json")
	if err != nil {
		t.Fatalf("read tracker: %v", err)
	}
	var items []item
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("parse tracker: %v", err)
	}
	steps := map[string]int{}
	baseline := 0
	for _, l := range walk(items, "", nil, steps) {
		switch l.source {
		case "baseline":
			baseline++
			continue
		case "":
			t.Errorf("%s: no src; want %s", l.path, accepted)
			continue
		}
		for _, s := range strings.Split(l.source, " · ") {
			m := sourcePattern.FindStringSubmatch(s)
			if m == nil {
				t.Errorf("%s: src %q is not a known source; want %s", l.path, s, accepted)
			} else if why := sourceExists(t, m, steps); why != "" {
				t.Errorf("%s: src %q %s", l.path, s, why)
			}
		}
	}
	if baseline != baselineItems {
		t.Errorf("%d items carry src baseline, want %d: a new item names its source, and a removed one lowers baselineItems", baseline, baselineItems)
	}
}
