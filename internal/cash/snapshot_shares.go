package cash

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
)

// The forecast as the analytical store holds it (ADR-0044, ADR-0051 §6,
// §11; T53j2): what the snapshots forecast for each value date, at each
// lag. One snapshot per company and date is read, the one the variance
// compares against (snapshotPreferred), and of it the lines at document
// level, the 31 days a snapshot keeps by day: a bucket's sum beyond names
// no day. As with the actuals, the store's cash_snapshots dataset and the
// semantic layer's database path both read them here, through ports the
// analytics context declares, so the house banks' codes and the items'
// names have one mechanism.

// ForecastShare is a snapshot's line at document level: the line by its
// snapshot and number, the snapshot's date, the line's value date and the
// days between them (its lag), its house bank's code ("" for none; a bank
// is never shown by its account here, §15), its currency and that
// currency's exponent, its source and certainty, whether it is a blocked
// supplier's (SourcePayableBlocked, which the forecast's totals leave
// out), its liquidity item's code and today's name ("" for no item) and
// its signed amount: negative is money out.
type ForecastShare struct {
	SnapshotID    uuid.UUID
	LineNo        int32
	SnapshotDate  time.Time
	ValueDate     time.Time
	LagDays       int32
	HouseBankCode string
	Currency      string
	Exponent      int
	Source        string
	Certainty     string
	Blocked       bool
	Item          string
	ItemName      string
	Amount        money.Minor
}

// snapshotPreferred orders a company's snapshots of one date by the one
// the variance compares against first: its daily snapshot, else the last
// taken on demand that day (a tenant whose snapshotter missed a day, or a
// walkthrough before the snapshot time, still compares).
const snapshotPreferred = `s.kind = 'daily' DESC, s.taken_at DESC, s.id DESC`

// forecastSQL is the lines at document level of the legal entity's
// preferred snapshot of each date, by date and line.
const forecastSQL = `SELECT s.id, f.line_no, s.snapshot_date, f.value_date, f.value_date - s.snapshot_date, f.house_bank_id, f.currency, c.exponent,
		f.source, f.certainty, coalesce(f.liquidity_item, ''), f.amount
	FROM (SELECT DISTINCT ON (s.snapshot_date) s.tenant_id, s.id, s.snapshot_date FROM cash.snapshot s WHERE s.legal_entity_id = $1
		ORDER BY s.snapshot_date, ` + snapshotPreferred + `) s
	JOIN cash.snapshot_flow f ON f.tenant_id = s.tenant_id AND f.snapshot_id = s.id AND f.bucket_end IS NULL
	JOIN ref.currency c ON c.code = f.currency
	ORDER BY s.snapshot_date, f.line_no`

// ForecastShares are the forecast's lines of the legal entity, read as
// the caller in a consistent read of their own: the semantic layer's
// database path, whose endpoint has checked cash.forecast.read, as
// listCashSnapshotFlows reads them. A legal entity the caller cannot act
// in is not found.
func (e *Endpoints) ForecastShares(ctx context.Context, legalEntity uuid.UUID) ([]ForecastShare, error) {
	return asCaller(ctx, e, legalEntity, e.forecastShares)
}

// ExtractForecastShares are the forecast's lines of the legal entity, read
// in the context's transaction: the analytical store's extract (ADR-0044).
// A legal entity the directory does not know has none.
func (e *Endpoints) ExtractForecastShares(ctx context.Context, legalEntity uuid.UUID) ([]ForecastShare, error) {
	return asExtract(ctx, e, legalEntity, e.forecastShares)
}

// forecastShares reads, in tx, the lines of the legal entity's preferred
// snapshot of each date at document level, with their banks' codes and
// their items' names as the active configuration names them, as the
// variance names its rows.
func (e *Endpoints) forecastShares(ctx context.Context, tx pgx.Tx, _ Company, legalEntity uuid.UUID) ([]ForecastShare, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("cash: configuration: %w", err)
	}
	var banks []*uuid.UUID // each line's
	var unique []uuid.UUID
	rows, _ := tx.Query(ctx, forecastSQL, legalEntity)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ForecastShare, error) {
		var s ForecastShare
		var bank *uuid.UUID
		err := r.Scan(&s.SnapshotID, &s.LineNo, &s.SnapshotDate, &s.ValueDate, &s.LagDays, &bank, &s.Currency, &s.Exponent, &s.Source, &s.Certainty,
			&s.Item, &s.Amount)
		banks = append(banks, bank)
		if bank != nil && !slices.Contains(unique, *bank) {
			unique = append(unique, *bank)
		}
		return s, errs.Wrapf(err, "scan cash forecast share")
	})
	if err != nil {
		return nil, fmt.Errorf("cash: forecast of %s: %w", legalEntity, err)
	}
	codes, err := idref.Resolve(ctx, e.Payments.HouseBankCodes, unique, houseBankRef)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of the forecast of %s: %w", legalEntity, err)
	}
	names := map[string]string{}
	for _, it := range doc.Cash.LiquidityItems {
		names[it.Code] = it.Name
	}
	for i := range out {
		if banks[i] != nil {
			out[i].HouseBankCode = codes[*banks[i]]
		}
		out[i].Blocked, out[i].ItemName = out[i].Source == SourcePayableBlocked, names[out[i].Item]
	}
	return out, nil
}
