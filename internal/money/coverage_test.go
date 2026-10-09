package money

import "testing"

func TestMoneyEdges(t *testing.T) {
	if v, err := ParseDecimal(".5", 2); err != nil || v != 50 {
		t.Fatalf(".5 -> %d %v", v, err)
	}
	if Minor(-1234).Format(0) != "-1,234" {
		t.Fatalf("zero decimals: %s", Minor(-1234).Format(0))
	}
	if Qty(-12500).String() != "-12.5" || Qty(3000).String() != "3" {
		t.Fatal("qty strings")
	}
	if Extend(2_000, 150, 0) != 300 || Extend(2_000, 150, -5) != 300 {
		t.Fatal("price unit defaults to 1")
	}
	if Percent(1_000, 8) != 80 || Percent(-125, 10) != -13 {
		t.Fatal("percent rounding")
	}
	if divRound(7, -2) != -4 || divRound(-7, -2) != 4 {
		t.Fatal("negative denominator")
	}
	if Minor(-5).Abs() != 5 || Minor(5).Abs() != 5 {
		t.Fatal("abs")
	}
}
