package recommend

import (
	"strings"
	"testing"
)

// TestInputNumbersAreNotRead: no number a read's input wrote is read
// from that read's answer, wherever it stood in the input and however
// the answer gives it back: an amount with other zeros at its end, a
// number literal, a list typed as one text and answered as numbers, the
// days of a date, a number in a nested list. What the answer writes of
// its own is read, and so is the prompt's.
func TestInputNumbersAreNotRead(t *testing.T) {
	in := supplied(map[string]any{"customer_id": "01a1aa00-0000-7000-8000-000000000c02", "amount": "1617.010", "share": 0.75,
		"aging_days": "30,1600", "key_date": "2026-12-31", "more": map[string]any{"pages": []any{float64(7)}}})
	f := newFigures("Customer 100042.")
	f.add([]byte(`{"amount": {"amount": "1617.01", "currency": "CAD"}, "share": 0.75, "aging_days": [30, 1600], "key_date": "2026-12-31",
		"page": 7, "exposure": {"amount": "1470.00", "currency": "CAD"}, "rate": 1.25, "open_item_count": 2}`), in)
	for text, want := range map[string]string{
		"1617.01 CAD at 0.75, days 30 and 1600, on 2026-12-31, page 7": "1617.01 0.75 30 1600 2026 12 31 7",
		"1470.00 CAD at 1.25 on 2 items of customer 100042":            "",
	} {
		t.Run(text, func(t *testing.T) {
			if got := strings.Join(f.Unread(text), " "); got != want {
				t.Errorf("numbers not read: got %q, want %q", got, want)
			}
		})
	}
	if f.Returned("1617.01") {
		t.Error("the amount the input asked about: got returned as money, want not")
	}
}
