package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// RFQs ended without an award (T82g, migration 000369): a draft or
// published RFQ is cancelled, a published one closed, each with a reason
// and only once its quotations are all not awarded; it stays so, and an
// RFQ cancelled or closed is not its requisition's open RFQ.

const endSQL = `UPDATE procurement.rfq SET status = $2, closing_reason = $3, version = version + 1 WHERE id = $1`

// ending settles the quotations as not awarded, then ends the RFQ, as the
// application does.
func ending(rfq any, status string, reason any, quotations ...any) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		for _, q := range quotations {
			if err := exec(tx, outcomeSQL, q, "not_awarded"); err != nil {
				return err
			}
		}
		return exec(tx, endSQL, rfq, status, reason)
	}
}

// asOwner runs do as the table owner, past the application's grants: a
// trigger's rule holds for it too.
func asOwner(do func(tx pgx.Tx) error) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := exec(tx, `SET LOCAL ROLE erp_owner`); err != nil {
			return err
		}
		return do(tx)
	}
}

// TestRFQEndRules: what the database refuses of an RFQ's end, as the
// application role and, for the trigger's rules, as the owner.
func TestRFQEndRules(t *testing.T) {
	need(t)
	f := newAwards(t)
	for name, c := range map[string]struct {
		do      func(tx pgx.Tx) error
		want    string
		trigger bool
	}{
		"a draft closed":            {ending(f.draft, "closed", "R"), "only a published RFQ is closed", true},
		"a quotation left recorded": {ending(f.published, "cancelled", "R"), "each of its quotations is not awarded first", true},
		"one of two left recorded":  {ending(f.other, "closed", "R", f.second), "each of its quotations is not awarded first", true},
		"a quotation awarded": {func(tx pgx.Tx) error { // its outcome is set once, so it is not settled again
			if err := exec(tx, outcomeSQL, f.second, "awarded"); err != nil {
				return err
			}
			return ending(f.other, "closed", "R", f.third)(tx)
		}, "each of its quotations is not awarded first", true},
		"a cancel without a reason":     {ending(f.draft, "cancelled", nil), "rfq_closing_reason_check", false},
		"a blank reason":                {ending(f.draft, "cancelled", " "), "rfq_closing_reason_check", false},
		"a reason too long":             {ending(f.draft, "cancelled", strings.Repeat("r", 501)), "rfq_closing_reason_check", false},
		"a reason without an end":       {ending(f.draft, "draft", "R"), "rfq_closing_reason_check", false},
		"an unknown status":             {ending(f.draft, "withdrawn", nil), "rfq_status_check", false},
		"a published RFQ back to draft": {ending(f.other, "draft", nil, f.second, f.third), "stays published", true},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, c.do), c.want)
			if c.trigger {
				failsWith(t, asApp(t, f.tenant, asOwner(c.do)), c.want)
			}
		})
	}
}

// TestRFQEnded: the ends the database accepts, and what it keeps of them.
func TestRFQEnded(t *testing.T) {
	need(t)
	f := newAwards(t)
	mustApp(t, f.tenant, ending(f.draft, "cancelled", strings.Repeat("r", 500)))
	mustApp(t, f.tenant, ending(f.other, "closed", "Above budget", f.second, f.third))
	mustApp(t, f.tenant, ending(f.published, "cancelled", "Not needed", f.quotation))
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	const stays = "with its reason"
	for name, do := range map[string]func(tx pgx.Tx) error{
		"a cancelled RFQ republished": one(`UPDATE procurement.rfq SET status = 'published', closing_reason = NULL, version = version + 1 WHERE id = $1`, f.draft),
		"a closed RFQ cancelled":      one(`UPDATE procurement.rfq SET status = 'cancelled', version = version + 1 WHERE id = $1`, f.other),
		"the reason changed":          one(`UPDATE procurement.rfq SET closing_reason = 'Other', version = version + 1 WHERE id = $1`, f.other),
		"a closed RFQ awarded":        one(`UPDATE procurement.rfq SET status = 'awarded', contract_id = $2, closing_reason = NULL, version = version + 1 WHERE id = $1`, f.other, f.contract),
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, do), stays)
			failsWith(t, asApp(t, f.tenant, asOwner(do)), stays)
		})
	}
	t.Run("a cancelled RFQ is no requisition's open RFQ", func(t *testing.T) {
		s := newSourcing(t)
		second := id()
		open := func(tx pgx.Tx) error {
			return exec(tx, sourcingSQL, s.tenant, second, "RQ9", s.entity, "EUR", s.inSourcing, s.actor)
		}
		failsWith(t, asApp(t, s.tenant, open), "sourced by one open RFQ at a time")
		mustApp(t, s.tenant, ending(s.sourcing, "cancelled", "Raised twice"))
		mustApp(t, s.tenant, open)
	})
}

// TestRFQEndRevert (000369's down): the revert is refused while an RFQ is
// cancelled or closed, which the previous version cannot show.
func TestRFQEndRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000369_rfq_cancel_close").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // the test only reads the refusal; nothing is kept
	tenant, rfq := id(), id()
	if err := all(tx, step(rfqSQL, tenant, rfq, "RQ1", tenant, "EUR", "draft", tenant), step(endSQL, rfq, "cancelled", "Not needed")); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "000369 cannot be reverted")
}
