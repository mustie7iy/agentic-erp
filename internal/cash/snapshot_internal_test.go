package cash

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBucketSums: a snapshot's sums after the document level add each
// share of a flow to its bucket's sum of the share's item, source,
// certainty and bank, count the flows in it, start a bucket holding the
// document level's end the day after it, and keep no line for a share of
// nothing, as the document level keeps none (T53f).
func TestBucketSums(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 5, d, 0, 0, 0, 0, time.UTC) }
	bs := []Bucket{{Grain: "week", Start: day(15), End: day(21)}, {Grain: "week", Start: day(22), End: day(28)}}
	bank := uuid.New()
	b := newBucketSums(bs, day(18))
	s := &sums{}
	b.add(s, datedFlow{flow: flow{date: day(19), source: SourcePayable, certainty: CertaintyExpected, amount: -300}, currency: "CAD", bank: &bank,
		shares: []Share{{Item: "RENT", Amount: -300}, {Item: "TAX", Amount: 0}}})
	b.add(s, datedFlow{flow: flow{date: day(20), source: SourcePayable, certainty: CertaintyExpected, amount: -200}, currency: "CAD", bank: &bank,
		shares: []Share{{Item: "RENT", Amount: -200}}})
	b.add(s, datedFlow{flow: flow{date: day(23), source: SourceMemo, certainty: CertaintyPlanned, amount: 50}, currency: "CAD",
		shares: []Share{{Item: "", Amount: 50}}})
	var got []string
	for _, l := range b.lines(7) {
		got = append(got, fmt.Sprintf("%d %s..%s %s %s %q %d x%d bank=%v", l.flowNo, l.date.Format(time.DateOnly), l.end.Format(time.DateOnly), l.source,
			l.certainty, l.item, l.amount, l.count, l.bank != nil))
	}
	want := []string{
		`7 2026-05-18..2026-05-21 payable expected "RENT" -500 x2 bank=true`,
		`8 2026-05-22..2026-05-28 memo planned "" 50 x1 bank=false`,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sums:\n got %v\nwant %v", got, want)
	}
}
