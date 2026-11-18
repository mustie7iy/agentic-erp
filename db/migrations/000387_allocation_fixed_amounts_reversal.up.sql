-- Fixed amounts per receiver, and an allocation run reversed (T60k;
-- ADR-0064 As built T60k, plan-depth-3 §T60k). An assessment cycle's
-- receivers may be charged fixed amounts in the company's local currency,
-- whatever the sender's costs; such a run reads no costs, so it names no
-- account. A run is reversed once, its entry reversed through the ledger
-- (the reversal's entry is the ledger's: no foreign key crosses a
-- schema), and is then no longer in force: the cycle may run for the
-- period again, as the run's attempt within its cycle and period, one
-- in force at a time. Expand only (06 §6.4): a new table, a nullable
-- column, a column with a constant default, a trigger about values the
-- previous version never writes, and wider checks added NOT VALID, which
-- 000389 validates; 000388 builds the new unique index concurrently, and
-- 000389 drops the narrower one-run rule it replaces. The previous
-- version writes attempt 1 only, which the trigger accepts while no
-- reversed run of the period exists, and it reads a reversed run as the
-- period's run, refusing to run again: nothing it does is wrong.

-- The share basis fixed_amounts, an assessment's alone.
ALTER TABLE controlling.allocation_cycle DROP CONSTRAINT allocation_cycle_share_basis;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_share_basis
  CHECK (share_basis IN ('fixed_percentages', 'statistical_key_figure', 'fixed_amounts')) NOT VALID;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_fixed_amounts
  CHECK (share_basis <> 'fixed_amounts' OR kind = 'assessment') NOT VALID;

-- A receiver's fixed amount, in minor units of the company's local
-- currency, above zero.
ALTER TABLE controlling.allocation_receiver ADD COLUMN amount bigint;
ALTER TABLE controlling.allocation_receiver ADD CONSTRAINT allocation_receiver_amount CHECK (amount > 0) NOT VALID;

-- 000385's rule with fixed amounts: by fixed percentages every receiver
-- has its share and they add up to the whole; by fixed amounts every
-- receiver has its amount; by a key figure the cycle has receivers. Only
-- fixed percentages hold shares and only fixed amounts amounts.
CREATE OR REPLACE FUNCTION controlling.allocation_shares_whole(tenant uuid, cycle uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  total bigint;
  shared bigint;
  amounts bigint;
  receivers bigint;
  sender uuid;
  basis text;
BEGIN
  SELECT sender_cost_center_id, share_basis INTO sender, basis FROM controlling.allocation_cycle WHERE tenant_id = tenant AND id = cycle;
  SELECT coalesce(sum(share_bp), 0), count(share_bp), count(amount), count(*) INTO total, shared, amounts, receivers
    FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle;
  IF basis = 'fixed_percentages' AND (total <> 10000 OR shared <> receivers OR amounts <> 0) THEN
    RAISE EXCEPTION 'the shares of allocation cycle % add up to % basis points, not 10000, or a receiver has none or an amount', cycle, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_whole';
  END IF;
  IF basis <> 'fixed_percentages' AND (shared <> 0 OR receivers = 0 OR amounts <> CASE WHEN basis = 'fixed_amounts' THEN receivers ELSE 0 END) THEN
    RAISE EXCEPTION 'allocation cycle % takes its shares from %, so it has receivers, none has a share of its own, and each has an amount exactly by fixed amounts',
      cycle, basis USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_by_basis';
  END IF;
  IF EXISTS (SELECT 1 FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle AND cost_center_id = sender) THEN
    RAISE EXCEPTION 'allocation cycle % names its sender as a receiver', cycle
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_not_sender';
  END IF;
END $$;

-- A run by fixed amounts names no account; its attempt within its cycle
-- and period, 1 for every run before.
ALTER TABLE controlling.allocation_run DROP CONSTRAINT allocation_run_account_count_check;
ALTER TABLE controlling.allocation_run ADD CONSTRAINT allocation_run_account_count CHECK (account_count >= 0) NOT VALID;
-- The attempt is held by the trigger below, which refuses any but the
-- next one.
ALTER TABLE controlling.allocation_run ADD COLUMN attempt smallint NOT NULL DEFAULT 1;

-- A run reversed, once (the primary key), by one entry. Insert-only: the
-- table has no version, so the application is granted no UPDATE
-- (platform.secure_table), and never a DELETE; a reversal is not taken
-- back.
CREATE TABLE controlling.allocation_run_reversal (
  tenant_id         uuid NOT NULL,
  run_id            uuid NOT NULL,
  journal_entry_id  uuid NOT NULL,     -- ledger context: the reversal's entry
  document_number   text NOT NULL CONSTRAINT allocation_run_reversal_number CHECK (document_number <> ''),
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, run_id),
  CONSTRAINT allocation_run_reversal_run FOREIGN KEY (tenant_id, run_id) REFERENCES controlling.allocation_run (tenant_id, id),
  CONSTRAINT allocation_run_reversal_entry_once UNIQUE (tenant_id, journal_entry_id)
);
SELECT platform.secure_table('controlling.allocation_run_reversal'::regclass);

-- One run in force per cycle and period: a run's attempt is one more than
-- the cycle's runs of the period before it, each of them reversed. Two
-- runs of one attempt are refused by the unique index of 000388.
CREATE FUNCTION controlling.check_allocation_run_attempt() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  earlier bigint;
  in_force bigint;
BEGIN
  SELECT count(*), count(*) FILTER (WHERE v.run_id IS NULL) INTO earlier, in_force
    FROM controlling.allocation_run r LEFT JOIN controlling.allocation_run_reversal v ON v.tenant_id = r.tenant_id AND v.run_id = r.id
    WHERE r.tenant_id = NEW.tenant_id AND r.cycle_id = NEW.cycle_id AND r.fiscal_year = NEW.fiscal_year AND r.fiscal_period = NEW.fiscal_period
      AND r.attempt < NEW.attempt;
  IF earlier <> NEW.attempt - 1 OR in_force <> 0 THEN
    RAISE EXCEPTION 'run % of allocation cycle % for %/% is attempt %, after % earlier runs, % of them in force', NEW.id, NEW.cycle_id, NEW.fiscal_year,
      NEW.fiscal_period, NEW.attempt, earlier, in_force USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_run_in_force_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION controlling.check_allocation_run_attempt() OWNER TO erp_owner;
CREATE TRIGGER allocation_run_in_force_once BEFORE INSERT ON controlling.allocation_run
  FOR EACH ROW EXECUTE FUNCTION controlling.check_allocation_run_attempt();
