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
	occs, err := memoOccurrences(ctx, tx, r, legalEntity, ids, last)
	for _, o := range occs {
		out[*o.record.HouseBankID] = append(out[*o.record.HouseBankID], o.flow)
	}
	return err
}

// memoForecast are the legal entity's active memo records' occurrences in
// the forecast (T53e4), with a house bank or none, each classified by its
// record's own liquidity item and naming the record: a series' up to the
// last day, a one-off record whatever its day, so one after the last
// bucket is beyond.
func memoForecast(ctx context.Context, tx pgx.Tx, r reader, legalEntity uuid.UUID, last time.Time) ([]datedFlow, error) {
	occs, err := memoOccurrences(ctx, tx, r, legalEntity, nil, last)
	out := make([]datedFlow, len(occs))
	for i, o := range occs {
		m := o.record
		out[i] = datedFlow{flow: o.flow, currency: m.Currency, parts: []part{{amount: m.Amount, item: m.Item}},
			documents: []paidDocument{{Document: Document{Kind: DocMemoRecord, ID: m.ID, Number: deref(m.Reference)}, amount: m.Amount}}}
	}
	return out, err
}

// deref is the text, "" for none.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// memoOccurrence is an occurrence of an active memo record as a flow.
type memoOccurrence struct {
	record MemoRecord
	flow   flow
}

// memoOccurrences are the occurrences of the legal entity's active memo
// records that count at the read: on the banks, those dated up to the last
// day; with no banks (nil), every record, with a bank or none, a one-off
// whatever its day. Each occurrence of a series up to the last day is on
// its day at the bank, an occurrence a series' override replaces left to
// the override, an expired one left out.
func memoOccurrences(ctx context.Context, tx pgx.Tx, r reader, legalEntity uuid.UUID, banks []uuid.UUID, last time.Time) ([]memoOccurrence, error) {
	every := banks == nil
	rows, _ := tx.Query(ctx, memoSelect+` WHERE m.legal_entity_id = $1 AND ($2 OR m.house_bank_id = ANY($3)) AND m.status = 'active'
		AND (m.counts_until IS NULL OR m.counts_until >= $4) AND ($2 OR m.value_date <= $5) ORDER BY m.value_date, m.id`, legalEntity, every, banks, r.read, last)
	ms, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MemoRecord, error) { return scanMemo(row) })
	if err != nil {
		return nil, fmt.Errorf("cash: memo records of %s: %w", legalEntity, err)
	}
	overridden, err := overriddenOccurrences(ctx, tx, ms)
	if err != nil {
		return nil, err
	}
	var out []memoOccurrence
	for _, m := range ms {
		for _, d := range m.days(last) {
			if overridden[occurrence{m.ID, d}] || d.AddDate(0, 0, m.expiryDays()).Before(r.read) {
				continue
			}
			day, late := r.atBank(d)
			out = append(out, memoOccurrence{record: m, flow: flow{date: day, source: SourceMemo, certainty: m.Certainty, late: late, amount: m.Amount}})
		}
	}
	return out, nil
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
