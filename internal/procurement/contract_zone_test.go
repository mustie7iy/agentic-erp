package procurement_test

import (
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestContractNumberTimeZone: a contract's number is drawn from the range
// of the fiscal year its company's date is in, in the company's time zone,
// not UTC's: at 22:00 on 31 December 2026 in Toronto (03:00 UTC on 1
// January 2027) from 2026's range, at 01:00 on 1 January 2027 in Dubai
// (21:00 UTC on 31 December 2026) from 2027's. Company 2000's 2027
// contract range has its own prefix, CU, so the number shows the year; it
// replaces the fixture's own 2027 range, which the fixture has from 1
// January 2027 (fixture.Years). A renewal, drafted by the same path, is
// numbered alike.
func TestContractNumberTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone, number string
		at           time.Time
	}{
		{"America/Toronto", "CT100000", time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC)},
		{"Asia/Dubai", "CU100000", time.Date(2026, 12, 31, 21, 0, 0, 0, time.UTC)},
	} {
		t.Run(c.zone, func(t *testing.T) {
			f := fixture.NewWith(t, nil, func(doc *config.Config) {
				doc.Company("2000").TimeZone = c.zone
				doc.NumberRanges = slices.DeleteFunc(doc.NumberRanges, func(n config.NumberRange) bool {
					return n.Company == "2000" && n.DocumentType == "contract" && n.FiscalYear == 2027 // the fixture's own from 2027 (fixture.Years)
				})
				doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "contract", FiscalYear: 2027, Prefix: "CU",
					First: 100000, Last: 999999})
			}, perms...)
			f.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			e := env{Env: f, office: f.Account(t, "2000", "520000").String()}
			ct := e.contract(t, e.Supplier.String(), "")
			if ct["document_number"] != c.number {
				t.Errorf("contract: got document_number %v, want %s", ct["document_number"], c.number)
			}
			path := "/v1/contracts/" + id(ct)
			e.act(t, path, "release", "")
			r, _ := e.Must(201, "POST", path+":renew", renewalOf(t, e, path), "*")
			if want := c.number[:2] + "100001"; r["document_number"] != want {
				t.Errorf("renewal: got document_number %v, want %s", r["document_number"], want)
			}
		})
	}
}
