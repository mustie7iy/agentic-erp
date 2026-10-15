package money

import "testing"

func TestMoneyEdges(t *testing.T) {
	if v, err := ParseDecimal(".5", 2); err != nil || v != 50 {
		t.Fatalf(".5 -> %d %v", v, err)
	}
	if Minor(-1234).Format(0) != "-1,234" {
		t.Fatalf("zero decimals: %s", Minor(-1234).Format(0))
	}
	if got, err := Percent(1_000, 8); err != nil || got != 80 {
		t.Errorf("Percent(1000, 8) = %d, %v, want 80", got, err)
	}
	if got, err := Percent(-125, 10); err != nil || got != -13 {
		t.Errorf("Percent(-125, 10) = %d, %v, want -13", got, err)
	}
	if got, err := Percent(125, -10); err != nil || got != -13 {
		t.Errorf("Percent(125, -10) = %d, %v, want -13", got, err)
	}
	if got := Minor(-5).Abs(); got != 5 {
		t.Errorf("Minor(-5).Abs() = %d, want 5", got)
	}
	if got := Minor(5).Abs(); got != 5 {
		t.Errorf("Minor(5).Abs() = %d, want 5", got)
	}
	for in, want := range map[string]Minor{"1080.00": 108000, "-0.05": -5, "0.00": 0} {
		t.Run("parse "+in, func(t *testing.T) {
			if got, err := ParseAmount(in, 2); err != nil || got != want {
				t.Errorf("ParseAmount(%q) = %d, %v; want %d, nil", in, got, err, want)
			}
		})
	}
	if v, err := ParseAmount("100", 0); err != nil || v != 100 {
		t.Errorf("JPY: %d %v", v, err)
	}
	for _, in := range []string{"1080.0", "1080", "1080.000", "-", "-.50", "1.0a", ""} {
		t.Run("reject "+in, func(t *testing.T) {
			if _, err := ParseAmount(in, 2); err == nil {
				t.Errorf("ParseAmount(%q, 2) accepted, want an error", in)
			}
		})
	}
	if _, err := ParseAmount("100.0", 0); err == nil {
		t.Error("decimals on a zero-digit currency accepted")
	}
	for _, c := range []struct {
		m    Minor
		dec  int
		want string
	}{{-123456789, 2, "-1234567.89"}, {1000, 0, "1000"}, {5, 3, "0.005"}} {
		if got := c.m.Plain(c.dec); got != c.want {
			t.Errorf("Minor(%d).Plain(%d) = %q, want %q", c.m, c.dec, got, c.want)
		}
	}
}
