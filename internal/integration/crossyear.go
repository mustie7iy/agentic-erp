package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Vendor items SAP cleared in a later fiscal year (ADR-0034 As built,
// decided by the product owner 2026-10-31; AUGDT and AUGGJ confirmed by
// the SAP specialist, po:2026-11-09, A3 [H]). A run's scope reads its
// journal at the fiscal year's end (sap.FilterScope): an item SAP cleared
// by a clearing document of a later fiscal year (AUGGJ) loads open, in
// either mode, and the later year's run, whose scope sets the earlier
// years' documents its clearings clear apart (sap.Extract.ClearedEarlier),
// clears it with the documents it loads. The earlier item is on the
// platform as an entry of its own (its ACDOCA reference: a documents run,
// or a balances run's document that posts on its own) or as a line of a
// balances run's opening entry, which integration.opening_item names
// (addOpeningItems), since the later year's extract cannot tell the
// opening entry's line numbers.

// sapItemKey is an SAP journal line's key: <BUKRS>/<GJAHR>/<BELNR>/<DOCLN>.
func sapItemKey(l sap.ACDOCA) string { return documentKey(l) + "/" + l.DOCLN }

// fiscalYear is the run's fiscal year as SAP writes it (GJAHR).
func (t target) fiscalYear() string { return strconv.Itoa(int(t.year)) }

// earlierYear reports whether a line is of a fiscal year before the run's:
// one of the documents its scope sets apart (sap.Extract.ClearedEarlier).
func (t target) earlierYear(l sap.ACDOCA) bool { return l.GJAHR != t.fiscalYear() }

// clearsThisYear reports whether SAP cleared a line by a clearing document
// of the run's fiscal year.
func (t target) clearsThisYear(l sap.ACDOCA) bool {
	return l.AUGBL != "" && l.AUGGJ == t.fiscalYear()
}

// carriedClearings are the supplier lines of earlier fiscal years SAP
// cleared in the run's fiscal year: the counter-items of its clearings.
func carriedClearings(e *sap.Extract, t target) []sap.ACDOCA {
	var out []sap.ACDOCA
	for _, l := range e.ClearedEarlier {
		if t.clearsThisYear(l) && t.supplierLine(l) {
			out = append(out, l)
		}
	}
	return out
}

// clearingYears refuses a clearing year (AUGGJ) that is no fiscal year,
// which the scope cannot read the line at the year's end by, and, in
// balances mode, a clearing of earlier fiscal years' items: the opening
// entry leaves out the year's cleared items, so nothing it posts could
// clear the items an earlier year's run loaded open.
func (q *qualityCheck) clearingYears() {
	for _, l := range q.e.ACDOCA {
		if l.AUGGJ != "" && l.AUGGJ != "0000" && !sap.IsYear(l.AUGGJ) {
			q.add(SevError, "ACDOCA", sapItemKey(l), "the clearing's fiscal year (AUGGJ) %q is no fiscal year", l.AUGGJ)
		}
	}
	if !q.t.balances {
		return
	}
	items := map[string][]string{}
	for _, l := range carriedClearings(q.e, q.t) {
		items[l.RBUKRS+"/"+l.AUGBL] = append(items[l.RBUKRS+"/"+l.AUGBL], sapItemKey(l))
	}
	for _, key := range sortedKeys(items) {
		q.add(SevError, "ACDOCA", "clearing "+key, "SAP cleared items of an earlier fiscal year (%s) with it in %d, which a balances run cannot "+
			"clear on the platform: load the year in documents mode", strings.Join(items[key], ", "), q.t.year)
	}
}

// addOpeningItems records the SAP key of each open vendor item an opening
// entry holds, its lines from the first after first on, in order.
func (l *loader) addOpeningItems(ctx context.Context, entry ledger.Entry, first int, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	nos := make([]int32, len(keys))
	for i := range keys {
		nos[i] = int32(first + i + 1)
	}
	if _, err := l.tx.Exec(ctx, `INSERT INTO integration.opening_item (tenant_id, system, external_key, journal_entry_id, fiscal_year, line_no,
			migration_run_id, created_by)
		SELECT $1, $2, k, $3, $4, n, $5, $6 FROM unnest($7::text[], $8::int[]) AS x(k, n)`,
		l.p.TenantID, l.system, entry.ID, entry.FiscalYear, l.run.ID, l.p.ID, keys, nos); err != nil {
		return fmt.Errorf("integration: record the open items of opening entry %s: %w", entry.ID, err)
	}
	return nil
}

// openingItems are the open items earlier runs' opening entries hold for
// the SAP lines of keys (sapItemKey), as entries by document key whose
// lines are those items' line numbers.
func (l *loader) openingItems(ctx context.Context, keys []string) (map[string]loadedEntry, error) {
	out := map[string]loadedEntry{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, _ := l.tx.Query(ctx, `SELECT external_key, journal_entry_id, fiscal_year, line_no FROM integration.opening_item
		WHERE system = $1 AND external_key = ANY($2)`, l.system, keys)
	var key string
	var ref ledger.ItemRef
	_, err := pgx.ForEachRow(rows, []any{&key, &ref.EntryID, &ref.FiscalYear, &ref.LineNo}, func() error {
		cut := strings.LastIndex(key, "/")
		doc := key[:cut]
		e, ok := out[doc]
		if !ok {
			e = loadedEntry{ref: ledger.ItemRef{FiscalYear: ref.FiscalYear, EntryID: ref.EntryID}, lines: map[string]int32{}}
		}
		e.lines[key[cut+1:]] = ref.LineNo
		out[doc] = e
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("integration: read the opening items of system %s: %w", l.system, err)
	}
	return out, nil
}

// clearingDate is the day a clearing is recorded on: its clearing
// document's, or the latest document's (clearingItems), but SAP's
// clearing date (AUGDT) for one of earlier fiscal years' items, whose
// documents may all be of those years (a clearing document of no line,
// as SAP posts one when the items net to zero), when the extract carries
// it.
func (l *loader) clearingDate(group []sap.ACDOCA, by *loadedEntry) loadedEntry {
	for _, line := range group {
		if !l.t.earlierYear(line) {
			continue
		}
		if day, err := sapDate(line.AUGDT); err == nil {
			out := *by
			out.date = day
			return out
		}
	}
	return *by
}
