package cabank

import "testing"

// FuzzParse: an account Parse accepts is digits of the scheme's lengths,
// parses to itself again, and gives a valid member id.
func FuzzParse(f *testing.F) {
	f.Add("003", "12345", "1234567")
	f.Add("0 03", "12-345", "12 3456-7890 12")
	f.Add("", "", "")
	f.Add("003", "12345", "DE89370400440532013000")
	f.Fuzz(func(t *testing.T, institution, transit, number string) {
		a, err := Parse(institution, transit, number)
		if err != nil {
			return
		}
		if !ValidRouting(a.Institution, a.Transit) || !ValidNumber(a.Number) {
			t.Fatalf("Parse(%q, %q, %q) = %+v: not the stored form", institution, transit, number, a)
		}
		if again, err := Parse(a.Institution, a.Transit, a.Number); err != nil || again != a {
			t.Fatalf("Parse of %+v: got %+v, %v", a, again, err)
		}
		if id := MemberID(a.Institution, a.Transit); !ValidMemberID(id) {
			t.Fatalf("member id %q of %+v is invalid", id, a)
		}
	})
}
