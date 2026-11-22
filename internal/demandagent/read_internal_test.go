package demandagent

import (
	"context"
	"math/big"
	"strings"
	"testing"
)

// TestReadDay: a day that is no date stops the read before it reads;
// Forecast takes the day from the API's time zone, so only a broken clock
// or zone could give one.
func TestReadDay(t *testing.T) {
	if _, err := (&Agent{}).read(context.Background(), "", material{}, "2026-13-01"); err == nil || !strings.Contains(err.Error(), `the day "2026-13-01"`) {
		t.Errorf("got %v, want the day refused", err)
	}
}

// TestPlain: a quantity is written to three decimals, half away from
// zero, without trailing zeros, and a negative that rounds to nothing (a
// week whose reversals gave back a little more than it issued) as 0.
func TestPlain(t *testing.T) {
	for in, want := range map[string]string{"30/13": "2.308", "-1/10000": "0", "-3/2": "-1.5", "60": "60", "1/2000": "0.001"} {
		t.Run(in, func(t *testing.T) {
			r, _ := new(big.Rat).SetString(in) // the test's own literal
			if got := plain(r); got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}
