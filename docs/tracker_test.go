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
	"strconv"
	"strings"
	"testing"
	"time"
)

// baselineItems is the number of items carrying "baseline": the items of
// 2026-10-25, when sources were first recorded, whose sources are in their
// references, ADRs and history. It must match exactly and only ever falls: an
// item given its real source, or removed, lowers it here, so no new item can
// take a baseline item's place unseen in the diff.
const baselineItems = 949

// pilotItems is the number of leaves carrying "pilot": the pilot-ready list the
// product owner approved on 2026-10-31, the finish line of the first pilot. It
// must match exactly, so a leaf joining or leaving the pilot scope shows in the
// diff; a leaf's pilot value starts with the product owner's decision (po:),
// which TestTrackerPilotScope refuses a value without. 73 since the leaf
// loading SAP's foreign-currency open items was split in two, its GR/IR
// positions apart (fxopen, 2026-11-01), within the same decision. 75 since
// batch 31: a leaf split from a pilot leaf inherits its tag, so the tax
// registration number formats split from T61g joined (74), and the
// foreign-currency open item that HSL 0 drops from the pilot migration
// (review:batch31) joined as a data-correctness gap in its path (75). 76 since batch 32:
// preparing a later tax period's return first claims an earlier unfiled
// period's items (review:batch32), a gap in the pilot's return chain. 77
// since batch 33: T62g's sign-off part, in the approved scope, became its
// own leaf (T62h). 81 since batch 34: the staging half of RDS IAM logins
// split from T72h's leaf (78), the product owner had erp-web's egress to STS
// narrowed to the endpoint's subnets (po:2026-11-01), a security-posture
// leaf of the pilot (79), T73d2's pilot approver's walk-through and the two
// object summaries no operation reads split from it (T73d3, 80), and the
// product owner added P6's hold of chosen tax dates to open returns (T61o,
// po:2026-11-01, 81). 82 since batch 35: T73f2's walk-through by a pilot
// treasurer and AP lead, human acceptance, split into a leaf of its own
// (T73f3) that inherits the tag (82); T73h2's pilot walkthrough,
// Arabic-Indic digits and partners on an entry's pages split into T73h3,
// which inherits the tag (83); T73i2's walk-through by a pilot GL
// accountant and controller, human acceptance, split into T73i3, which
// inherits its tag (84). 85 since batch 36: T73e2's capture by upload,
// down payments, line changes, match changes and pilot AP clerk's
// walk-through split into T73e3, which inherits the tag (85). 88 since
// batch 37: T73g2's walk-through by a pilot treasury user, human
// acceptance, split into T73g3, which inherits the tag (86); the product
// owner added the end-to-end id collision on a new fiscal year's first
// payment run (review:T73g2) to the pilot (po:2026-11-01, 87); T73j2's
// downloads of the ledger reconciliation and the audit history, budgets
// at the pilot's volumes and pilot walk-through split into T73j4, which
// inherits the tag (88). 89 since batch 38: T73e3's down payments and
// pilot AP clerk's walk-through split into T73e4, which inherits the tag
// (89); capture by upload, decided by the product owner (po:2026-11-01),
// is a leaf of its own without the tag. 90 since T73e4: the pilot AP
// clerk's walk-through, human acceptance, split into T73e5, which inherits
// the tag (90). 91 since T73d3: the walk-through by a pilot approver, human
// acceptance, split into T73d4, which inherits the tag (91). 92 since
// T73j4: the reports' page budgets at the pilot's volumes and the pilot's
// walk-through split into T73j5, which inherits the tag (92). 94 since
// T73h3: amounts in Arabic-Indic digits (T73h4) and the walk-through by a
// pilot GL accountant and controller, human acceptance (T73h5), split
// from it, each inheriting the tag (94). 96 since T61j: the GST/HST
// return's sign-off by the tax specialist and the FPZ-500 for a Quebec
// registrant (P4), in the approved scope, split from it, each inheriting
// the tag (96). 97 since T62f: the UAE pack's sign-off by the tax
// specialist, in the approved scope, split from it, inheriting the tag.
// 98 since T61k: the VAT 201's sign-off by the tax specialist, in the
// approved scope, split from it, inheriting the tag. 99 since T61k's
// review: the emirate guard the sign-off waits for, inheriting the tag.
// 102 since T56h1: the SAP migration of controlling data split into
// T56h1-T56h4 for size (hierarchies and categories, OKB9 defaults,
// ACDOCA's centers and center totals), each inheriting the tag. 103 since
// T56h2: the OData mapping of the controlling tables, whose service names
// wait for the pilot's $metadata, split from it as T56h5, inheriting the
// tag. 111 since the specialists' answers of 2026-11-09 (po:2026-11-09):
// eight leaves changing pilot leaves' behaviour, each tagged as the leaf it
// changes: payment method order (the SAP payment methods leaf), an invoice's
// GR/IR line in another currency (the foreign-currency GR/IR positions
// leaf), TKA02, centers checked against the derivation at posting, values
// on inner groups, 24-character group codes and the CSKB slice at cutover
// (the T56h leaves), and the VAT 201's threshold measure (the VAT 201 leaf).
// 114 since its review (review round 1): A9 split into its profit center
// and segment halves (+1), the empty-WAERS house bank leaf tagged as the
// SAP house bank leaf it changes (+1), and SAP amounts in internal or
// external format (D10's planning default), tagged as the foreign-currency
// open items leaf (+1). 116 since the payment method order leaf was split
// in three (a4c): the company's run order, an item's fixed method and
// T042I's account, the two new leaves inheriting the tag (+2).
const pilotItems = 116

