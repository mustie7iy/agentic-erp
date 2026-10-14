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

// Rows is the part of a transaction Load needs.
type Rows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Load reads every currency's exponent from ref.currency, for checking a
// document whose currencies are not known in advance (a configuration).
func Load(ctx context.Context, q Rows) (Exponents, error) {
	rows, _ := q.Query(ctx, `SELECT code, exponent FROM ref.currency`) // a failed query reports through the rows, read below
	out := Exponents{}
	var code string
	var exp int
	if _, err := pgx.ForEachRow(rows, []any{&code, &exp}, func() error { out[code] = exp; return nil }); err != nil {
		return nil, fmt.Errorf("currency: read exponents: %w", err)
	}
	return out, nil
}

var (
	seedInsertRe = regexp.MustCompile(`(?s)INSERT INTO ref\.currency \(code, name, exponent\) VALUES(.*?);`)
	seedRowRe    = regexp.MustCompile(`\('([A-Z]{3})', '(?:[^']|'')*', ([0-9]+)\)`)
)

// seeded parses the ref.currency rows the migrations insert, in migration
// order, so a later row for a code replaces an earlier one.
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
