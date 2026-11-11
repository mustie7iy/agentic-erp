package consolidation

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestInterestShare (ADR-0059 §9's translation share; ADR-0005): the
// non-controlling interest's part of a member's translation difference is
// the difference times the part the parent does not hold, rounded once,
// half away from zero, and the owners have the rest, so the two always
// sum to the difference; the parent and a member held whole have none.
func TestInterestShare(t *testing.T) {
	pct := func(s string) *string { return &s }
	cases := []struct {
		name       string
		difference money.Minor
		ownership  *string
		want       money.Minor
	}{
		{"a gain at 80%", -11000, pct("80.0000"), -2200},
		{"a loss at 80%", 11000, pct("80.0000"), 2200},
		{"half a minor unit rounds away from zero", 5, pct("90.0000"), 1},
		{"minus half a minor unit rounds away from zero", -5, pct("90.0000"), -1},
		{"below half rounds to zero", 4, pct("90.0000"), 0},
		{"a third not held", -100, pct("66.6667"), -33},
		{"two thirds not held", 100, pct("33.3333"), 67},
		{"held whole", -11000, pct("100.0000"), 0},
		{"the parent", -11000, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := interestShare(c.difference, c.ownership)
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if got != c.want {
				t.Errorf("the interest's share: got %v, want %v", got, c.want)
			}
		})
	}
}

// TestReserves: each company's reserve from its opening and closing
// translation difference, none being zero, in the members' order, the
// difference shared once per member; a share out of range is refused,
// never wrapped.
func TestReserves(t *testing.T) {
	eighty, whole := "80.0000", "100.0000"
	sub, other, parent := uuid.New(), uuid.New(), uuid.New()
	ms := []Member{{LegalEntityID: sub, CompanyCode: "1000", Role: "member", OwnershipPercent: &eighty},
		{LegalEntityID: other, CompanyCode: "1500", Role: "member", OwnershipPercent: &whole}, {LegalEntityID: parent, CompanyCode: "2000", Role: "parent"}}
	got, err := reservesOf(ms, map[uuid.UUID]money.Minor{sub: -3003}, map[uuid.UUID]money.Minor{sub: -14010, other: 250})
	if err != nil {
		t.Fatalf("got %v, want no error", err)
	}
	var lines string
	for _, v := range got {
		lines += fmt.Sprintf("%s %d+%d=%d interest %d; ", v.member.CompanyCode, v.opening, v.difference(), v.closing, v.nci)
	}
	// 20% of -110.07 is -22.014: -22.01, and the owners have -88.06.
	if w := "1000 -3003+-11007=-14010 interest -2201; 1500 0+250=250 interest 0; 2000 0+0=0 interest 0; "; lines != w {
		t.Errorf("the reserves: got %s, want %s", lines, w)
	}
	beyond := "-1e30"
	if _, err := reservesOf([]Member{{CompanyCode: "1000", OwnershipPercent: &beyond}}, nil, map[uuid.UUID]money.Minor{uuid.Nil: 1 << 40}); err == nil {
		t.Error("a share beyond an amount's range: got no error, want one")
	}
}

// TestComprehensiveTotals: with a member held at 80%, total comprehensive
// income's share of non-controlling interests is their share of the
// profit and of the translation difference, the owners have the rest in
// each column, and each reserve's two shares sum to its difference.
func TestComprehensiveTotals(t *testing.T) {
	eighty := "80.0000"
	sub := uuid.New()
	rs, err := reservesOf([]Member{{LegalEntityID: sub, CompanyCode: "1000", Role: "member", OwnershipPercent: &eighty}},
		map[uuid.UUID]money.Minor{sub: -3003}, map[uuid.UUID]money.Minor{sub: -14010})
	if err != nil {
		t.Fatalf("got %v, want no error", err)
	}
	s := statement{group: "EUR", exp: 2, oci: comprehensive{reserves: rs, eliminations: 600, names: map[uuid.UUID]string{sub: "Acme Corp US"}}}
	var out gen.ConsolidatedStatementTotals
	// A profit of 500.00 before eliminations of 20.00, 96.00 of it the
	// interest's.
	s.comprehensiveTotals(&out, columns{reported: -50000, eliminations: 2000}, columns{eliminations: -9600})
	show := func(f gen.OptConsolidatedFigures) string {
		return f.Value.Reported.Amount + " / " + f.Value.Eliminations.Amount + " / " + f.Value.Consolidated.Amount + " " + f.Value.Consolidated.Currency
	}
	for _, c := range []struct {
		name      string
		got, want string
	}{
		{"other comprehensive income", show(out.OtherComprehensiveIncome), "-110.07 / 6.00 / -104.07 EUR"},
		{"total comprehensive income", show(out.TotalComprehensiveIncome), "-610.07 / 26.00 / -584.07 EUR"},
		{"the interest's", show(out.ComprehensiveIncomeNonControllingInterests), "0.00 / -118.01 / -118.01 EUR"},
		{"the owners'", show(out.ComprehensiveIncomeOwnersOfTheParent), "-610.07 / 144.01 / -466.06 EUR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %s, want %s", c.got, c.want)
			}
		})
	}
	api := s.reservesAPI()[0]
	if got, w := fmt.Sprint(api.CompanyCode, " ", api.CompanyName, " ", api.Role, " ", api.OwnershipPercent.Value, ": ", api.Opening.Amount, " + ",
		api.Difference.Amount, " = ", api.Closing.Amount, ", ", api.NonControllingInterests.Amount, " and ", api.OwnersOfTheParent.Amount, " ",
		api.OwnersOfTheParent.Currency), "1000 Acme Corp US member 80: -30.03 + -110.07 = -140.10, -22.01 and -88.06 EUR"; got != w {
		t.Errorf("the reserve: got %s, want %s", got, w)
	}
}
