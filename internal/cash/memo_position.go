package cash

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// memoFlows adds the active memo records on the banks to their flows
// (source memo, the record's certainty): each occurrence up to the last
// day that counts at the read, on its day at the bank, an occurrence a
// series' override replaces left to the override.
func memoFlows(ctx context.Context, tx pgx.Tx, r reader, legalEntity uuid.UUID, banks []Bank, last time.Time, out map[uuid.UUID][]flow) error {
	ids := make([]uuid.UUID, len(banks))
	for i, b := range banks {
		ids[i] = b.ID
	}
	rows, _ := tx.Query(ctx, memoSelect+` WHERE m.legal_entity_id = $1 AND m.house_bank_id = ANY($2) AND m.status = 'active'
		AND (m.counts_until IS NULL OR m.counts_until >= $3) AND m.value_date <= $4 ORDER BY m.value_date, m.id`, legalEntity, ids, r.read, last)
	ms, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MemoRecord, error) { return scanMemo(row) })
	if err != nil {
		return fmt.Errorf("cash: memo records of %s: %w", legalEntity, err)
	}
	overridden, err := overriddenOccurrences(ctx, tx, ms)
	if err != nil {
		return err
	}
	for _, m := range ms {
		for _, d := range m.days(last) {
			if overridden[occurrence{m.ID, d}] || d.AddDate(0, 0, m.expiryDays()).Before(r.read) {
				continue
			}
			day, late := r.atBank(d)
			out[*m.HouseBankID] = append(out[*m.HouseBankID], flow{date: day, source: SourceMemo, certainty: m.Certainty, late: late, amount: m.Amount})
		}
	}
	return nil
}

// occurrence is a series' occurrence on a day.
type occurrence struct {
	series uuid.UUID
	day    time.Time
}

// overriddenOccurrences are the occurrences of the series among the
// records that an override replaces, whatever the override's status.
func overriddenOccurrences(ctx context.Context, tx pgx.Tx, ms []MemoRecord) (map[occurrence]bool, error) {
	var series []uuid.UUID
	for _, m := range ms {
		if m.Recurrence != nil {
			series = append(series, m.ID)
		}
	}
	out := map[occurrence]bool{}
	if len(series) == 0 {
		return out, nil
	}
	rows, _ := tx.Query(ctx, `SELECT recurrence_of, occurrence_date FROM cash.memo_record WHERE recurrence_of = ANY($1)`, series)
	var o occurrence
	if _, err := pgx.ForEachRow(rows, []any{&o.series, &o.day}, func() error { out[o] = true; return nil }); err != nil {
		return nil, fmt.Errorf("cash: overrides of %d series: %w", len(series), err)
	}
	return out, nil
}

// days are the record's dates up to last: its value date, or a series'
// occurrences.
func (m MemoRecord) days(last time.Time) []time.Time {
	if m.Recurrence == nil {
		return []time.Time{m.ValueDate}
	}
	return m.Recurrence.occurrences(m.ValueDate, last)
}
