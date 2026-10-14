package currency

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
)

// Exponents maps currency codes to their minor-unit digits, as
// ref.currency holds them.
type Exponents map[string]int

// Rows is the part of a transaction Load and LoadCodes need.
type Rows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Load reads every currency's exponent from ref.currency, for checking a
// document whose currencies are not known in advance (a configuration).
func Load(ctx context.Context, q Rows) (Exponents, error) {
	return read(ctx, q, "", `SELECT code, exponent FROM ref.currency`)
}

// LoadCodes reads the exponents of the named currencies from ref.currency.
// A code ref.currency does not hold is absent from the result, for the
// caller to refuse in its own terms.
func LoadCodes(ctx context.Context, q Rows, codes ...string) (Exponents, error) {
	return read(ctx, q, fmt.Sprintf(" of %v", codes), `SELECT code, exponent FROM ref.currency WHERE code = ANY($1)`, codes)
}

// read collects the code and exponent rows a query returns; which names
// the currencies read in a failure.
func read(ctx context.Context, q Rows, which, sql string, args ...any) (Exponents, error) {
	rows, _ := q.Query(ctx, sql, args...) // a failed query reports through the rows, read below
	out := Exponents{}
	var code string
	var exp int
	if _, err := pgx.ForEachRow(rows, []any{&code, &exp}, func() error { out[code] = exp; return nil }); err != nil {
		return nil, fmt.Errorf("currency: read exponents%s: %w", which, err)
	}
	return out, nil
}

var (
	seedInsertRe = regexp.MustCompile(`(?s)INSERT INTO ref\.currency \(code, name, exponent\) VALUES(.*?);`)
	seedRowRe    = regexp.MustCompile(`\('([A-Z]{3})', '(?:[^']|'')*', ([0-9]+)\)`)
)

// seeded parses the ref.currency rows the migrations insert, in migration
// order, so a later row for a code replaces an earlier one. It reads only
// `INSERT INTO ref.currency (code, name, exponent) VALUES (...), ...;`
// with no ';' inside a name; an UPDATE or DELETE of ref.currency, or
// another form, is not read, and TestSeededMatchesTheDatabase fails until
// this parser learns it.
var seeded = sync.OnceValue(func() Exponents {
	out := Exponents{}
	for _, m := range db.Migrations() {
		for _, insert := range seedInsertRe.FindAllStringSubmatch(m.Up, -1) {
			for _, row := range seedRowRe.FindAllStringSubmatch(insert[1], -1) {
				out[row[1]], _ = strconv.Atoi(row[2]) // the pattern admits only digits
			}
		}
	}
	return out
})

// Seeded returns the exponents the migrations seed into ref.currency, read
// from the embedded migration files: the reference data where there is no
// database (the CLI's offline `erp config validate`), without a second
// copy of the table to drift from it. A test holds it equal to ref.currency
// in a migrated database, so a migration that changes the seed in a form
// this does not read fails there.
func Seeded() Exponents { return maps.Clone(seeded()) }
