-- Consolidation runs and currency translation (T68d; ADR-0059 §5, §6, §7;
-- 03-data-model §3.24): a run of a consolidation group for a regular
-- period, queued by the request with its Operation and carried out as a
-- workflow; its members, each with its closing rate and the status and
-- version of its period when the run read its figures; and their
-- translated balances in the group currency. parent_id, the members'
-- legal entities and the trading partners are the tenancy context's, the
-- accounts the ledger's, operation_id the platform's: no foreign key
-- leaves the schema (02 §2.1). Group accounts are the configuration's
-- numbers, recorded with the configuration version the run read. Expand
-- only: a new schema and permission, which the previous version neither
-- reads nor writes.
CREATE SCHEMA consolidation;
GRANT USAGE ON SCHEMA consolidation TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('consolidation');
SELECT platform.secure_table('consolidation.outbox'::regclass);
SELECT platform.secure_table('consolidation.inbox'::regclass);
GRANT UPDATE (published_at) ON consolidation.outbox TO erp_app;
GRANT USAGE ON SCHEMA consolidation TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON consolidation.outbox TO erp_relay;

-- A run (§5, §6): queued -> running -> computed; queued or running ->
-- failed; computed -> final or discarded; failed -> discarded; final ->
-- superseded. companies are the group's parent and every member as the
-- run's configuration version has them, so a run is shown only to a
-- caller who may act in all of them (§12) without reading that version.
CREATE TABLE consolidation.run (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  group_code      text NOT NULL CHECK (group_code ~ '^[A-Z0-9_]{1,30}$'),
  parent_id       uuid NOT NULL,           -- tenancy context
  companies       uuid[] NOT NULL,
  fiscal_year     smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  fiscal_period   smallint NOT NULL,
  period_start    date NOT NULL,           -- the parent's leading ledger's period
  period_end      date NOT NULL,
  config_version  integer NOT NULL CHECK (config_version > 0),
  group_currency  char(3) NOT NULL REFERENCES ref.currency,
  rate_type       text NOT NULL,
  status          text NOT NULL CHECK (status IN ('queued', 'running', 'computed', 'failed', 'final', 'superseded', 'discarded')),
  problem         jsonb,
  operation_id    uuid NOT NULL,           -- platform.operation
  computed_at     timestamptz,
  finalized_by    uuid,                    -- identity context
  finalized_at    timestamptz,
  superseded_by   uuid,
  version         integer NOT NULL DEFAULT 1,
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  updated_by      uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT run_regular_period CHECK (fiscal_period BETWEEN 1 AND 12),
  CONSTRAINT run_period_dates CHECK (period_end >= period_start),
  CONSTRAINT run_companies CHECK (cardinality(companies) >= 2),
  CONSTRAINT run_parent_included CHECK (parent_id = ANY (companies)),
  -- The problem that failed it, kept when a failed run is discarded.
  CONSTRAINT run_problem CHECK (CASE status WHEN 'failed' THEN problem IS NOT NULL WHEN 'discarded' THEN true ELSE problem IS NULL END),
  -- When it was computed, kept from then on.
  CONSTRAINT run_computed CHECK (CASE WHEN status IN ('computed', 'final', 'superseded') THEN computed_at IS NOT NULL
    WHEN status IN ('queued', 'running', 'failed') THEN computed_at IS NULL ELSE true END),
  CONSTRAINT run_finalized CHECK ((status IN ('final', 'superseded')) = (finalized_by IS NOT NULL AND finalized_at IS NOT NULL)
    AND (finalized_by IS NULL) = (finalized_at IS NULL)),
  CONSTRAINT run_superseded CHECK ((status = 'superseded') = (superseded_by IS NOT NULL))
);
CREATE TRIGGER run_touch BEFORE UPDATE ON consolidation.run FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- One run of a group queued or running at a time (consolidation.run_in_progress).
CREATE UNIQUE INDEX consolidation_run_one_at_a_time ON consolidation.run (tenant_id, group_code) WHERE status IN ('queued', 'running');
-- One final run per group and period (consolidation.final_exists, T68g).
CREATE UNIQUE INDEX consolidation_run_one_final ON consolidation.run (tenant_id, group_code, fiscal_year, fiscal_period) WHERE status = 'final';
-- The list, newest first, and a group's period's runs.
CREATE INDEX run_by_group ON consolidation.run (tenant_id, group_code, fiscal_year, fiscal_period);
CREATE INDEX run_by_created ON consolidation.run (tenant_id, created_at, id);
SELECT platform.secure_table('consolidation.run'::regclass);

