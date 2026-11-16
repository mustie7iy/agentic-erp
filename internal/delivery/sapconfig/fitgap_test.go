package sapconfig

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// checkFitGap holds T104a's acceptance on an extract: the configuration
// and notes are Generate's, byte for byte; every note Generate returns
// appears in the report exactly once, under its object or among the
// defaults; a note the report adds sits on a left-out object; every
// left-out object says why; and the counts per outcome add up to the
// objects read.
func checkFitGap(t *testing.T, e *sap.Extract, opts Options) FitGap {
	t.Helper()
	want, wantNotes := Generate(e, opts)
	got, notes, r := FitGapReport(e, opts)
	wantDoc, _ := want.Marshal() // Config.Marshal cannot fail
	gotDoc, _ := got.Marshal()
	if !bytes.Equal(gotDoc, wantDoc) {
		t.Errorf("got a configuration other than Generate's:\n%s\nwant\n%s", gotDoc, wantDoc)
	}
	if !slices.Equal(notes, wantNotes) {
		t.Errorf("got notes\n%s\nwant Generate's\n%s", notesText(notes), notesText(wantNotes))
	}
	left := map[Note]int{}
	for _, n := range notes {
		left[n]++
	}
	for _, n := range r.Defaults {
		left[n]--
	}
	counts := map[Outcome]int{}
	for _, o := range r.Objects {
		counts[o.Outcome]++
		if o.Outcome == LeftOut && len(o.Notes) == 0 {
			t.Errorf("object %s %s: left out without a note, want one saying why", o.Table, o.Key)
		}
		for _, n := range o.Notes {
			if left[n]--; left[n] < 0 && o.Outcome != LeftOut {
				t.Errorf("object %s %s (%s): got note %q the generator did not return, want report notes on left-out objects only", o.Table, o.Key, o.Outcome, n)
			}
		}
	}
	for n, c := range left {
		if c > 0 {
			t.Errorf("note %q: %d not in the report, want each under its object or among the defaults", n, c)
		}
	}
	if sum := r.Counts.CarriedOver + r.Counts.CarriedWithNote + r.Counts.LeftOut; sum != r.Counts.Objects || r.Counts.Objects != len(r.Objects) {
		t.Errorf("got counts %+v over %d objects, want outcomes adding up to the objects read", r.Counts, len(r.Objects))
	}
	for outcome, n := range map[Outcome]int{CarriedOver: r.Counts.CarriedOver, CarriedWithNote: r.Counts.CarriedWithNote, LeftOut: r.Counts.LeftOut} {
		if counts[outcome] != n {
			t.Errorf("count of %s: got %d, want %d, the objects with it", outcome, n, counts[outcome])
		}
	}
	if r.Scope != FitGapScope {
		t.Errorf("got scope %q, want the statement that the report covers customizing only", r.Scope)
	}
	return r
}

// findObject finds a report's object, failing the test without it.
func findObject(t *testing.T, r FitGap, table, key string) Object {
	t.Helper()
	at := slices.IndexFunc(r.Objects, func(o Object) bool { return o.Table == table && o.Key == key })
	if at < 0 {
		t.Fatalf("object %s %s: got none, want it in the report", table, key)
	}
	return r.Objects[at]
}

