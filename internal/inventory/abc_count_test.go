package inventory_test

import (
	"fmt"
	"testing"
)

// TestABCWithAnOpenCount (batch 97S integration: S2c3 with S2d2 and S2c2):
// an indicator is the material's at the plant and a count document is a
// count of its stock, and neither waits for the other. An ABC analysis
// applied while the bracket's count document is counted moves the
// bracket's indicator and leaves the document as it is; the indicator
// taken off then, the document is still sent back for a recount, counted
// again and posted at the latest count.
func TestABCWithAnOpenCount(t *testing.T) {
	s := abcSetup(t, valued)
	s.a, s.b, s.c = s.indicator(t, s.p1, "A", 90), s.indicator(t, s.p1, "B", 180), s.indicator(t, s.p1, "C", 365)
	s.assign(t, s.bracket, s.c)
	doc := s.counted(t, []string{s.bracket}, "64")
	preview, _ := s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	applied, _ := s.Must(201, "POST", abcApplications, s.applyBody(t, preview["preview_token"]), "")
	first := rows(t, applied, "changes")[0]
	if got, want := fmt.Sprint(first["material_id"] == s.bracket, " ", first["before_indicator_code"], ">", first["indicator_code"]), "true C>A"; got != want {
		t.Errorf("the bracket's change: got %q, want %q", got, want)
	}
	counted, _ := s.Must(200, "GET", counts+"/"+doc, "", "")
	if got, want := fmt.Sprint(counted["status"], " ", counted["version"]), "counted 2"; got != want {
		t.Errorf("the count document after the apply: got %q, want %q, as it was", got, want)
	}
	indicator := first["cycle_count_material_id"].(string)
	_, tag := s.Must(200, "GET", cycleMaterials+"/"+indicator, "", "")
	s.Must(200, "POST", cycleMaterials+"/"+indicator+":remove", "", tag)
	if got := s.recount(t, doc, 200, 1)["status"]; got != "counting" {
		t.Errorf("recounted once the indicator is off: got %v, want counting", got)
	}
	s.enter(t, doc, 200, [3]string{"1", "63", "EA"})
	s.postDiffs(t, doc, 200)
	if got := s.stock(t, s.s1, ""); got != "63 EA" {
		t.Errorf("the bracket's stock after posting: got %s, want 63 EA, the recount", got)
	}
}
