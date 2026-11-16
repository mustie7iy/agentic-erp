package offboarding

import "testing"

// TestExportOrder: a table is read in its primary key's order, or by the
// row's whole text when it has none (no tenant table lacks one today, so
// only this test reaches that case).
func TestExportOrder(t *testing.T) {
	cases := map[string]struct {
		table tenantTable
		want  string
	}{
		"primary key":    {tenantTable{Key: []string{"tenant_id", "id"}}, `t."tenant_id", t."id"`},
		"no primary key": {tenantTable{}, "1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.table.order(); got != c.want {
				t.Errorf("order: got %s, want %s", got, c.want)
			}
		})
	}
}