// accepted lists the sources an item may name, several joined by " · ":
//
//	adr:ADR-0052           an accepted ADR asks for it
//	po:2026-10-25          a product owner decision of that day
//	review:T73j            a reviewer finding in that step
//	review:batch17         a reviewer finding on that batch
//	ci:batch6, ci:T73j     a failed or measured check in that batch or step
//	walkthrough:2026-10-22 the pilot walkthrough of that day
//	split:T73j             split from that approved step
//	design:01 §1.4         a design document's section
const accepted = `adr:ADR-NNNN, po:YYYY-MM-DD, review:Tn or review:batchN, ci:batchN or ci:Tn, walkthrough:YYYY-MM-DD, split:Tn or design:0N §n.n, joined by " · "`

var (
	sourcePattern = regexp.MustCompile(`^(adr:ADR-(\d{4})|(po|walkthrough):(\d{4}-\d{2}-\d{2})|(review|split|ci):(T\d+[a-z0-9]*)|(?:ci|review):batch\d+|design:(0[1-7]) §(\d+(\.\d+)*))$`)
	stepPattern   = regexp.MustCompile(`\bT\d+[a-z0-9]*\b`)
	statusPattern = regexp.MustCompile(`(?m)^\*\*Status:\*\* accepted`)
)

type item struct {
	Status   string `json:"s"`
	Title    string `json:"t"`
	Ref      string `json:"r"`
	Source   string `json:"src"`
	Pilot    string `json:"pilot"`
	Children []item `json:"c"`
}

type leaf struct {
	path   string
	source string
	// pilot names why the leaf is in the pilot scope, in src's grammar, or
	// is empty; text is the leaf's title and reference, read for "post-pilot".
	pilot string
	text  string
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
			out = append(out, leaf{path: p, source: it.Source, pilot: it.Pilot, text: it.Title + " " + it.Ref})
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

// pilotGroups lists the groups carrying "pilot": the pilot scope is a list of
// leaves, so a group's tag would hide which of its leaves the pilot needs.
func pilotGroups(items []item, path string, out []string) []string {
	for _, it := range items {
		p := it.Title
		if path != "" {
			p = path + " / " + it.Title
		}
		if len(it.Children) == 0 {
			continue
		}
		if it.Pilot != "" {
			out = append(out, p)
		}
		out = pilotGroups(it.Children, p, out)
	}
	return out
}

// checkSources reports each " · "-separated part of value (a leaf's src or
// pilot) that is not a known source or names nothing real.
func checkSources(t *testing.T, path, field, value string, steps map[string]int) {
	t.Helper()
	for _, s := range strings.Split(value, " · ") {
		m := sourcePattern.FindStringSubmatch(s)
		if m == nil {
			t.Errorf("%s: %s %q is not a known source; want %s", path, field, s, accepted)
		} else if why := sourceExists(t, m, steps); why != "" {
			t.Errorf("%s: %s %q %s", path, field, s, why)
		}
	}
}

func readTracker(t *testing.T) []item {
	t.Helper()
	raw, err := os.ReadFile("tracker.json")
	if err != nil {
		t.Fatalf("read tracker: %v", err)
	}
	var items []item
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("parse tracker: %v", err)
	}
	return items
}

func TestTrackerItemsNameTheirSource(t *testing.T) {
	items := readTracker(t)
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
		checkSources(t, l.path, "src", l.source, steps)
	}
	if baseline != baselineItems {
		t.Errorf("%d items carry src baseline, want %d: a new item names its source, and a removed one lowers baselineItems", baseline, baselineItems)
	}
}