func TestFitGapFromMockSAP(t *testing.T) {
	r := checkFitGap(t, sap.GenerateMock(42, "2026"), zoned())
	for _, c := range []struct {
		table, key string
		want       Outcome
	}{
		{"T001", "1000", CarriedWithNote},
		{"T007A", "1000/V1", CarriedOver},
		{"T052", "NT30", CarriedOver},
		{"T030", "1000/VST/140000", CarriedOver},
		{"SKB1", "1000/211000", CarriedOver},
		{"AGR_DEFINE", "Z_AP_CLERK", LeftOut},
		{"TCURX", "JPY", CarriedOver},
	} {
		t.Run(c.table+" "+c.key, func(t *testing.T) {
			if got := findObject(t, r, c.table, c.key).Outcome; got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	if !slices.IsSortedFunc(r.Objects, func(a, b Object) int { return strings.Compare(a.Table+"\x00"+a.Key, b.Table+"\x00"+b.Key) }) {
		t.Error("got objects out of order, want them in table and key order")
	}
	if len(r.Defaults) == 0 {
		t.Error("got no settings without an SAP object, want the approval tiers and holds the draft sets")
	}
}

// The generator's gaps: rows it reads and does not take, each left out
// with the note that says why, its own or the report's.
func TestFitGapLeftOut(t *testing.T) {
	cases := map[string]struct {
		change     func(e *sap.Extract, o *Options)
		table, key string
		note       string
	}{
		"a T030 key the configuration has no account for": {func(e *sap.Extract, _ *Options) {
			e.T030 = append(e.T030, sap.T030{BUKRS: "1000", KTOSL: "GBB", KONTS: "0000500000"})
		}, "T030", "1000/GBB/500000", "transaction key GBB is none the configuration's account determination holds"},
		"an earlier VST row overridden by a later one": {func(e *sap.Extract, _ *Options) {
			e.T030 = append([]sap.T030{{BUKRS: "1000", KTOSL: "VST", KONTS: "0000140001"}}, e.T030...)
		}, "T030", "1000/VST/140001", "a later T030 VST row names account 140000, which is taken"},
		"a second WRX account": {func(e *sap.Extract, _ *Options) {
			e.T030 = append(e.T030, sap.T030{BUKRS: "3000", KTOSL: "WRX", KONTS: "0000219999"})
		}, "T030", "3000/WRX/219999", "names accounts"},
		"a T074 row other than vendor down payments": {func(e *sap.Extract, _ *Options) {
			e.T074 = append(e.T074, sap.T074{BUKRS: "1000", KOART: "D", UMSKZ: "A", HKONT: "0000120000", SKONT: "0000120100"})
		}, "T074", "1000/D/A/120000", "only vendor down payments (K, A) are taken"},
		"a second down payment account": {func(e *sap.Extract, _ *Options) {
			e.T074 = append(e.T074, sap.T074{BUKRS: "3000", KOART: "K", UMSKZ: "A", HKONT: "0000212000", SKONT: "0000159999"})
		}, "T074", "3000/K/A/212000", "T074 names down payment accounts"},
		"a tolerance key other than PP": {func(e *sap.Extract, _ *Options) {
			e.T169G = append(e.T169G, sap.T169G{BUKRS: "1000", TOLSL: "BD", PROZ: "1", WERTO: "1.00"})
		}, "T169G", "1000/BD", "the configuration takes the price tolerance, key PP, alone"},
		"a house bank's second account": {func(e *sap.Extract, _ *Options) {
			hb := e.T012K[slices.IndexFunc(e.T012K, func(b sap.T012K) bool { return b.BUKRS == "1000" })]
			e.T012K = append(e.T012K, hb)
		}, "T012K", "", "has several accounts in T012K; only the first is taken"},
		"an account no type rule covers": {func(e *sap.Extract, _ *Options) {
			e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "0000000050", TXT50: "Low"})
		}, "SKB1", "1000/50", "no account type rule covers account 50"},
		"a TCURX row the conversion cannot use": {func(e *sap.Extract, _ *Options) {
			e.TCURX = append(e.TCURX, sap.TCURX{CURRKEY: "XAU", CURRDEC: "x"})
		}, "TCURX", "XAU", "amounts in XAU are not converted"},
		"a ranking for a method the company does not set up": {func(e *sap.Extract, _ *Options) {
			e.T042A = append(e.T042A, sap.T042A{ZBUKR: "1000", ZLSCH: "Q", RANGF: "1", HBKID: "X"})
		}, "T042A", "1000/Q", "which T042E does not set up for the company: left out"},
		"a default assignment": {func(e *sap.Extract, _ *Options) {
			e.TKA3A = append(e.TKA3A, sap.TKA3A{BUKRS: "1000", KOKRS: "A000", KSTAR: "0000600000", KOSTL: "NOPE"})
		}, "TKA3A", "1000/A000/600000", "not proposed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e, opts := sap.GenerateMock(42, "2026"), zoned()
			c.change(e, &opts)
			r := checkFitGap(t, e, opts)
			key := c.key
			if key == "" { // the house bank's: its second account
				hb := e.T012K[slices.IndexFunc(e.T012K, func(b sap.T012K) bool { return b.BUKRS == "1000" })]
				key = "1000/" + hb.HBKID + " (account 2)"
			}
			o := findObject(t, r, c.table, key)
			if o.Outcome != LeftOut {
				t.Errorf("got %s, want %s", o.Outcome, LeftOut)
			}
			if !slices.ContainsFunc(o.Notes, func(n Note) bool { return strings.Contains(n.Message, c.note) }) {
				t.Errorf("got notes %v, want one with %q", o.Notes, c.note)
			}
		})
	}
}