-- A run starts queued and moves only along §5's transitions; what it
-- consolidates never changes, nor anything of a superseded or discarded
-- run.
CREATE FUNCTION consolidation.check_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'queued' THEN
      RAISE EXCEPTION 'a consolidation run starts queued, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'run_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status IN ('superseded', 'discarded') OR (NEW.status <> OLD.status AND (OLD.status, NEW.status) NOT IN
      (('queued', 'running'), ('running', 'computed'), ('queued', 'failed'), ('running', 'failed'), ('computed', 'final'),
       ('computed', 'discarded'), ('failed', 'discarded'), ('final', 'superseded'))) THEN
    RAISE EXCEPTION 'consolidation run % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_transition';
  END IF;
  IF (NEW.group_code, NEW.parent_id, NEW.companies, NEW.fiscal_year, NEW.fiscal_period, NEW.period_start, NEW.period_end, NEW.config_version,
      NEW.group_currency, NEW.rate_type, NEW.operation_id, NEW.created_at, NEW.created_by) IS DISTINCT FROM (OLD.group_code, OLD.parent_id,
      OLD.companies, OLD.fiscal_year, OLD.fiscal_period, OLD.period_start, OLD.period_end, OLD.config_version, OLD.group_currency, OLD.rate_type,
      OLD.operation_id, OLD.created_at, OLD.created_by)
      OR (OLD.problem IS NOT NULL AND NEW.problem IS DISTINCT FROM OLD.problem)
      OR (OLD.computed_at IS NOT NULL AND NEW.computed_at IS DISTINCT FROM OLD.computed_at)
      OR (OLD.finalized_at IS NOT NULL AND (NEW.finalized_at, NEW.finalized_by) IS DISTINCT FROM (OLD.finalized_at, OLD.finalized_by)) THEN
    RAISE EXCEPTION 'consolidation run % keeps what it consolidates and its outcome', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'run_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION consolidation.check_run() OWNER TO erp_owner;
CREATE TRIGGER run_transition BEFORE INSERT OR UPDATE ON consolidation.run FOR EACH ROW EXECUTE FUNCTION consolidation.check_run();

-- A run's companies (§6, §7): the parent and each member whose first
-- period is on or before the run's, written when the run starts running,
-- each with its code as the run read it, its local currency and closing
-- rate into the group currency (1, valid from no date, for one in the
-- group currency), and, once translated, the status and version of its
-- leading ledger's period when its figures were read.
CREATE TABLE consolidation.run_member (
  tenant_id            uuid NOT NULL,
  run_id               uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,      -- tenancy context
  company_code         text NOT NULL,
  role                 text NOT NULL CHECK (role IN ('parent', 'member')),
  ownership_percent    numeric(7,4) CHECK (ownership_percent > 0 AND ownership_percent <= 100),
  first_fiscal_year    smallint,
  first_fiscal_period  smallint CHECK (first_fiscal_period BETWEEN 1 AND 12),
  local_currency       char(3) NOT NULL REFERENCES ref.currency,
  closing_rate         numeric(28,12) NOT NULL CHECK (closing_rate > 0),
  rate_valid_from      date,
  period_status        text CHECK (period_status IN ('open', 'closed', 'locked')),
  period_version       integer,
  translated_at        timestamptz,
  PRIMARY KEY (tenant_id, run_id, legal_entity_id),
  UNIQUE (tenant_id, run_id, company_code),
  FOREIGN KEY (tenant_id, run_id) REFERENCES consolidation.run (tenant_id, id),
  -- A member has its ownership and first period; the parent none.
  CONSTRAINT run_member_role CHECK ((role = 'member') = (ownership_percent IS NOT NULL)
    AND (role = 'member') = (first_fiscal_year IS NOT NULL AND first_fiscal_period IS NOT NULL)
    AND (first_fiscal_year IS NULL) = (first_fiscal_period IS NULL)),
  CONSTRAINT run_member_translated CHECK ((translated_at IS NULL) = (period_status IS NULL) AND (translated_at IS NULL) = (period_version IS NULL))
);
SELECT platform.secure_table('consolidation.run_member'::regclass);
-- Translating a member records its period once; nothing else changes.
GRANT UPDATE (period_status, period_version, translated_at) ON consolidation.run_member TO erp_app;

