-- The balance carry-forward (ADR-0059 §4-§6, §14, T64m2; 03-data-model
-- §3.7): the opening balances of fiscal year Y+1 of a company and ledger,
-- stored as balances per run, never as journal lines, and the retained
-- earnings account that takes year Y's profit or loss.
--
-- Expand only (06 §6.4): a column with a default, a check and a key added
-- NOT VALID (000228 validates them), new tables and a permission, none of
-- which the previous release writes. The previous release's OpenPeriod
-- updates period_status as before; the trigger counts its openings too.

-- How many times a period has been opened (ADR-0059 §6): a posting needs an
-- open period, so a period whose count has not moved since a run read it
-- and that is not open now has taken no posting since. The trigger adds one
-- on every change of the status to open, whoever writes it, and refuses
-- any other change of the count; a period starts at 0, or 1 when it is
-- inserted open.
ALTER TABLE ledger.period_status
  ADD COLUMN opened_count integer NOT NULL DEFAULT 0,
  ADD CONSTRAINT period_status_opened_count CHECK (opened_count >= 0) NOT VALID;

CREATE FUNCTION ledger.count_period_openings() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.opened_count <> 0 THEN
      RAISE EXCEPTION 'period %/% starts with an opened count of 0, not %', NEW.fiscal_year, NEW.period, NEW.opened_count
        USING ERRCODE = 'check_violation', CONSTRAINT = 'period_status_opened_count_set';
    END IF;
    -- A period inserted open (test fixtures do) has been opened once.
    NEW.opened_count := (NEW.status = 'open')::int;
    RETURN NEW;
  END IF;
  IF NEW.opened_count IS DISTINCT FROM OLD.opened_count THEN
    RAISE EXCEPTION 'period %/%: the opened count is kept by the database, not set', OLD.fiscal_year, OLD.period
      USING ERRCODE = 'check_violation', CONSTRAINT = 'period_status_opened_count_set';
  END IF;
  IF NEW.status = 'open' AND OLD.status <> 'open' THEN
    NEW.opened_count := OLD.opened_count + 1;
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.count_period_openings() OWNER TO erp_owner;
CREATE TRIGGER period_status_opened_count BEFORE INSERT OR UPDATE ON ledger.period_status
  FOR EACH ROW EXECUTE FUNCTION ledger.count_period_openings();

-- The company's retained earnings account, one for every ledger it keeps
-- (ADR-0059 §4, the product owner 2026-11-05): an account determination key,
-- applied from companies[].year_end.retained_earnings_account.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings')) NOT VALID;

-- A carry-forward run of a company and ledger from fiscal year Y into Y+1:
-- chained from the previous year's current run, or from every year up to
-- Y; the retained earnings account it carried the result to; current until
-- a later run of the same year supersedes it, naming it, in the same
-- transaction (superseded_by_id is checked at commit, when the successor
-- exists). One current run per company, ledger and year.
CREATE TABLE ledger.carry_forward_run (
  tenant_id                     uuid NOT NULL,
  id                            uuid NOT NULL,
  legal_entity_id               uuid NOT NULL,           -- tenancy context
  ledger_id                     uuid NOT NULL,
  from_fiscal_year              smallint NOT NULL,
  to_fiscal_year                smallint NOT NULL,
  basis                         text NOT NULL CHECK (basis IN ('chained', 'all_years')),
  chained_from_id               uuid,
  retained_earnings_account_id  uuid NOT NULL,
  status                        text NOT NULL DEFAULT 'current' CHECK (status IN ('current', 'superseded')),
  superseded_by_id              uuid,
  superseded_at                 timestamptz,
  row_count                     integer NOT NULL CHECK (row_count >= 0),
  created_at                    timestamptz NOT NULL DEFAULT now(),
  created_by                    uuid NOT NULL,           -- identity context: a person
  version                       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id),
  FOREIGN KEY (tenant_id, legal_entity_id, retained_earnings_account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, chained_from_id) REFERENCES ledger.carry_forward_run (tenant_id, id),
  FOREIGN KEY (tenant_id, superseded_by_id) REFERENCES ledger.carry_forward_run (tenant_id, id) DEFERRABLE INITIALLY DEFERRED,
  CONSTRAINT carry_forward_run_years CHECK (to_fiscal_year = from_fiscal_year + 1),
  CONSTRAINT carry_forward_run_basis CHECK ((basis = 'chained') = (chained_from_id IS NOT NULL)),
  CONSTRAINT carry_forward_run_superseded CHECK ((status = 'superseded') = (superseded_by_id IS NOT NULL)
    AND (superseded_by_id IS NULL) = (superseded_at IS NULL) AND superseded_by_id IS DISTINCT FROM id)
);
CREATE UNIQUE INDEX carry_forward_run_current ON ledger.carry_forward_run (tenant_id, legal_entity_id, ledger_id, from_fiscal_year)
  WHERE status = 'current';
-- A company's runs of a ledger and year, newest first (the history).
CREATE INDEX carry_forward_run_by_year ON ledger.carry_forward_run (tenant_id, legal_entity_id, ledger_id, from_fiscal_year, created_at);