// An extract without the tables a setting comes from, or with values the
// draft cannot take: the defaults and company notes land under the
// company or among the settings without an SAP object, and the
// configuration is still Generate's.
func TestFitGapGapsAndDefaults(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T052, e.T169G, e.T030, e.ACDOCA = nil, nil, nil, nil
	e.T052 = []sap.T052{{ZTERM: "NT14", ZTAG1: "14", ZPRZ1: "2", TEXT1: "2% in 14 days"}}
	e.LFB1 = append(e.LFB1, sap.LFB1{LIFNR: "0000100001", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"})
	opts := zoned()
	delete(opts.TimeZones, "1000")
	opts.ExternalAmounts = true
	r := checkFitGap(t, e, opts)
	if got := findObject(t, r, "T052", "NT14").Outcome; got != CarriedWithNote {
		t.Errorf("payment term NT14: got %s, want %s", got, CarriedWithNote)
	}
	if slices.ContainsFunc(r.Objects, func(o Object) bool { return o.Table == "TCURX" }) {
		t.Error("got TCURX objects for an extract in external form, want none: the draft does not read it")
	}
	company := notesText(findObject(t, r, "T001", "1000").Notes)
	for _, want := range []string{"T169G key PP not found", "reconciliation accounts", "time_zone: not in the SAP extract"} {
		if !strings.Contains(company, want) {
			t.Errorf("got company notes\n%s\nwant one with %q", company, want)
		}
	}
	defaults := notesText(r.Defaults)
	for _, want := range []string{"has no journal", "release strategies", "provision these identity roles"} {
		if !strings.Contains(defaults, want) {
			t.Errorf("got defaults\n%s\nwant one with %q", defaults, want)
		}
	}
}

// Every other note path of the generator, run through the report's
// invariants: cost elements, payment methods and house banks.
func TestFitGapOtherExtracts(t *testing.T) {
	cases := map[string]func(e *sap.Extract, o *Options){
		"a cutover date":       func(_ *sap.Extract, o *Options) { o.Cutover = "20260101" },
		"no cost elements":     func(e *sap.Extract, _ *Options) { e.CSKB = nil },
		"no payment methods":   func(e *sap.Extract, _ *Options) { e.T042E = nil },
		"no house bank ranked": func(e *sap.Extract, _ *Options) { e.T042A = nil },
		"no names of methods":  func(e *sap.Extract, _ *Options) { e.T042Z = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e, opts := sap.GenerateMock(42, "2026"), zoned()
			change(e, &opts)
			checkFitGap(t, e, opts)
		})
	}
}

func TestFitGapText(t *testing.T) {
	r := FitGap{
		Scope:  FitGapScope,
		Counts: FitGapCounts{Objects: 2, CarriedOver: 1, LeftOut: 1},
		Objects: []Object{
			{Table: "T007A", Key: "1000/V1", Outcome: CarriedOver, Notes: []Note{}},
			{Table: "T030", Key: "1000/GBB/500000", Outcome: LeftOut, Notes: []Note{{Path: "companies[1000].account_determination", Message: "left out"}}},
		},
		Defaults: []Note{{Path: "roles", Message: "provision"}},
	}
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatalf("got %v, want the report written", err)
	}
	want := "Fit-gap report of the SAP customizing extract\n" + FitGapScope + "\n\n" +
		"Objects read: 2 (carried over 1, carried over with a note 0, left out 1)\n\n" +
		"T007A 1000/V1: carried over\n\n" +
		"T030 1000/GBB/500000: left out\n  companies[1000].account_determination: left out\n\n" +
		"Settings the draft sets with no SAP object behind them: 1\n  roles: provision\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	if err := r.WriteText(failingWriter{}); err == nil || !strings.Contains(err.Error(), "write fit-gap report") {
		t.Errorf("got %v, want the failed write reported", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
