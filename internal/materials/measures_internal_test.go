package materials

import (
	"context"
	"testing"
)

// TestUnitNamesOutsideAUnitOfWork: the units' names are read in the
// request's transaction, so outside one the read fails.
func TestUnitNamesOutsideAUnitOfWork(t *testing.T) {
	if _, err := unitNames(context.Background()); err == nil {
		t.Error("got nil error, want one")
	}
}

// TestCanonical: values are written as the database answers them, no
// trailing zeros after the point nor the point alone; whole numbers and
// what is no decimal stay as given.
func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{"2.50": "2.5", "2.000": "2", "0.120": "0.12", "10": "10", "100.5": "100.5", "1.0001": "1.0001",
		"x.0": "x.0", "": ""} {
		t.Run(in, func(t *testing.T) {
			if got := (Measures{GrossWeight: in}).canonical().GrossWeight; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestDecimalAbove: decimals compare exactly; a string that is no
// decimal is above nothing and nothing is above it.
func TestDecimalAbove(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.001", "1", true}, {"1", "1.000", false}, {"0.9", "10", false}, {"x", "1", false}, {"1", "x", false}} {
		t.Run(c.a+" "+c.b, func(t *testing.T) {
			if got := DecimalAbove(c.a, c.b); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
