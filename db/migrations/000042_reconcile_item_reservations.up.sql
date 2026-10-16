-- Contract step of the open-item reservations (F5b, 03-data-model §3.10,
-- 06-engineering-process §6.4): the ledger's reservations become exactly
-- the items live payment runs hold and have not paid. During 000034's
-- rollout a release from before it could reject a run without freeing its
-- reservations, or propose one without reserving its items; once no replica
-- of that release runs, this reconciles what it left.
--
-- A live item is an active payment item of a proposing or proposed run, or
-- of a releasing run whose payment is not posted yet: the rule
-- payments.Reservations.RunOf applies and 000034 backfilled with.
--
-- The statements below each read afresh (read committed), and replicas may
-- still propose, reject and post while this runs, so the tables are locked
-- SHARE first: other writers wait until it commits, and every statement
-- sees the same runs and reservations. A lock not had within the timeout
-- fails the migration, which the next start retries, rather than queue
-- every writer behind it. A release posting under way can hold a
-- reservation it settles and wait for its payment row, a lock cycle with
-- this one; PostgreSQL aborts one of the two (deadlock_detected), and the
-- next start retries the migration, or the workflow retries the posting.
-- Row-level security is off for the transaction,
-- so the reconciliation sees every tenant's rows or, run by a role that
-- does not bypass it, fails instead of reconciling nothing.
SET LOCAL lock_timeout = '5s';
SET LOCAL row_security = off;
LOCK TABLE payments.payment_run, payments.payment, payments.payment_item, ledger.item_reservation IN SHARE MODE;

CREATE TEMPORARY TABLE live_item ON COMMIT DROP AS
SELECT i.tenant_id, r.id AS run_id, r.document_number, r.proposed_by, i.item_fiscal_year, i.item_entry_id, i.item_line_no
FROM payments.payment_item i
JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
WHERE i.is_active AND (r.status IN ('proposing', 'proposed') OR (r.status = 'releasing' AND p.journal_entry_id IS NULL));

-- A reservation no live item matches is released in the name of whoever
-- last decided or changed its run (an old release rejected it, or its
-- payment posted), or of its creator when the run is gone.
UPDATE ledger.item_reservation x
SET released_at = now(), released_by = coalesce(
  (SELECT coalesce(r.decided_by, r.updated_by) FROM payments.payment_run r WHERE r.tenant_id = x.tenant_id AND r.id = x.owner_id), x.created_by)
WHERE x.released_at IS NULL
  AND NOT EXISTS (SELECT FROM live_item l WHERE l.tenant_id = x.tenant_id AND l.run_id = x.owner_id
    AND l.item_fiscal_year = x.fiscal_year AND l.item_entry_id = x.journal_entry_id AND l.item_line_no = x.line_no);

-- A live item whose run already released its reservation cannot be
-- reserved for that run again: a reservation is released once
-- (item_reservation_released_once) and keyed by owner and item. Stop rather
-- than leave a live item unreserved; such an item is resolved by hand
-- (the run rejected and proposed again) before this migration is retried.
DO $$
DECLARE n bigint;
BEGIN
  SELECT count(*) INTO n FROM live_item l JOIN ledger.item_reservation x ON x.tenant_id = l.tenant_id AND x.owner_id = l.run_id
    AND x.fiscal_year = l.item_fiscal_year AND x.journal_entry_id = l.item_entry_id AND x.line_no = l.item_line_no
  WHERE x.released_at IS NOT NULL;
  IF n > 0 THEN
    RAISE EXCEPTION '% item(s) of live payment runs were released by their run and cannot be reserved for it again', n
      USING ERRCODE = 'check_violation', HINT = 'Reject the runs that pay them and propose again, then rerun the migrations.';
  END IF;
END $$;

-- A live item without a reservation is reserved for its run, as 000034
-- did: in the proposer's name, under the run's document number.
INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context, owner_reference, created_by)
SELECT l.tenant_id, l.run_id, l.item_fiscal_year, l.item_entry_id, l.item_line_no, 'payments', l.document_number, l.proposed_by
FROM live_item l
WHERE NOT EXISTS (SELECT FROM ledger.item_reservation x WHERE x.tenant_id = l.tenant_id AND x.owner_id = l.run_id
  AND x.fiscal_year = l.item_fiscal_year AND x.journal_entry_id = l.item_entry_id AND x.line_no = l.item_line_no);