-- A run is current when inserted, chained only from the previous year's
-- run of its company and ledger, and carries to an equity account; it
-- moves only from current to superseded, naming its successor, and nothing
-- else of it changes. Never deleted.
CREATE FUNCTION ledger.check_carry_forward_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'carry-forward run % is kept', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_fixed';
  END IF;
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'current' THEN
      RAISE EXCEPTION 'a carry-forward run is current first, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_lifecycle';
    END IF;
    IF NEW.chained_from_id IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.carry_forward_run r WHERE r.tenant_id = NEW.tenant_id
        AND r.id = NEW.chained_from_id AND r.legal_entity_id = NEW.legal_entity_id AND r.ledger_id = NEW.ledger_id
        AND r.to_fiscal_year = NEW.from_fiscal_year) THEN
      RAISE EXCEPTION 'carry-forward run % chains only from the previous year''s run of its company and ledger', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_chain';
    END IF;
    IF NOT EXISTS (SELECT FROM ledger.gl_account a WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.retained_earnings_account_id
        AND a.account_type = 'equity') THEN
      RAISE EXCEPTION 'carry-forward run % carries the result to an equity account', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_retained_earnings';
    END IF;
    RETURN NEW;
  END IF;
  IF NOT (OLD.status = 'current' AND NEW.status = 'superseded') THEN
    RAISE EXCEPTION 'carry-forward run % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_lifecycle';
  END IF;
  IF (NEW.legal_entity_id, NEW.ledger_id, NEW.from_fiscal_year, NEW.to_fiscal_year, NEW.basis, NEW.chained_from_id,
      NEW.retained_earnings_account_id, NEW.row_count, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM
     (OLD.legal_entity_id, OLD.ledger_id, OLD.from_fiscal_year, OLD.to_fiscal_year, OLD.basis, OLD.chained_from_id,
      OLD.retained_earnings_account_id, OLD.row_count, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'carry-forward run % is fixed but for being superseded', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_carry_forward_run() OWNER TO erp_owner;
CREATE TRIGGER carry_forward_run_check BEFORE INSERT OR UPDATE OR DELETE ON ledger.carry_forward_run
  FOR EACH ROW EXECUTE FUNCTION ledger.check_carry_forward_run();
SELECT platform.secure_table('ledger.carry_forward_run'::regclass);

-- A run's opening balances (ADR-0059 §5): per account and transaction
-- currency, the amount in transaction, local and group currency, debit
-- positive. A balance sheet account carries its balance (kind balance);
-- year Y's profit or loss is carried per currency to the run's retained
-- earnings account as kind result, beside that account's own balance row.
-- Insert-only.
CREATE TABLE ledger.carry_forward_balance (
  tenant_id    uuid NOT NULL,
  run_id       uuid NOT NULL,
  account_id   uuid NOT NULL,
  kind         text NOT NULL CHECK (kind IN ('balance', 'result')),
  currency_tc  char(3) NOT NULL REFERENCES ref.currency,
  amount_tc    bigint NOT NULL,
  amount_lc    bigint NOT NULL,
  amount_gc    bigint NOT NULL,
  PRIMARY KEY (tenant_id, run_id, account_id, kind, currency_tc),
  FOREIGN KEY (tenant_id, run_id) REFERENCES ledger.carry_forward_run (tenant_id, id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES ledger.gl_account (tenant_id, id)
);
CREATE TRIGGER carry_forward_balance_immutable BEFORE UPDATE OR DELETE ON ledger.carry_forward_balance
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- A result row is on its run's retained earnings account alone.
CREATE FUNCTION ledger.check_carry_forward_result() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind = 'result' AND NOT EXISTS (SELECT FROM ledger.carry_forward_run r WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.run_id
      AND r.retained_earnings_account_id = NEW.account_id) THEN
    RAISE EXCEPTION 'carry-forward run %: a result row is on the run''s retained earnings account, not %', NEW.run_id, NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_balance_result';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_carry_forward_result() OWNER TO erp_owner;
CREATE TRIGGER carry_forward_balance_result BEFORE INSERT ON ledger.carry_forward_balance
  FOR EACH ROW WHEN (NEW.kind = 'result') EXECUTE FUNCTION ledger.check_carry_forward_result();
SELECT platform.secure_table('ledger.carry_forward_balance'::regclass);

-- The opened count of every period of year Y and every earlier year of the
-- run's ledger when it ran (ADR-0059 §6): what its staleness is judged by.
-- Insert-only.
CREATE TABLE ledger.carry_forward_period (
  tenant_id     uuid NOT NULL,
  run_id        uuid NOT NULL,
  period_id     uuid NOT NULL,
  opened_count  integer NOT NULL CHECK (opened_count >= 0),
  PRIMARY KEY (tenant_id, run_id, period_id),
  FOREIGN KEY (tenant_id, run_id) REFERENCES ledger.carry_forward_run (tenant_id, id),
  FOREIGN KEY (tenant_id, period_id) REFERENCES ledger.period_status (tenant_id, id)
);
CREATE TRIGGER carry_forward_period_immutable BEFORE UPDATE OR DELETE ON ledger.carry_forward_period
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.carry_forward_period'::regclass);

-- Running a carry-forward (ADR-0059 §12): people only, granted with
-- ledger.period.lock.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.carry_forward.run', 'Carry a fiscal year''s balances forward into the next year', false);
