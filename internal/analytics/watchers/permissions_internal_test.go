package watchers

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
)

// TestReadPermissions: an alert of a watcher filtered by a center's name
// or node is shown only to a caller holding ledger.cost_center.read, as
// its query reads the centers (T56i2); one filtered by a center's code
// needs the journal's permissions alone.
func TestReadPermissions(t *testing.T) {
	m, err := analytics.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		filters []analytics.Filter
		want    string
	}{
		"of a node": {[]analytics.Filter{{Dim: "cost_center_node", Op: "in", Values: []string{"ADMIN"}}},
			"ledger.account.read ledger.cost_center.read ledger.entry.read partner.supplier.read tenancy.org.read"},
		"of a code": {[]analytics.Filter{{Dim: "cost_center", Op: "in", Values: []string{"CC100"}}},
			"ledger.account.read ledger.entry.read partner.supplier.read tenancy.org.read"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(readPermissions(m, Watcher{Metric: "expenses", Filters: c.filters}), " "); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
