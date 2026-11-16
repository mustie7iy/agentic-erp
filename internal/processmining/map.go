package processmining

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// mapChunk is how many cases a map reads with their events at a time:
// the map keeps their sums, never the cases. A variable for a test to
// read several chunks of a few cases.
var mapChunk = 1000

// topVariants is how many variants a map gives.
const topVariants = 10

// ends are the activities that end a case of each kind (ADR-0080 As
// built, T103b): a case whose last activity is none of its kind's is open.
var ends = map[string][]string{
	CasePurchaseOrder:   {"Pay invoice", "Close purchase order", "Cancel purchase order"},
	CaseSupplierInvoice: {"Pay invoice", "Reverse invoice"},
}

// ActivityCount is an activity of a map with its events and cases.
type ActivityCount struct {
	Activity          string
	Events, CaseCount int
}

// Pair is a directly-follows pair: To directly follows From within a
// case Count times, the median time between them MedianSeconds.
type Pair struct {
	From, To      string
	Count         int
	MedianSeconds int64
}

// Variant is a sequence of activities with the cases that ran it.
type Variant struct {
	Activities      []string
	CaseCount, Open int
	// SharePercent is its share of the map's cases in percent, two
	// decimals, rounded half away from zero.
	SharePercent string
}

// Map is a process map.
type Map struct {
	Process                       string
	LegalEntity                   uuid.UUID
	CompanyCode                   string
	From, To                      time.Time
	CaseCount, Open, VariantCount int
	Activities                    []ActivityCount
	Pairs                         []Pair
	Variants                      []Variant
}

// MapQuery asks for a map.
type MapQuery struct {
	Process     string
	LegalEntity uuid.UUID
	From, To    time.Time
}

// sums are what a map keeps of the cases it read.
type sums struct {
	cases, open int
	events      map[string]int
	withCases   map[string]int
	durations   map[[2]string][]int64
	variants    map[string]*Variant
}

// ReadMap reads the company's map of the process for the period: every
// case of its log, a chunk at a time, summed.
func (e *Endpoints) ReadMap(ctx context.Context, q MapQuery) (Map, error) {
	if err := valid(ctx, q.Process, q.From, q.To); err != nil {
		return Map{}, err
	}
	code, err := e.company(ctx, q.LegalEntity)
	if err != nil {
		return Map{}, err
	}
	s := sums{events: map[string]int{}, withCases: map[string]int{}, durations: map[[2]string][]int64{}, variants: map[string]*Variant{}}
	lq := Query{Process: q.Process, LegalEntity: q.LegalEntity, From: q.From, To: q.To}
	var after *cursor
	for {
		cases, err := e.cases(ctx, lq, after, mapChunk)
		if err != nil {
			return Map{}, err
		}
		if err := e.events(ctx, cases); err != nil {
			return Map{}, err
		}
		for _, c := range cases {
			s.add(c)
		}
		if len(cases) < mapChunk {
			break
		}
		last := cases[len(cases)-1]
		after = &cursor{Date: last.DocumentDate, ID: last.ID}
	}
	return s.result(Map{Process: q.Process, LegalEntity: q.LegalEntity, CompanyCode: code, From: q.From, To: q.To}), nil
}

// add sums a case: its activities, its directly-follows pairs with the
// time between them in whole seconds, and its variant.
func (s *sums) add(c Case) {
	s.cases++
	seen := map[string]bool{}
	names := make([]string, len(c.Events))
	for i, ev := range c.Events {
		names[i] = ev.Activity
		s.events[ev.Activity]++
		if !seen[ev.Activity] {
			seen[ev.Activity] = true
			s.withCases[ev.Activity]++
		}
		if i > 0 {
			prev := c.Events[i-1]
			pair := [2]string{prev.Activity, ev.Activity}
			s.durations[pair] = append(s.durations[pair], int64(ev.OccurredAt.Sub(prev.OccurredAt)/time.Second))
		}
	}
	open := len(names) == 0 || !slices.Contains(ends[c.Kind], names[len(names)-1])
	if open {
		s.open++
	}
	key := strings.Join(names, "\x00")
	v := s.variants[key]
	if v == nil {
		v = &Variant{Activities: names}
		s.variants[key] = v
	}
	v.CaseCount++
	if open {
		v.Open++
	}
}

// result is the map of the sums: activities by events then name, pairs
// by count then activities, the most frequent variants.
func (s *sums) result(m Map) Map {
	m.CaseCount, m.Open, m.VariantCount = s.cases, s.open, len(s.variants)
	m.Activities = make([]ActivityCount, 0, len(s.events))
	for a, n := range s.events {
		m.Activities = append(m.Activities, ActivityCount{Activity: a, Events: n, CaseCount: s.withCases[a]})
	}
	slices.SortFunc(m.Activities, func(a, b ActivityCount) int {
		return cmp.Or(cmp.Compare(b.Events, a.Events), cmp.Compare(a.Activity, b.Activity))
	})
	m.Pairs = make([]Pair, 0, len(s.durations))
	for p, d := range s.durations {
		m.Pairs = append(m.Pairs, Pair{From: p[0], To: p[1], Count: len(d), MedianSeconds: median(d)})
	}
	slices.SortFunc(m.Pairs, func(a, b Pair) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To))
	})
	variants := slices.Collect(maps.Values(s.variants))
	slices.SortFunc(variants, func(a, b *Variant) int {
		return cmp.Or(cmp.Compare(b.CaseCount, a.CaseCount), slices.Compare(a.Activities, b.Activities))
	})
	m.Variants = make([]Variant, 0, min(len(variants), topVariants))
	for _, v := range variants[:min(len(variants), topVariants)] {
		v.SharePercent = share(v.CaseCount, s.cases)
		m.Variants = append(m.Variants, *v)
	}
	return m
}

// median is the median of the durations, the lower middle of an even
// count.
func median(d []int64) int64 {
	slices.Sort(d)
	return d[(len(d)-1)/2]
}

// share is part of whole in percent with two decimals, rounded half away
// from zero, in integers: part and whole are counts, whole more than
// nought.
func share(part, whole int) string {
	hundredths := (part*20000 + whole) / (2 * whole)
	return fmt.Sprintf("%d.%02d", hundredths/100, hundredths%100)
}
