-- The controlling context and its plan values (T56g; ADR-0050 §2, §8,
-- decision P5; 03-data-model §3.19): plan categories (PLAN, FORECAST, ...)
-- that a person locks so a budget does not move after its approval, and
-- plan lines per category, company, fiscal year, period, cost center or
-- profit center and account in the company's local currency, SAP's ACDOCP
-- in miniature. Lines are written by batches: a batch replaces the plan of
-- its category, company, year and centers (marking their lines replaced,
-- never deleting them), and a batch key already applied
-- changes nothing again. legal_entity_id is the tenancy context's, the
-- centers and accounts the ledger's: no foreign key crosses a schema; the
-- controlling context checks them through ledger.AssignmentBalances.
-- Expand only: a new schema, which the previous version neither reads nor
-- writes.
CREATE SCHEMA controlling;
GRANT USAGE ON SCHEMA controlling TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('controlling');
SELECT platform.secure_table('controlling.outbox'::regclass);
SELECT platform.secure_table('controlling.inbox'::regclass);
GRANT UPDATE (published_at) ON controlling.outbox TO erp_app;
GRANT USAGE ON SCHEMA controlling TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON controlling.outbox TO erp_relay;

-- A plan category (SAP's category for planning): open, its plan replaced
-- by batches, or locked by a person holding controlling.plan.lock.
CREATE TABLE controlling.plan_category (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  code        text NOT NULL CHECK (code ~ '^[0-9A-Z_]{1,10}$'),
  name        text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
  locked      boolean NOT NULL DEFAULT false,
  version     integer NOT NULL DEFAULT 1,
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  updated_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT plan_category_code_once UNIQUE (tenant_id, code)
);
CREATE TRIGGER plan_category_touch BEFORE UPDATE ON controlling.plan_category FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('controlling.plan_category'::regclass);

-- A batch applied: its key, once per category, company and year, and the
-- digest of its lines, so the same batch again is recognised and changes
-- nothing, and the key with other lines is refused. Insert-only.
CREATE TABLE controlling.plan_batch (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  category_id      uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,      -- tenancy context
  fiscal_year      smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  batch_key        text NOT NULL CHECK (batch_key = btrim(batch_key) AND length(batch_key) BETWEEN 1 AND 100),
  digest           text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
  line_count       integer NOT NULL CHECK (line_count > 0),
  replaced_count   integer NOT NULL CHECK (replaced_count >= 0),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT plan_batch_key_once UNIQUE (tenant_id, category_id, legal_entity_id, fiscal_year, batch_key),
  -- Lines name their batch with its category, company and year.
  UNIQUE (tenant_id, id, category_id, legal_entity_id, fiscal_year),
  FOREIGN KEY (tenant_id, category_id) REFERENCES controlling.plan_category (tenant_id, id)
);
SELECT platform.secure_table('controlling.plan_batch'::regclass);

-- A plan line: the amount planned on a cost center or a profit center and
-- an account in a period, in the company's local currency (minor units,
-- debit positive as on the journal: costs positive, revenue negative),
-- with an optional quantity. Written by its batch and never deleted: a
-- later batch of the same category, company and year that names its
-- center marks it replaced (replaced_by_batch_id, written once), so the
-- plan is the lines not replaced, one per category, company, year,
-- center, account and period, and every earlier plan stays readable.
CREATE TABLE controlling.plan_line (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  batch_id          uuid NOT NULL,
  category_id       uuid NOT NULL,
  legal_entity_id   uuid NOT NULL,     -- tenancy context
  fiscal_year       smallint NOT NULL,
  fiscal_period     smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  cost_center_id    uuid,              -- ledger context
  profit_center_id  uuid,              -- ledger context
  account_id        uuid NOT NULL,     -- ledger context
  amount            bigint NOT NULL,
  replaced_by_batch_id uuid,
  quantity          numeric(20,6) CHECK (quantity > 0),
  uom_code          text REFERENCES ref.unit_of_measure,
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT plan_line_one_center CHECK (num_nonnulls(cost_center_id, profit_center_id) = 1),
  CONSTRAINT plan_line_quantity_unit CHECK ((quantity IS NULL) = (uom_code IS NULL)),
  FOREIGN KEY (tenant_id, batch_id, category_id, legal_entity_id, fiscal_year)
    REFERENCES controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year),
  -- Replaced by a later batch of the same category, company and year.
  CONSTRAINT plan_line_replaced_by FOREIGN KEY (tenant_id, replaced_by_batch_id, category_id, legal_entity_id, fiscal_year)
    REFERENCES controlling.plan_batch (tenant_id, id, category_id, legal_entity_id, fiscal_year),
  CONSTRAINT plan_line_replaced_later CHECK (replaced_by_batch_id <> batch_id)
);
-- The plan: one line per category, company, year, center, account and
-- period; the report reads a category's lines by company, year and cost
-- center.
CREATE UNIQUE INDEX plan_line_once ON controlling.plan_line (tenant_id, category_id, legal_entity_id, fiscal_year, cost_center_id, profit_center_id,
  account_id, fiscal_period) NULLS NOT DISTINCT WHERE replaced_by_batch_id IS NULL;
CREATE INDEX plan_line_by_batch ON controlling.plan_line (tenant_id, batch_id);
SELECT platform.secure_table('controlling.plan_line'::regclass);
GRANT UPDATE (replaced_by_batch_id) ON controlling.plan_line TO erp_app;

-- A line is replaced once, and nothing else of it changes.
CREATE FUNCTION controlling.check_plan_line_replaced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.replaced_by_batch_id IS NOT NULL OR NEW.replaced_by_batch_id IS NULL
     OR to_jsonb(NEW) - 'replaced_by_batch_id' <> to_jsonb(OLD) - 'replaced_by_batch_id' THEN
    RAISE EXCEPTION 'plan line % is replaced once and otherwise kept', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'plan_line_replaced_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION controlling.check_plan_line_replaced() OWNER TO erp_owner;
CREATE TRIGGER plan_line_replaced_once BEFORE UPDATE ON controlling.plan_line FOR EACH ROW EXECUTE FUNCTION controlling.check_plan_line_replaced();

-- A locked category's plan does not move: no batch is applied to it and
-- none of its lines is written or replaced. The category is read FOR
-- SHARE, so a lock waits for a batch under way, and a batch after the
-- lock sees it.
CREATE FUNCTION controlling.check_plan_open() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE is_locked boolean;
BEGIN
  SELECT c.locked INTO is_locked FROM controlling.plan_category c WHERE c.tenant_id = NEW.tenant_id AND c.id = NEW.category_id FOR SHARE;
  IF is_locked THEN
    RAISE EXCEPTION 'plan category % is locked', NEW.category_id USING ERRCODE = 'check_violation', CONSTRAINT = 'plan_category_open';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION controlling.check_plan_open() OWNER TO erp_owner;
CREATE TRIGGER plan_batch_open BEFORE INSERT ON controlling.plan_batch FOR EACH ROW EXECUTE FUNCTION controlling.check_plan_open();
CREATE TRIGGER plan_line_open BEFORE INSERT OR UPDATE ON controlling.plan_line FOR EACH ROW EXECUTE FUNCTION controlling.check_plan_open();

-- Reading, writing and locking (ADR-0050 §8, §12). Locking is an approval
-- permission, which agents cannot hold.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.plan.read', 'View plan categories, plan lines and plan against actual', false),
  ('controlling.plan.write', 'Create plan categories and upload plan batches', false),
  ('controlling.plan.lock', 'Lock and unlock plan categories, so a budget does not move after its approval', true);
