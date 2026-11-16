package config

import (
	"slices"
	"testing"
)

// TestCited: the settings a text cites as `path` = `value`, in its order;
// a path with a space, a setting in prose and a lone backticked text are
// none; Uncited leaves the rest of the text.
func TestCited(t *testing.T) {
	text := "Held above `companies[1000].invoice_tolerance.price_percent` = `2` or `a b` = `3`, " +
		"`purchasing.requisition_approval[0].up_to` = `10000.00` and `payables.invoice.review`; price_percent = 5."
	got := Cited(text)
	want := []Citation{
		{Path: "companies[1000].invoice_tolerance.price_percent", Value: "2", Text: "`companies[1000].invoice_tolerance.price_percent` = `2`"},
		{Path: "purchasing.requisition_approval[0].up_to", Value: "10000.00", Text: "`purchasing.requisition_approval[0].up_to` = `10000.00`"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Cited: got %+v, want %+v", got, want)
	}
	if got, want := Uncited(text), "Held above   or `a b` = `3`,   and `payables.invoice.review`; price_percent = 5."; got != want {
		t.Errorf("Uncited: got %q, want %q", got, want)
	}
	if got := Cited("no citation"); got != nil {
		t.Errorf("Cited of none: got %+v, want none", got)
	}
}
