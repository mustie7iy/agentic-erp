package assets

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestGroupTaken: the group amount of what a share takes, by the first of
// ADR-0057 §8's three rules that applies.
func TestGroupTaken(t *testing.T) {
	for name, c := range map[string]struct {
		taken, booked, bookedGC, translated, want money.Minor
	}{
		"nothing left locally: all of the booked group amount": {taken: -1000, booked: -1000, bookedGC: -1087, translated: -1250, want: -1087},
		"a zero balance with a group amount, taken in full":    {taken: 0, booked: 0, bookedGC: 163, translated: 0, want: 163},
		"a group amount of the other sign: translated":         {taken: -500, booked: -1000, bookedGC: 30, translated: -625, want: -625},
		"otherwise in proportion, rounded half away from zero": {taken: -500, booked: -1000, bookedGC: -1087, translated: -625, want: -544},
		"in proportion with no group amount booked":            {taken: 600, booked: 1200, bookedGC: 0, translated: 652, want: 0},
		"a positive balance in proportion":                     {taken: 600, booked: 1200, bookedGC: 130435, translated: 652, want: 65218},
	} {
		t.Run(name, func(t *testing.T) {
			if got := groupTaken(c.taken, c.booked, c.bookedGC, c.translated); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