-- A run's translated balances (§7): per member, one row per company
-- account and trading partner (kind balance, with the local, booked group
-- and translated amounts), the earlier results on the retained earnings
-- account (prior_results) and the translation difference on the cta
-- account (cta, a group amount alone). line_no follows the group
-- account's number, the company account's (none first), the trading
-- partner's code (none first) and the kind, so two runs of the same
-- figures number them alike (§5).
CREATE TABLE consolidation.run_balance (
  tenant_id           uuid NOT NULL,
  run_id              uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,
  line_no             integer NOT NULL CHECK (line_no >= 1),
  group_account       text NOT NULL CHECK (group_account ~ '^[0-9A-Z]{1,10}$'),
  account_id          uuid,                -- ledger context
  trading_partner_id  uuid,                -- tenancy context
  kind                text NOT NULL CHECK (kind IN ('balance', 'prior_results', 'cta')),
  amount_lc           bigint,
  amount_gc_booked    bigint,
  amount_gc           bigint NOT NULL,
  PRIMARY KEY (tenant_id, run_id, legal_entity_id, line_no),
  FOREIGN KEY (tenant_id, run_id, legal_entity_id) REFERENCES consolidation.run_member (tenant_id, run_id, legal_entity_id),
  -- A balance is a company account's; the computed rows are none's.
  CONSTRAINT run_balance_account CHECK ((kind = 'balance') = (account_id IS NOT NULL)),
  CONSTRAINT run_balance_partner CHECK (trading_partner_id IS NULL OR (kind = 'balance' AND trading_partner_id <> legal_entity_id)),
  -- The translation difference is a group amount alone.
  CONSTRAINT run_balance_amounts CHECK ((kind = 'cta') = (amount_lc IS NULL) AND (amount_lc IS NULL) = (amount_gc_booked IS NULL))
);
-- The balances list's filters by group account and by account.
CREATE INDEX run_balance_by_group_account ON consolidation.run_balance (tenant_id, run_id, group_account);
SELECT platform.secure_table('consolidation.run_balance'::regclass);

-- A run's members and balances are written while it runs (§5), the
-- members' translation recorded once, and never changed or removed after.
CREATE FUNCTION consolidation.check_run_rows() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  st text;
BEGIN
  SELECT status INTO st FROM consolidation.run WHERE tenant_id = NEW.tenant_id AND id = NEW.run_id;
  IF st IS DISTINCT FROM 'running' THEN
    RAISE EXCEPTION 'consolidation run % is %, not running: its rows are written while it runs', NEW.run_id, st
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_rows_while_running';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION consolidation.check_run_rows() OWNER TO erp_owner;
CREATE TRIGGER run_member_while_running BEFORE INSERT OR UPDATE ON consolidation.run_member
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_rows();
CREATE TRIGGER run_balance_while_running BEFORE INSERT ON consolidation.run_balance
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_rows();

CREATE FUNCTION consolidation.check_run_member() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.translated_at IS NOT NULL OR (NEW.tenant_id, NEW.run_id, NEW.legal_entity_id, NEW.company_code, NEW.role, NEW.ownership_percent,
      NEW.first_fiscal_year, NEW.first_fiscal_period, NEW.local_currency, NEW.closing_rate, NEW.rate_valid_from)
      IS DISTINCT FROM (OLD.tenant_id, OLD.run_id, OLD.legal_entity_id, OLD.company_code, OLD.role, OLD.ownership_percent, OLD.first_fiscal_year,
      OLD.first_fiscal_period, OLD.local_currency, OLD.closing_rate, OLD.rate_valid_from) THEN
    RAISE EXCEPTION 'member % of consolidation run % records its translation once', OLD.legal_entity_id, OLD.run_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_member_translated_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION consolidation.check_run_member() OWNER TO erp_owner;
CREATE TRIGGER run_member_translated_once BEFORE UPDATE ON consolidation.run_member
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_member();

-- Running and discarding consolidation runs (ADR-0059 §12).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('consolidation.run.manage', 'Start and discard consolidation runs of the groups whose companies one may act in', false);
