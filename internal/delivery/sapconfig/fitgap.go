package sapconfig

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Outcome is what the draft did with a customizing object it read
// (T104a, docs/migration.md "The fit-gap report").
type Outcome string

// The outcomes of a customizing object: taken into the draft as SAP has
// it, taken with a note the reviewers should read, or not taken.
const (
	CarriedOver     Outcome = "carried_over"
	CarriedWithNote Outcome = "carried_with_note"
	LeftOut         Outcome = "left_out"
)

// FitGapScope is the statement every report carries: it is computed from
// customizing alone (plan-breadth-week3 §T104a, default 13).
const FitGapScope = "This report covers SAP customizing only: it does not cover usage, changes or custom code."

// Object is one customizing object the generator read: its SAP table and
// key, its name where SAP gives one (a company's, an account's, a
// payment term's, a tax code's, a payment method's in the company's
// country, a role's), its outcome and the notes about it.
type Object struct {
	Table   string  `json:"table"`
	Key     string  `json:"key"`
	Name    string  `json:"name,omitempty"`
	Outcome Outcome `json:"outcome"`
	Notes   []Note  `json:"notes"`
}

// FitGapCounts are the objects read and how many had each outcome; the
// three outcomes add up to Objects.
type FitGapCounts struct {
	Objects         int `json:"objects"`
	CarriedOver     int `json:"carried_over"`
	CarriedWithNote int `json:"carried_with_note"`
	LeftOut         int `json:"left_out"`
}

// FitGap is the fit-gap report of an extract's customizing: per object the
// generator read, its outcome and notes, in table and key order, and the
// notes on settings the draft fills with no SAP object behind them
// (Defaults), so every note Generate returns appears in it once.
type FitGap struct {
	Scope    string       `json:"scope"`
	Counts   FitGapCounts `json:"counts"`
	Objects  []Object     `json:"objects"`
	Defaults []Note       `json:"defaults"`
}

// objRef names a customizing object; the zero value is none, for a note on
// a setting no SAP object gives.
type objRef struct{ table, key string }

// none is the objRef of a setting no SAP object gives.
var none = objRef{}

// object is a customizing object as the generator records it.
type object struct {
	Object
	leftOut bool
}

// read registers the object table/key as read and returns its reference;
// reading it again (a duplicate row) returns the same object.
func (g *gen) read(table, key string) objRef {
	o := objRef{table, key}
	if g.objects == nil {
		g.objects = map[objRef]*object{}
	}
	if g.objects[o] == nil {
		g.objects[o] = &object{Object: Object{Table: table, Key: key, Notes: []Note{}}}
	}
	return o
}

// named gives object o, which the generator has read, its name in SAP,
// where SAP gives one; a duplicate row keeps the first row's.
func (g *gen) named(o objRef, name string) objRef {
	if obj := g.objects[o]; obj.Name == "" {
		obj.Name = strings.TrimSpace(name)
	}
	return o
}

// note records a note for the reviewers on object o, or on a setting with
// no SAP object when o is the zero objRef.
func (g *gen) note(o objRef, path, msg string) {
	n := Note{Path: path, Message: msg}
	g.notes = append(g.notes, n)
	if o == (objRef{}) {
		g.defaults = append(g.defaults, n)
		return
	}
	obj := g.objects[g.read(o.table, o.key)]
	obj.Notes = append(obj.Notes, n)
}

// leaveOut records a note on object o and marks it left out of the draft.
func (g *gen) leaveOut(o objRef, path, msg string) {
	g.note(o, path, msg)
	g.objects[o].leftOut = true
}

// gap marks object o left out with a note the report alone carries: the
// generator never noted why it skips the object, and its notes stay as
// they were (T104a's acceptance).
func (g *gen) gap(o objRef, path, msg string) {
	obj := g.objects[g.read(o.table, o.key)]
	obj.Notes = append(obj.Notes, Note{Path: path, Message: msg})
	obj.leftOut = true
}

// report builds the fit-gap report from the objects read.
func (g *gen) report() FitGap {
	r := FitGap{Scope: FitGapScope, Objects: []Object{}, Defaults: append([]Note{}, g.defaults...)}
	for _, o := range g.objects {
		obj := o.Object
		switch {
		case o.leftOut:
			obj.Outcome = LeftOut
			r.Counts.LeftOut++
		case len(obj.Notes) > 0:
			obj.Outcome = CarriedWithNote
			r.Counts.CarriedWithNote++
		default:
			obj.Outcome = CarriedOver
			r.Counts.CarriedOver++
		}
		r.Objects = append(r.Objects, obj)
	}
	r.Counts.Objects = len(r.Objects)
	slices.SortFunc(r.Objects, func(a, b Object) int { return cmp.Or(cmp.Compare(a.Table, b.Table), cmp.Compare(a.Key, b.Key)) })
	return r
}

// FitGapReport generates the draft as Generate does and returns, beside the
// configuration and its notes, the fit-gap report of the extract's
// customizing. The configuration and notes are Generate's.
func FitGapReport(e *sap.Extract, opts Options) (*config.Config, []Note, FitGap) {
	g := newGen(e, opts)
	c := g.generate()
	return c, g.notes, g.report()
}

// WriteText writes the report for a person to read: the scope statement,
// the counts, each object with its outcome and notes, then the settings
// without an SAP object.
func (r FitGap) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Fit-gap report of the SAP customizing extract\n%s\n\n", r.Scope)
	fmt.Fprintf(&b, "Objects read: %d (carried over %d, carried over with a note %d, left out %d)\n",
		r.Counts.Objects, r.Counts.CarriedOver, r.Counts.CarriedWithNote, r.Counts.LeftOut)
	for _, o := range r.Objects {
		name := ""
		if o.Name != "" {
			name = " (" + o.Name + ")"
		}
		fmt.Fprintf(&b, "\n%s %s%s: %s\n", o.Table, o.Key, name, outcomeText[o.Outcome])
		for _, n := range o.Notes {
			fmt.Fprintf(&b, "  %s\n", n)
		}
	}
	fmt.Fprintf(&b, "\nSettings the draft sets with no SAP object behind them: %d\n", len(r.Defaults))
	for _, n := range r.Defaults {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("sapconfig: write fit-gap report: %w", err)
	}
	return nil
}

// outcomeText is how the text report names an outcome.
var outcomeText = map[Outcome]string{CarriedOver: "carried over", CarriedWithNote: "carried over with a note", LeftOut: "left out"}
