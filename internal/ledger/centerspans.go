package ledger

import (
	"time"

	"github.com/google/uuid"
)

// A center's effective-dated periods (ADR-0050 §3) are spans: [From, To),
// To the zero time when open-ended, with the attributes postings dated in
// them read. A center's spans are contiguous and in date order. Changes are
// computed on spans in Go and written as the difference: the spans that
// are no longer there are retired, the new ones inserted (periods are
// written once, 000125).
type span[A comparable] struct {
	ID   uuid.UUID // the period's row; uuid.Nil for a span not yet written
	From time.Time
	To   time.Time // exclusive; zero: open-ended
	Attr A
}

// forever stands in for an open end in comparisons.
var forever = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// day is t's calendar date at midnight UTC, as dates are compared.
func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// endOf is the span's end, forever when open.
func endOf(t time.Time) time.Time {
	if t.IsZero() {
		return forever
	}
	return t
}

// openOf is an end as spans hold it: zero for forever.
func openOf(t time.Time) time.Time {
	if t.Equal(forever) {
		return time.Time{}
	}
	return t
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// validity is the first span's start and the last span's end (zero:
// open-ended); ok is false for no spans.
func validity[A comparable](spans []span[A]) (from, to time.Time, ok bool) {
	if len(spans) == 0 {
		return time.Time{}, time.Time{}, false
	}
	return spans[0].From, spans[len(spans)-1].To, true
}

// within reports whether [from, to) lies within the spans' validity.
func within[A comparable](spans []span[A], from, to time.Time) bool {
	first, last, ok := validity(spans)
	return ok && !from.Before(first) && !endOf(to).After(endOf(last))
}

// setFrom applies f to the attributes of [from, to), splitting the spans
// it starts or ends in, and joins neighbours that end up alike. The range
// lies within the spans' validity.
func setFrom[A comparable](spans []span[A], from, to time.Time, f func(A) A) []span[A] {
	var out []span[A]
	for _, s := range spans {
		end := endOf(s.To)
		if s.From.Before(from) {
			out = append(out, span[A]{From: s.From, To: openOf(earlier(end, from)), Attr: s.Attr})
		}
		if lo, hi := later(s.From, from), earlier(end, endOf(to)); lo.Before(hi) {
			out = append(out, span[A]{From: lo, To: openOf(hi), Attr: f(s.Attr)})
		}
		if end.After(endOf(to)) {
			out = append(out, span[A]{From: later(s.From, endOf(to)), To: s.To, Attr: s.Attr})
		}
	}
	return keepIDs(spans, joined(out))
}

// redated gives the spans the validity [from, to): spans outside it are
// dropped, the first and last cut or stretched to it. ok is false when
// nothing is left.
func redated[A comparable](spans []span[A], from, to time.Time) ([]span[A], bool) {
	var out []span[A]
	for _, s := range spans {
		if !endOf(s.To).After(from) || !s.From.Before(endOf(to)) {
			continue
		}
		out = append(out, span[A]{From: later(s.From, from), To: openOf(earlier(endOf(s.To), endOf(to))), Attr: s.Attr})
	}
	if len(out) == 0 {
		return nil, false
	}
	out[0].From, out[len(out)-1].To = from, to
	return keepIDs(spans, out), true
}

// joined merges neighbours with equal attributes.
func joined[A comparable](spans []span[A]) []span[A] {
	var out []span[A]
	for _, s := range spans {
		if n := len(out); n > 0 && out[n-1].Attr == s.Attr {
			out[n-1].To = s.To
			continue
		}
		out = append(out, s)
	}
	return out
}

// keepIDs gives each new span the id of an old one it is identical to:
// that period is kept as it is.
func keepIDs[A comparable](old, spans []span[A]) []span[A] {
	for i := range spans {
		spans[i].ID = uuid.Nil
		for _, o := range old {
			if same(o, spans[i]) {
				spans[i].ID = o.ID
			}
		}
	}
	return spans
}

func same[A comparable](a, b span[A]) bool {
	return a.From.Equal(b.From) && a.To.Equal(b.To) && a.Attr == b.Attr
}

// diffSpans reports whether the spans differ from old, and lists the old
// periods to retire (those not kept) and the new spans to insert.
func diffSpans[A comparable](old, spans []span[A]) (retire []uuid.UUID, insert []span[A], differ bool) {
	kept := map[uuid.UUID]bool{}
	for _, s := range spans {
		if s.ID == uuid.Nil {
			insert = append(insert, s)
		}
		kept[s.ID] = true
	}
	for _, o := range old {
		if !kept[o.ID] {
			retire = append(retire, o.ID)
		}
	}
	return retire, insert, len(retire) > 0 || len(insert) > 0
}
