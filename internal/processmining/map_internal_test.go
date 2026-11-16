package processmining

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestShare: a share is in percent with two decimals, rounded half away
// from zero, computed in integers.
func TestShare(t *testing.T) {
	for _, c := range []struct {
		part, whole int
		want        string
	}{
		{1, 1, "100.00"}, {1, 3, "33.33"}, {2, 3, "66.67"}, {1, 8, "12.50"}, {1, 20000, "0.01"}, {1, 20001, "0.00"}, {0, 7, "0.00"},
	} {
		t.Run(fmt.Sprintf("%d of %d", c.part, c.whole), func(t *testing.T) {
			if got := share(c.part, c.whole); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestTopVariants: a map gives the ten most frequent variants, equal
// counts by their activities, while variant_count counts them all; the
// median of an odd count is its middle.
func TestTopVariants(t *testing.T) {
	s := sums{events: map[string]int{}, withCases: map[string]int{}, durations: map[[2]string][]int64{}, variants: map[string]*Variant{}}
	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	for i := range 12 {
		for range 1 + i%3 {
			s.add(Case{Kind: CasePurchaseOrder, Events: []Event{{Activity: fmt.Sprintf("A%02d", i), OccurredAt: at},
				{Activity: "Pay invoice", OccurredAt: at.Add(time.Duration(i) * time.Second)}}})
		}
	}
	m := s.result(Map{})
	var got []string
	for _, v := range m.Variants {
		got = append(got, fmt.Sprintf("%s %d %s", v.Activities[0], v.CaseCount, v.SharePercent))
	}
	want := "A02 3 12.50, A05 3 12.50, A08 3 12.50, A11 3 12.50, A01 2 8.33, A04 2 8.33, A07 2 8.33, A10 2 8.33, A00 1 4.17, A03 1 4.17"
	if s := strings.Join(got, ", "); s != want {
		t.Errorf("variants: got %s, want %s", s, want)
	}
	for name, c := range map[string]struct{ got, want int }{"variants": {m.VariantCount, 12}, "cases": {m.CaseCount, 24}, "open cases": {m.Open, 0}} {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", name, c.got, c.want)
		}
	}
	if p := m.Pairs[0]; p.From != "A02" || p.Count != 3 || p.MedianSeconds != 2 {
		t.Errorf("the first pair: got %+v, want A02 thrice, 2 s", p)
	}
}
