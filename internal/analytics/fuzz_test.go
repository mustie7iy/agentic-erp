package analytics

import "testing"

// Commentary of any shape never panics the figure extractor or the check.
func FuzzNarrative(f *testing.F) {
	for _, s := range []string{"Revenue grew 7.0% to $25.07M.", "8 of 9 months", "1,2,3,,4.5.6%", "€1.5bn and 3 pts", "-−+$"} {
		f.Add(s)
	}
	r := &Result{Columns: []Column{{Name: "y", Kind: "dimension"}, {Name: "v", Kind: "measure", Type: Money}},
		Rows: []Row{{"2025", usd("100.00")}, {"2026", usd("150.00")}}}
	f.Fuzz(func(t *testing.T, text string) {
		for _, fig := range extractFigures(text) {
			if fig.decimals < 0 || fig.scale <= 0 {
				t.Fatalf("bad figure %+v", fig)
			}
		}
		CheckNarrative(text, []*Result{r})
	})
}
