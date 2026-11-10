-- The investment and equity elimination (T68h; ADR-0059 §5, §9;
-- 03-data-model §3.24): each run's acquisition figures of its members,
-- the acquisition a group's first final run including a member freezes,
-- with its equity per group account, and the component of an
-- elimination entry's line. The investment entry is a run_entry of kind
-- investment (000238 allows it). Goodwill and the non-controlling
-- interest are columns for the steps that compute them (the tracker's
-- non-pilot leaves); a run refuses a member that needs them, so they are
-- zero today, and the rule that goodwill is the residual holds them to
-- it. Expand only: a new nullable column and new tables, which the
-- previous version neither reads nor writes.

-- What an elimination entry's line is (§9): the acquisition equity
-- reversed or the investment of an investment entry; none on an
-- intercompany entry's line. The interest's components (goodwill, nci,
-- profit, translation, other) come with the steps that compute them.
ALTER TABLE consolidation.run_entry_line ADD COLUMN component text
  CONSTRAINT run_entry_line_component CHECK (component IN ('acquisition_equity', 'investment'));

-- A run's acquisition figures of a member (§9), written by its
-- elimination step while it runs: measured at the period before the
-- member's first (F-1) at that period's closing rate, or read from the
-- frozen acquisition (frozen). The net assets are the member's asset and
-- liability balances through F-1, in local currency and translated; the
-- investment the parent's balance on the member's investment account
-- through the run's period. Goodwill is investment - net assets + the
-- interest (§9's residual), never otherwise.
CREATE TABLE consolidation.run_acquisition (
  tenant_id              uuid NOT NULL,
  run_id                 uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,
  origin                 text NOT NULL CHECK (origin IN ('incorporated', 'acquired')),
  investment_account_id  uuid NOT NULL,           -- ledger context
  net_assets_lc          bigint NOT NULL,
  closing_rate           numeric(28,12) NOT NULL CHECK (closing_rate > 0),
  rate_valid_from        date,
  net_assets_gc          bigint NOT NULL,
  investment_gc          bigint NOT NULL,
  goodwill_gc            bigint NOT NULL,
  nci_gc                 bigint NOT NULL,
  frozen                 boolean NOT NULL,
  PRIMARY KEY (tenant_id, run_id, legal_entity_id),
  FOREIGN KEY (tenant_id, run_id, legal_entity_id) REFERENCES consolidation.run_member (tenant_id, run_id, legal_entity_id),
  CONSTRAINT run_acquisition_goodwill CHECK (goodwill_gc = investment_gc - net_assets_gc + nci_gc)
);
SELECT platform.secure_table('consolidation.run_acquisition'::regclass);
CREATE TRIGGER run_acquisition_while_running BEFORE INSERT ON consolidation.run_acquisition
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_rows();

-- A member's acquisition, frozen by the group's first final run including
-- it (§9, §10), with what a later run checks against its configuration:
-- the origin, ownership, first period and investment account. One per
-- group and member, inserted once and never changed or removed (no update
-- grant: the table has no version).
CREATE TABLE consolidation.acquisition (
  tenant_id              uuid NOT NULL,
  group_code             text NOT NULL CHECK (group_code ~ '^[A-Z0-9_]{1,30}$'),
  legal_entity_id        uuid NOT NULL,          -- tenancy context
  origin                 text NOT NULL CHECK (origin IN ('incorporated', 'acquired')),
  ownership_percent      numeric(7,4) NOT NULL CHECK (ownership_percent > 0 AND ownership_percent <= 100),
  first_fiscal_year      smallint NOT NULL,
  first_fiscal_period    smallint NOT NULL CHECK (first_fiscal_period BETWEEN 1 AND 12),
  investment_account_id  uuid NOT NULL,          -- ledger context
  local_currency         char(3) NOT NULL,
  net_assets_lc          bigint NOT NULL,
  closing_rate           numeric(28,12) NOT NULL CHECK (closing_rate > 0),
  rate_valid_from        date,
  net_assets_gc          bigint NOT NULL,
  investment_gc          bigint NOT NULL,
  goodwill_gc            bigint NOT NULL,
  nci_gc                 bigint NOT NULL,
  run_id                 uuid NOT NULL,
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, group_code, legal_entity_id),
  FOREIGN KEY (tenant_id, run_id, legal_entity_id) REFERENCES consolidation.run_acquisition (tenant_id, run_id, legal_entity_id),
  CONSTRAINT acquisition_goodwill CHECK (goodwill_gc = investment_gc - net_assets_gc + nci_gc)
);
SELECT platform.secure_table('consolidation.acquisition'::regclass);

-- The acquisition equity (§9): the member's translated rows at F-1 other
-- than its assets and liabilities, per group account (its equity
-- accounts, retained earnings with every result through F-1, the
-- translation reserve), credit negative, summing to minus the net assets.
CREATE TABLE consolidation.acquisition_equity (
  tenant_id        uuid NOT NULL,
  group_code       text NOT NULL,
  legal_entity_id  uuid NOT NULL,
  group_account    text NOT NULL CHECK (group_account ~ '^[0-9A-Z]{1,10}$'),
  amount_gc        bigint NOT NULL CHECK (amount_gc <> 0),
  PRIMARY KEY (tenant_id, group_code, legal_entity_id, group_account),
  FOREIGN KEY (tenant_id, group_code, legal_entity_id) REFERENCES consolidation.acquisition (tenant_id, group_code, legal_entity_id)
);
SELECT platform.secure_table('consolidation.acquisition_equity'::regclass);

-- An acquisition is frozen by a final run of its group: inserted in the
-- finalization's transaction once the run is final. An import (the
-- tenant app.importing names, ADR-0029, 000230) inserts it as exported.
CREATE FUNCTION consolidation.check_acquisition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  st text;
  grp text;
BEGIN
  IF platform.importing(NEW.tenant_id) THEN
    RETURN NEW;
  END IF;
  SELECT status, group_code INTO st, grp FROM consolidation.run WHERE tenant_id = NEW.tenant_id AND id = NEW.run_id;
  IF st IS DISTINCT FROM 'final' OR grp IS DISTINCT FROM NEW.group_code THEN
    RAISE EXCEPTION 'the acquisition of % in group % is frozen by a final run of the group; run % is %', NEW.legal_entity_id, NEW.group_code,
      NEW.run_id, coalesce(st, 'unknown') USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_frozen_by_final_run';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION consolidation.check_acquisition() OWNER TO erp_owner;
CREATE TRIGGER acquisition_frozen_by_final_run BEFORE INSERT ON consolidation.acquisition
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_acquisition();
