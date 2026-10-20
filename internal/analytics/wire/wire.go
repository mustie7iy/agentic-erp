// Package wire is the contract between the semantic layer in erp-server
// and the analytical store's reader in erp-analytics (ADR-0044): the query
// plan, the principal's scope and the pre-aggregated rows. It is plain
// data with no cgo, so erp-server speaks it without linking DuckDB
// (ADR-0032, amended).
package wire

import (
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// QueryPath is where erp-server posts a Request.
const QueryPath = "/v1/query"

// Problem codes of the reader's refusals. Both are answered from the
// database: refused means the reader will not read what the request names,
// busy that it is at its cap of concurrent queries (retryable, 503).
const (
	RefusedCode = "analytics.refused"
	BusyCode    = "analytics.busy"
)

// Request is one query from erp-server: the tenant, the manifest its
// pointer names (read under the tenant's RLS), what the principal may see
// and the plan.
type Request struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	ManifestKey string    `json:"manifest_key"`
	Scope       Scope     `json:"scope"`
	Plan        Plan      `json:"plan"`
}

// Scope is what the principal may read: every company or the ones listed,
// and the fact datasets of the fact types it may see.
type Scope struct {
	AllCompanies bool     `json:"all_companies,omitempty"`
	Companies    []string `json:"companies,omitempty"`
	Datasets     []string `json:"datasets"`
}

// Plan is a semantic query plan: the semantic layer generates it from a
// metric, and the reader turns it into SQL over the contract's columns
// alone, so a request never carries SQL and cannot name a column the
// contract does not list.
type Plan struct {
	Dataset   string   `json:"dataset"`   // a fact dataset
	Aggregate string   `json:"aggregate"` // sum (of the amount, per currency) | count_distinct (of the document id)
	By        []string `json:"by,omitempty"`
	Where     []Filter `json:"where,omitempty"`
	// Ledger is the one ledger a dataset keyed by ledger is read in: the
	// tenant's leading ledger unless the question named another. A plan
	// over such a dataset without one is refused, so a sum never covers
	// every ledger by default (ADR-0044).
	Ledger string `json:"ledger,omitempty"`
	// FiscalYearStartMonth is the semantic layer's calendar (1-12): the
	// fiscal year, quarter and month are derived from the dataset's date
	// with it at read time.
	FiscalYearStartMonth int `json:"fiscal_year_start_month,omitempty"`
}

// Filter restricts a column: in and not_in on any column the plan may
// name, >=, <= and between on the dataset's date (ISO dates) and on the
// fiscal year, quarter and month (integers).
type Filter struct {
	Column string   `json:"column"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// Result is a plan's rows and the database state they are as of.
type Result struct {
	ManifestKey string    `json:"manifest_key"`
	AsOf        time.Time `json:"as_of"`
	// DataThrough is the latest date of the fact dataset in the scope
	// ("" when it has no rows).
	DataThrough string `json:"data_through,omitempty"`
	Rows        []Row  `json:"rows"`
}

// Row is one group: the plan's By values in its order, then the sum per
// currency or the count.
type Row struct {
	By       []string    `json:"by"`
	Currency string      `json:"currency,omitempty"`
	Exponent int         `json:"exponent"` // 0 is a currency's exponent too (JPY)
	Amount   money.Minor `json:"amount_minor"`
	Count    int64       `json:"count"`
}
