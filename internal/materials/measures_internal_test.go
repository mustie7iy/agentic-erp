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