// TestTrackerPilotScope: the pilot scope (product owner 2026-10-31) is a set of
// leaves, each naming in "pilot" the sources that put it there, as src names
// those that put it in the tracker. A leaf the product owner put after the
// pilot cannot be in it, and the scope's size changes only in a reviewed diff.
func TestTrackerPilotScope(t *testing.T) {
	items := readTracker(t)
	steps := map[string]int{}
	tagged := 0
	for _, l := range walk(items, "", nil, steps) {
		if l.pilot == "" {
			continue
		}
		tagged++
		checkSources(t, l.path, "pilot", l.pilot, steps)
		if !strings.HasPrefix(l.pilot, "po:") {
			t.Errorf("%s: pilot %q does not start with a product owner decision; want po:YYYY-MM-DD first", l.path, l.pilot)
		}
		if strings.Contains(l.text, "post-pilot") {
			t.Errorf("%s: carries pilot but says post-pilot; want one or the other", l.path)
		}
	}
	for _, p := range pilotGroups(items, "", nil) {
		t.Errorf("%s: a group carries pilot; want it on the leaves the pilot needs", p)
	}
	if tagged != pilotItems {
		t.Errorf("%d leaves carry pilot, want %d: a change to the pilot scope names the product owner's decision and updates pilotItems", tagged, pilotItems)
	}
}

// summaryStatus is the status a summary item's children give it (07 §7.5):
// done when every child is done, to do when every child is still to do,
// partly done otherwise. A child without a known status is named instead.
func summaryStatus(children []item) (string, string) {
	done, todo := 0, 0
	for _, c := range children {
		switch c.Status {
		case "D":
			done++
		case "T":
			todo++
		case "P":
		default:
			return "", c.Title
		}
	}
	switch len(children) {
	case done:
		return "D", ""
	case todo:
		return "T", ""
	}
	return "P", ""
}

// summaryMismatches lists the summary items (items with a status and
// children) whose status is not the one their children give them; groups,
// which carry no status, are only walked.
func summaryMismatches(items []item, path string, out []string) []string {
	for _, it := range items {
		p := it.Title
		if path != "" {
			p = path + " / " + it.Title
		}
		if len(it.Children) == 0 {
			continue
		}
		if it.Status != "" {
			want, untracked := summaryStatus(it.Children)
			switch {
			case untracked != "":
				out = append(out, p+": child "+untracked+" has no status or an unknown one; want D, P or T")
			case it.Status != want:
				out = append(out, p+": status "+it.Status+", want "+want+" from its children")
			}
		}
		out = summaryMismatches(it.Children, p, out)
	}
	return out
}

// TestTrackerSummaryStatus: a summary item's status is derived from its
// children, never set apart from them, so a summary cannot read as done while
// one of its children is open, or stay open once every child is done.
func TestTrackerSummaryStatus(t *testing.T) {
	for _, m := range summaryMismatches(readTracker(t), "", nil) {
		t.Error(m)
	}
}

// TestSummaryStatus: the status children give their summary item.
func TestSummaryStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		children []string
		want     string
		named    string
	}{
		"every child done":          {[]string{"D", "D"}, "D", ""},
		"every child to do":         {[]string{"T", "T"}, "T", ""},
		"one child open":            {[]string{"D", "T"}, "P", ""},
		"a child partly done":       {[]string{"T", "P"}, "P", ""},
		"every child partly":        {[]string{"P", "P"}, "P", ""},
		"a child of no status":      {[]string{"D", ""}, "", "child 1"},
		"a child of unknown status": {[]string{"X", "D"}, "", "child 0"},
	} {
		t.Run(name, func(t *testing.T) {
			var children []item
			for i, s := range tc.children {
				children = append(children, item{Status: s, Title: "child " + strconv.Itoa(i)})
			}
			got, named := summaryStatus(children)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if named != tc.named {
				t.Errorf("named %q, want %q", named, tc.named)
			}
		})
	}
}

// TestSourcePattern: the shapes a source may take, and some it may not.
func TestSourcePattern(t *testing.T) {
	for src, want := range map[string]bool{
		"review:T73j":    true,
		"review:batch17": true,
		"ci:batch6":      true,
		"ci:T73j":        true,
		"review:foo":     false,
		"review:batch":   false,
		"review:batch1b": false,
		"split:batch17":  false,
	} {
		t.Run(src, func(t *testing.T) {
			if got := sourcePattern.MatchString(src); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
