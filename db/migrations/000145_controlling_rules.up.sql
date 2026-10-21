-- Configuration rules for controlling (T56c; ADR-0050 §4; 03-data-model
-- §3.7). The configuration document keeps the rules about cost and profit
-- centers; the ledger's setup (ledger.Setup.Apply) writes them here on
-- activation, where postings (T56d) and center changes read them. Expand
-- only: a column with a constant default, a small backfill of master data,
-- new tables and a foreign key on a small master data table.

-- An account's cost element category (SAP KATYP): which cost object a line
-- on it needs. none for balance sheet accounts and uncontrolled P&L
-- accounts, primary_cost (SAP 01) for expenses, revenue (SAP 11/12) for
-- revenues; secondary cost elements come with allocations (T60). Accounts
-- set up before T56c take the default of their type, as setup writes it.
ALTER TABLE ledger.gl_account ADD COLUMN cost_element text NOT NULL DEFAULT 'none',
  ADD CONSTRAINT gl_account_cost_element CHECK (cost_element = 'none' OR (cost_element = 'primary_cost' AND account_type = 'expense')
    OR (cost_element = 'revenue' AND account_type = 'revenue'));
UPDATE ledger.gl_account SET cost_element = CASE account_type WHEN 'expense' THEN 'primary_cost' ELSE 'revenue' END
  WHERE account_type IN ('expense', 'revenue');

-- The tenant's cost center categories (SAP KOSAR, controlling.
-- cost_center_categories): whether revenue may carry a cost center of the
-- category (T56d) and the locks a new cost center starts with. A category
-- the configuration no longer has is deactivated, never deleted; setup
-- refuses removing one an active cost center period has.
CREATE TABLE ledger.cost_center_category (
  tenant_id            uuid NOT NULL,
  code                 text NOT NULL CHECK (code ~ '^[0-9A-Z_]{1,10}$'),
  name                 text NOT NULL CHECK (name <> ''),
  revenue_allowed      boolean NOT NULL DEFAULT false,
  lock_actual_cost     boolean NOT NULL DEFAULT false,
  lock_actual_revenue  boolean NOT NULL DEFAULT false,
  lock_plan            boolean NOT NULL DEFAULT false,
  is_active            boolean NOT NULL DEFAULT true,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, code)
);
CREATE TRIGGER cost_center_category_touch BEFORE UPDATE ON ledger.cost_center_category FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.cost_center_category'::regclass);

-- T56a accepted any category code; the ones its periods have become
-- categories, named by their code, until the configuration names them.
INSERT INTO ledger.cost_center_category (tenant_id, code, name, created_by, updated_by)
SELECT tenant_id, category, category, (array_agg(created_by ORDER BY created_at))[1], (array_agg(created_by ORDER BY created_at))[1]
FROM ledger.cost_center_period GROUP BY tenant_id, category;

ALTER TABLE ledger.cost_center_period ADD CONSTRAINT cost_center_period_category
  FOREIGN KEY (tenant_id, category) REFERENCES ledger.cost_center_category (tenant_id, code);

-- A company's controlling (SAP's assignment of the company code to the
-- controlling area): the fiscal year cost center accounting (decision P6)
-- and profit center accounting (decision P2) apply from, NULL while off,
-- and the default profit center, a profit center assigned to the company,
-- that P&L lines nothing else assigns take. Once on, accounting stays on
-- from its year: a year's P&L by center is complete or not kept.
CREATE TABLE ledger.company_controlling (
  tenant_id                      uuid NOT NULL,
  legal_entity_id                uuid NOT NULL,      -- tenancy context
  cost_center_accounting_from    smallint,           -- fiscal year
  profit_center_accounting_from  smallint,           -- fiscal year
  default_profit_center_id       uuid,
  created_at                     timestamptz NOT NULL DEFAULT now(),
  created_by                     uuid NOT NULL,
  updated_at                     timestamptz NOT NULL DEFAULT now(),
  updated_by                     uuid NOT NULL,
  version                        integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id),
  CONSTRAINT company_controlling_default_profit_center FOREIGN KEY (tenant_id, default_profit_center_id, legal_entity_id)
    REFERENCES ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id),
  CONSTRAINT company_controlling_needs_default CHECK (profit_center_accounting_from IS NULL OR default_profit_center_id IS NOT NULL)
);
CREATE TRIGGER company_controlling_touch BEFORE UPDATE ON ledger.company_controlling FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.company_controlling'::regclass);

CREATE FUNCTION ledger.check_company_controlling() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.cost_center_accounting_from IS NOT NULL AND NEW.cost_center_accounting_from IS DISTINCT FROM OLD.cost_center_accounting_from)
     OR (OLD.profit_center_accounting_from IS NOT NULL AND NEW.profit_center_accounting_from IS DISTINCT FROM OLD.profit_center_accounting_from) THEN
    RAISE EXCEPTION 'company_controlling: center accounting stays on from the year it was turned on'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'company_controlling_stays_on';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_company_controlling() OWNER TO erp_owner;
CREATE TRIGGER company_controlling_stays_on BEFORE UPDATE ON ledger.company_controlling
  FOR EACH ROW EXECUTE FUNCTION ledger.check_company_controlling();

-- The cost center a company's lines on an account take when none is
-- entered or derived (SAP OKB9), a cost center of that company. An
-- assignment the configuration no longer has is deactivated.
CREATE TABLE ledger.default_assignment (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  account_id       uuid NOT NULL,
  cost_center_id   uuid NOT NULL,
  is_active        boolean NOT NULL DEFAULT true,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  CONSTRAINT default_assignment_cost_center_company FOREIGN KEY (tenant_id, cost_center_id, legal_entity_id)
    REFERENCES ledger.cost_center (tenant_id, id, legal_entity_id)
);
-- The defaults naming a cost center: its lock and redate checks.
CREATE INDEX default_assignment_by_cost_center ON ledger.default_assignment (tenant_id, cost_center_id) WHERE is_active;
CREATE TRIGGER default_assignment_touch BEFORE UPDATE ON ledger.default_assignment FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.default_assignment'::regclass);
