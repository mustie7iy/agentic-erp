package inventory

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

// TestMovementTypes (batch 98S integration, S1e2 with S2b4): each
// movement type's direction, what posts it and whether a person reverses
// it, and whether it carries batches and serial numbers, as one table, so
// that a type added by one slice keeps every other's rules: 122 and 123
// are an order's goods movement, as 101 and 102 are, posted and reversed
// by procurement alone; 653 is a customer's return, posted by sales'
// return receipt and not reversed yet; all three carry batches and serial
// numbers. A reversal reverses its forward type with the opposite sign.
func TestMovementTypes(t *testing.T) {
	// kind is who posts the type: "person" through Post, or the document
	// that does.
	kind := func(mt movementType) string {
		switch {
		case mt.byPerson():
			return "person"
		case mt.order:
			return "order"
		case mt.count:
			return "count"
		case mt.delivery:
			return "delivery"
		case mt.transport:
			return "transport"
		case mt.intercompany:
			return "intercompany"
		case mt.customerReturn:
			return "customer return"
		}
		return "reversal"
	}
	want := map[string]string{
		"101": "+ order 102 tracked", "102": "- order of 101 tracked", "122": "- order 123 tracked", "123": "+ order of 122 tracked",
		"201": "- person 202 tracked", "202": "+ reversal of 201 tracked", "301": "- person 302 tracked", "302": "+ reversal of 301 tracked",
		"311": "- person 312 tracked", "312": "+ reversal of 311 tracked", "351": "- transport - untracked",
		"501": "+ person 502 tracked", "502": "- reversal of 501 tracked", "551": "- person 552 tracked", "552": "+ reversal of 551 tracked",
		"561": "+ person 562 tracked", "562": "- reversal of 561 tracked", "601": "- delivery 602 tracked", "602": "+ delivery of 601 tracked",
		"643": "- intercompany - untracked", "653": "+ customer return - tracked", "701": "+ count - untracked", "702": "- count - untracked",
	}
	codes := slices.Sorted(maps.Keys(movementTypes))
	if got, wantCodes := codes, slices.Sorted(maps.Keys(want)); !slices.Equal(got, wantCodes) {
		t.Fatalf("the movement types: got %v, want %v", got, wantCodes)
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			mt := movementTypes[code]
			sign, pair, tracked := "+", mt.reversal, "untracked"
			if mt.sign < 0 {
				sign = "-"
			}
			if mt.reverses != "" {
				pair = "of " + mt.reverses
				if r := movementTypes[mt.reverses]; r.reversal != code || r.sign != -mt.sign {
					t.Errorf("its forward type %s: got reversal %q, sign %d; want %s, %d", mt.reverses, r.reversal, r.sign, code, -mt.sign)
				}
			}
			if pair == "" {
				pair = "-"
			}
			if mt.tracked {
				tracked = "tracked"
			}
			if got := fmt.Sprintf("%s %s %s %s", sign, kind(mt), pair, tracked); got != want[code] {
				t.Errorf("got %s, want %s", got, want[code])
			}
		})
	}
}
