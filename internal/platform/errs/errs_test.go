package errs

import (
	"errors"
	"testing"
)

// TestWrapf: nil stays nil; an error gets the context and stays matchable.
func TestWrapf(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if err := Wrapf(nil, "ledger: scan account %s", "a"); err != nil {
			t.Errorf("got %v, want nil", err)
		}
	})
	t.Run("error", func(t *testing.T) {
		cause := errors.New("boom")
		err := Wrapf(cause, "ledger: scan account %s", "a")
		if got, want := err.Error(), "ledger: scan account a: boom"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !errors.Is(err, cause) {
			t.Errorf("got %v, want it to wrap the cause", err)
		}
	})
}
