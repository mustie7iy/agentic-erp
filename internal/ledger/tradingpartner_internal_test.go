package ledger

import (
	"testing"

	"github.com/google/uuid"
)

// TestSharedTradingPartner: a clearing's difference lines take the
// trading partner only when every item names the same one (ADR-0059 §4).
func TestSharedTradingPartner(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	for name, c := range map[string]struct {
		items []*uuid.UUID
		want  *uuid.UUID
	}{
		"every item names the same": {[]*uuid.UUID{&a, &a}, &a},
		"the first names none":      {[]*uuid.UUID{nil, &a}, nil},
		"a later one names none":    {[]*uuid.UUID{&a, nil}, nil},
		"two name different ones":   {[]*uuid.UUID{&a, &b}, nil},
		"no items":                  {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			items := make([]item, len(c.items))
			for i, tp := range c.items {
				items[i].tradingPartner = tp
			}
			got := sharedTradingPartner(items)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("lines naming one keep theirs", func(t *testing.T) {
		lines := []Line{{TradingPartnerID: &b}, {}}
		withTradingPartner(lines, &a)
		if *lines[0].TradingPartnerID != b || *lines[1].TradingPartnerID != a {
			t.Errorf("got %v and %v, want %v and %v", *lines[0].TradingPartnerID, *lines[1].TradingPartnerID, b, a)
		}
	})
}
