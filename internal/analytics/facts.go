package analytics

import (
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Fact is one row of a fact table. Amount is in minor units of Currency.
// A journal line names its Ledger and whether it is the Leading one; a
// fact of a table without ledgers names none. A group the store
// aggregated carries its document Count in place of an ID.
type Fact struct {
	ID       string
	Dims     map[string]string
	Amount   money.Minor
	Currency string
	Ledger   string
	Leading  bool
	Count    int64
}

// The sources an answer comes from, named in its lineage (ADR-0044).
const (
	SourceStore    = "store"
	SourceDatabase = "database"
)

// Facts is the analytical store: denormalized fact tables built from what
// the API returns (Source), so queries never touch transactional
// structures. A columnar engine over read models replaces this in-memory
// store at scale.
type Facts struct {
	Tables    map[string][]Fact
	Companies map[string]string // company code -> currency
	// Exponents holds each fact currency's minor-unit digits, from
	// ref.currency (ADR-0005): Source reads them off the API's amounts,
	// which carry exactly their currency's digits. A money measure is
	// rendered with them; a currency missing here refuses the query rather
	// than being guessed.
	Exponents map[string]int
	// Aggregated holds, per base metric, groups the analytical store
	// already filtered and aggregated for one query; a base metric found
	// here is not computed from Tables.
	Aggregated map[string][]Fact
	Source     string // SourceStore or SourceDatabase
	AsOf       time.Time
	// DataThrough is the latest posting or document date in the data.
	DataThrough string
}

// fiscal maps an ISO date to fiscal year and period. With a start month
// other than January, the fiscal year is named after the calendar year in
// which it ends.
func fiscal(date string, startMonth int) (year, month int, err error) {
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid date %q", date)
	}
	m := int(t.Month())
	year = t.Year()
	month = (m-startMonth+12)%12 + 1
	if startMonth > 1 && m >= startMonth {
		year++
	}
	return year, month, nil
}
