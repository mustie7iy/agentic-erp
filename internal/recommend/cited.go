package recommend

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// Cited keeps the identifiers of what a run's reads returned, by what
// they are (an audit event by its sequence number, a control test, a
// recommendation), so an agent's check can hold the content's citations
// to them (T105f, T105g): a content cites a thing as its label and its
// identifier ("audit event 1234", "control test <id>"), and a citation of
// one no read of the run returned is refused, as a number no read
// returned is (§4). As with the figures, only what the run's tools read
// counts: what the agent's code read before the session does not, so the
// session's tool calls show where each citation came from. An observer of
// the run's tools keeps them, under the lock the tools share.
type Cited struct {
	ids map[string]map[string]bool // label to identifiers
}

// NewCited is an empty set of citations.
func NewCited() *Cited { return &Cited{ids: map[string]map[string]bool{}} }

// Citation is one thing a content cites: its label and its identifier.
type Citation struct {
	Label, ID string
}

func (c Citation) String() string { return c.Label + " " + c.ID }

// Keep is an observer of a read that keeps, under the label, the values
// its answer has at each path: keys joined by dots, a key ending in "[]"
// standing for each element of the array under it ("items[].id",
// "items[].last_test.id"). An answer that is not a success keeps nothing.
func (c *Cited) Keep(label string, paths ...string) func(apitools.Response) {
	return func(resp apitools.Response) {
		if !resp.OK() {
			return
		}
		d := json.NewDecoder(bytes.NewReader(resp.Body))
		d.UseNumber()
		var body any
		if d.Decode(&body) != nil {
			return // no JSON: nothing to keep, so nothing it holds can be cited
		}
		for _, p := range paths {
			for _, v := range at(body, strings.Split(p, ".")) {
				c.add(label, v)
			}
		}
	}
}

func (c *Cited) add(label, id string) {
	if c.ids[label] == nil {
		c.ids[label] = map[string]bool{}
	}
	c.ids[label][strings.ToLower(id)] = true
}

// at are the texts and numbers of v at the path's keys.
func at(v any, keys []string) []string {
	if len(keys) == 0 {
		switch x := v.(type) {
		case string:
			return []string{x}
		case json.Number:
			return []string{x.String()}
		}
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	key, each := strings.CutSuffix(keys[0], "[]")
	if !each {
		return at(m[key], keys[1:])
	}
	list, _ := m[key].([]any) // not a list: no element
	var out []string
	for _, e := range list {
		out = append(out, at(e, keys[1:])...)
	}
	return out
}

// citedID is what follows a label: a UUID or a whole number.
const citedID = `([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9]+)\b`

// Cites are the citations of the labels a text makes, in the text's
// order: each label (in any case) followed by white space and an
// identifier, a UUID or a whole number.
func Cites(text string, labels ...string) []Citation {
	type found struct {
		at int
		c  Citation
	}
	var all []found
	for _, l := range labels {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(l) + `\s+` + citedID)
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			all = append(all, found{m[0], Citation{Label: l, ID: strings.ToLower(text[m[2]:m[3]])}})
		}
	}
	slices.SortStableFunc(all, func(a, b found) int { return a.at - b.at })
	out := make([]Citation, 0, len(all))
	for _, f := range all {
		out = append(out, f.c)
	}
	return out
}

// Unknown are the citations no read of the run returned.
func (c *Cited) Unknown(cites []Citation) []Citation {
	var out []Citation
	for _, ci := range cites {
		if !c.ids[ci.Label][ci.ID] {
			out = append(out, ci)
		}
	}
	return out
}
