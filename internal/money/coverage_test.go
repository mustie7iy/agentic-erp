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
	for in, want := range map[string]Minor{"1080.00": 108000, "-0.05": -5, "0.00": 0} {
		if v, err := ParseAmount(in, 2); err != nil || v != want {
			t.Errorf("ParseAmount(%q) = %d, %v", in, v, err)
		}
	}
	if v, err := ParseAmount("100", 0); err != nil || v != 100 {
		t.Errorf("JPY: %d %v", v, err)
	}
	for _, in := range []string{"1080.0", "1080", "1080.000", "-", "-.50", "1.0a", ""} {
		if _, err := ParseAmount(in, 2); err == nil {
			t.Errorf("ParseAmount(%q, 2) accepted", in)
		}
	}
	if _, err := ParseAmount("100.0", 0); err == nil {
		t.Error("decimals on a zero-digit currency accepted")
	}
	if Minor(-123456789).Plain(2) != "-1234567.89" || Minor(1000).Plain(0) != "1000" || Minor(5).Plain(3) != "0.005" {
		t.Errorf("plain: %s %s %s", Minor(-123456789).Plain(2), Minor(1000).Plain(0), Minor(5).Plain(3))
	}
}
