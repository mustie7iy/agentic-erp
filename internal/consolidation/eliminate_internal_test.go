package consolidation

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestMatchEdges: rows of zero leave no line, so a pair whose rows are
// all zero and match has no entry; a trading partner tenancy no longer
// lists is ordered by its id (here before code 2000); a pair's rows met from the
// partner's side first still make one difference, the lower code its
// company.
func TestMatchEdges(t *testing.T) {
	a, b, gone := uuid.MustParse("00000000-0000-7000-8000-00000000000a"), uuid.MustParse("00000000-0000-7000-8000-00000000000b"),
		uuid.MustParse("00000000-0000-7000-8000-0000000000ff")
	m := matching{codes: map[uuid.UUID]string{a: "1000", b: "2000"}, partners: map[uuid.UUID]string{a: "1000", b: "2000"},
		eliminationOf: map[string]string{"G1250": "IC", "G2250": "IC"}, tolerance: map[string]money.Minor{"IC": 0},
		differenceTo: map[string]string{"IC": "G6950"}}
	diffs, entries := m.match([]icRow{
		{company: b, line: 1, group: "G2250", partner: &a, gc: 0},
		{company: a, line: 1, group: "G1250", partner: &b, gc: 0},
		{company: a, line: 2, group: "G1250", partner: &gone, gc: 7},
	})
	var got []string
	for _, d := range diffs {
		got = append(got, fmt.Sprintf("%s %s/%s %s %d %d %d", d.elimination, d.companyCode, d.partnerCode, d.status, d.amount, d.partnerAmount, d.diff))
	}
	if g, w := strings.Join(got, " | "), "IC 1000/"+gone.String()+" outside_group 7 0 0 | IC 1000/2000 matched 0 0 0"; g != w {
		t.Errorf("the differences: got %s, want %s", g, w)
	}
	if len(entries) != 0 {
		t.Errorf("the entries: got %d, want none for a pair of zero rows", len(entries))
	}
}
