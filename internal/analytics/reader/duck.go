package reader

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math/big"
	"strings"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/analytics/datasets"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// duckTypes are the contract's types in DuckDB.
var duckTypes = map[string]string{
	"uuid": "UUID", "string": "VARCHAR", "int32": "INTEGER", "int64": "BIGINT",
	"bool": "BOOLEAN", "date": "DATE", "timestamp": "TIMESTAMP", "xid8": "UBIGINT",
}

// connect opens a query's database: in memory, and installing or loading
// no extension on demand, since the reader reads local Parquet alone and
// httpfs would open a network path. A variable, so a test can fail it.
var connect = func() (driver.Connector, error) {
	return duckdb.NewConnector("?autoinstall_known_extensions=false&autoload_known_extensions=false", nil)
}

// run answers a compiled plan in a database of its own, opened for this
// query alone and closed after it, so nothing one query registered is seen
// by the next.
func (r *Reader) run(ctx context.Context, req Request, c compiled, files map[string][]string) ([]Row, error) {
	db, cx, err := r.open(ctx, req, c, files)
	if err != nil {
		return nil, err
	}
	defer db.Close() //nolint:errcheck // an in-memory database: closing it frees memory and loses nothing
	defer cx.Close() //nolint:errcheck // as above
	rows, err := cx.QueryContext(ctx, c.sql, c.args...)
	if err != nil {
		return nil, fmt.Errorf("reader: query: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err reports a failed read
	return r.scan(rows, len(req.Plan.By), c.sum)
}

// open is a confined database with the plan's views (setup).
func (r *Reader) open(ctx context.Context, req Request, c compiled, files map[string][]string) (*sql.DB, *sql.Conn, error) {
	conn, err := connect()
	if err != nil {
		return nil, nil, fmt.Errorf("reader: duckdb: %w", err)
	}
	db := sql.OpenDB(conn)
	cx, err := db.Conn(ctx)
	if err == nil {
		for _, step := range r.setup(req, c, files) {
			if _, err = cx.ExecContext(ctx, step.sql, step.args...); err != nil {
				err = fmt.Errorf("setup: %w", err)
				break
			}
		}
	}
	if err != nil {
		// db.Close closes idle connections alone: the one held is closed
		// first, or its in-memory database would outlive the query.
		if cx != nil {
			_ = cx.Close() // an in-memory database: closing it frees memory and loses nothing
		}
		_ = db.Close() // as above
		return nil, nil, fmt.Errorf("reader: duckdb: %w", err)
	}
	return db, cx, nil
}

type stmt struct {
	sql  string
	args []any
}

// setup is the statements that confine a query's database: the scope's
// tenant and companies as tables, file access limited to the files the
// plan reads, external access off and the configuration locked, then one
// view per dataset over its files.
func (r *Reader) setup(req Request, c compiled, files map[string][]string) []stmt {
	steps := []stmt{
		{"CREATE TEMP TABLE scope_tenant (tenant_id UUID)", nil},
		{"INSERT INTO scope_tenant VALUES (CAST($1 AS UUID))", []any{req.TenantID.String()}},
		{"CREATE TEMP TABLE scope_company (company VARCHAR)", nil},
	}
	for _, co := range req.Scope.Companies {
		steps = append(steps, stmt{"INSERT INTO scope_company VALUES ($1)", []any{co}})
	}
	var all []string
	for _, name := range c.datasets {
		all = append(all, files[name]...)
	}
	steps = append(steps,
		stmt{"SET allowed_paths = " + list(all), nil},
		stmt{"SET enable_external_access = false", nil},
		stmt{"SET lock_configuration = true", nil})
	for _, name := range c.datasets {
		steps = append(steps, stmt{view(r.contract[name], files[name], !req.Scope.AllCompanies), nil})
	}
	return steps
}

// view selects a dataset's contract columns from its files, the tenant's
// rows alone and, for a dataset by company, the scope's companies alone. A
// dataset without files is an empty relation of the same columns.
func view(d *datasets.Dataset, files []string, byCompany bool) string {
	cols := make([]string, len(d.Columns))
	for i, c := range d.Columns {
		name := pgx.Identifier{c.Name}.Sanitize()
		cols[i] = name
		if len(files) == 0 {
			cols[i] = "CAST(NULL AS " + duckTypes[c.Type] + ") AS " + name
		}
	}
	q := "CREATE TEMP VIEW " + pgx.Identifier{d.Dataset}.Sanitize() + " AS SELECT " + strings.Join(cols, ", ")
	if len(files) == 0 {
		return q + " WHERE false"
	}
	// No columns from the path: the cache directory's segments must never
	// stand in for the file's tenant_id.
	q += " FROM read_parquet(" + list(files) + ", hive_partitioning = false) WHERE tenant_id = (SELECT tenant_id FROM scope_tenant)"
	if byCompany && d.Column("company") != nil {
		q += " AND company IN (SELECT company FROM scope_company)"
	}
	return q
}

// list is a DuckDB list of string literals. Its strings are the reader's
// own cache paths (the configured directory and a SHA-256 the guard
// checked); a view and a SET take no parameters, so they are quoted here.
func list(paths []string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = "'" + strings.ReplaceAll(p, "'", "''") + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// scanner is the part of sql.Rows scan reads.
type scanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// scan reads the groups, refusing an amount whose exponent is not its
// currency's or whose sum leaves int64.
func (r *Reader) scan(rows scanner, by int, sum bool) ([]Row, error) {
	out := []Row{}
	for rows.Next() {
		row := Row{By: make([]string, by)}
		dest := make([]any, 0, by+3)
		for i := range row.By {
			dest = append(dest, &row.By[i])
		}
		var total *big.Int
		if sum {
			dest = append(dest, &row.Currency, &row.Exponent, &total)
		} else {
			dest = append(dest, &row.Count)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("reader: scan: %w", err)
		}
		if sum {
			if err := r.amount(&row, total); err != nil {
				return nil, err
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reader: rows: %w", err)
	}
	return out, nil
}

// amount sets a group's sum, checked against its currency.
func (r *Reader) amount(row *Row, total *big.Int) error {
	if e, ok := r.Exponents[row.Currency]; !ok || e != row.Exponent {
		return refused("currency %q with exponent %d", row.Currency, row.Exponent)
	}
	if total == nil || !total.IsInt64() {
		return refused("the sum in %s is outside int64", row.Currency)
	}
	row.Amount = money.Minor(total.Int64())
	return nil
}
