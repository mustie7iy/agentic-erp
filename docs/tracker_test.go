// Package docs holds the checks on the design documents and the tracker.
//
// docs/tracker.json is the only plan (07-transition §7.5). Scope may grow only
// by detailing what an ADR, the product owner, a review or a failed check asked
// for, so every item names where it came from (PO 2026-10-25).
package docs

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// baselineItems is the number of items on 2026-10-25, when sources were first
// recorded. Their sources are in their references, ADRs and commit history, so
// they carry "baseline". The count may only fall: a new item names its source.
const baselineItems = 966

// sourcePattern lists the accepted sources of an item:
//
//	adr:ADR-0052           an accepted ADR asks for it
//	po:2026-10-25          a product owner decision of that day
//	review:T73j            a reviewer finding in that step
//	ci:batch6, ci:T73j     a failed or measured check in that batch or step
//	walkthrough:2026-10-22 the pilot walkthrough of that day
//	split:T73j             split from that approved step
//	design:01 §1.4         a design document's section
var sourcePattern = regexp.MustCompile(`^(adr:ADR-\d{4}|po:\d{4}-\d{2}-\d{2}|review:T\d+[a-z0-9]*|ci:[A-Za-z0-9-]+|walkthrough:\d{4}-\d{2}-\d{2}|split:T\d+[a-z0-9]*|design:0[1-7] §\d+(\.\d+)*)$`)

type item struct {
	Title    string `json:"t"`
	Source   string `json:"src"`
	Children []item `json:"c"`
}

type leaf struct {
	path   string
	source string
}

func leaves(items []item, path string, out []leaf) []leaf {
	for _, it := range items {
		p := it.Title
		if path != "" {
			p = path + " / " + it.Title
		}
		if len(it.Children) == 0 {
			out = append(out, leaf{path: p, source: it.Source})
			continue
		}
		out = leaves(it.Children, p, out)
	}
	return out
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
	baseline := 0
	for _, l := range leaves(items, "", nil) {
		if l.source == "baseline" {
			baseline++
			continue
		}
		if l.source == "" {
			t.Errorf("%s: no src; want one of adr:, po:, review:, ci:, walkthrough:, split:, design:", l.path)
			continue
		}
		for _, s := range strings.Split(l.source, " · ") {
			if !sourcePattern.MatchString(s) {
				t.Errorf("%s: src %q is not a known source", l.path, s)
			}
		}
	}
	if baseline > baselineItems {
		t.Errorf("%d items carry src baseline, want at most %d: a new item names its source", baseline, baselineItems)
	}
}
