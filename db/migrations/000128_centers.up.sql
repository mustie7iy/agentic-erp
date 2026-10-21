-- Cost centers, profit centers and segments (T56a; ADR-0050 §3, §11;
-- 03-data-model §3.7). They are the ledger's master data, beside the chart
-- of accounts, because they are dimensions of the journal (T56d adds them
-- to journal_line with foreign keys to these tables).
--
-- A cost center belongs to one legal entity, fixed: a changed company is a
-- new cost center. A profit center and a segment are tenant-wide (the
-- tenant is SAP's controlling area); a profit center is assigned to the
-- legal entities it may be posted in, and a cost center's profit center
-- must be assigned to the cost center's legal entity (a foreign key).
--
-- What a posting needs to know (category, profit center, locks) is
-- effective-dated in periods; texts (name, description, person
-- responsible) are on the center and change without approval, recorded in
-- the audit trail. Periods are written once: a change retires the periods
-- it replaces (retired_by_change_id) and inserts the new ones, so the
-- periods a posting read stay as they were (ADR-0006's reposting rule) and
-- every earlier state is kept. Active periods of a center never overlap.
--
-- Creating a center and every change to its periods or its profit center's
-- companies is a center_change approved by a controller (ADR-0050 §11,
-- decision P1); the change is applied in the deciding transaction. A
-- center is 'pending' until its create change is applied and 'rejected'
-- when it is not, which frees its code. Nothing here is deleted.

CREATE TABLE ledger.segment (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL,
  code         text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,10}$'),
  name         text NOT NULL CHECK (name <> ''),
  description  text CHECK (description <> ''),
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  updated_by   uuid NOT NULL,
  version      integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT segment_code_once UNIQUE (tenant_id, code)
);
CREATE TRIGGER segment_touch BEFORE UPDATE ON ledger.segment FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.segment'::regclass);

CREATE TABLE ledger.profit_center (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL,
  code         text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,10}$'),
  segment_id   uuid,                  -- the segment lines of the profit center carry (T56d)
  name         text NOT NULL CHECK (name <> ''),
  description  text CHECK (description <> ''),
  responsible  text NOT NULL CHECK (responsible <> ''),
  status       text NOT NULL CHECK (status IN ('pending', 'active', 'rejected')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  updated_by   uuid NOT NULL,
  version      integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, segment_id) REFERENCES ledger.segment (tenant_id, id)
);
-- A rejected center's code may be used again.
CREATE UNIQUE INDEX profit_center_code_once ON ledger.profit_center (tenant_id, code) WHERE status <> 'rejected';
CREATE TRIGGER profit_center_touch BEFORE UPDATE ON ledger.profit_center FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.profit_center'::regclass);

-- The legal entities a profit center may be posted in (SAP CEPC_BUKRS).
-- An assignment is never removed: a profit center leaving a company is a
-- reorganisation, not a master data change.
CREATE TABLE ledger.profit_center_company (
  tenant_id         uuid NOT NULL,
  profit_center_id  uuid NOT NULL,
  legal_entity_id   uuid NOT NULL,     -- tenancy context
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, profit_center_id, legal_entity_id),
  FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id)
);
CREATE TRIGGER profit_center_company_insert_only BEFORE UPDATE OR DELETE ON ledger.profit_center_company
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.profit_center_company'::regclass);

CREATE TABLE ledger.cost_center (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,      -- tenancy context; fixed
  code             text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,10}$'),
  name             text NOT NULL CHECK (name <> ''),
  description      text CHECK (description <> ''),
  responsible      text NOT NULL CHECK (responsible <> ''),
  status           text NOT NULL CHECK (status IN ('pending', 'active', 'rejected')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  -- Periods (and, from T56d, journal lines) name the cost center with its
  -- legal entity, so they cannot name another company's.
  CONSTRAINT cost_center_of_company UNIQUE (tenant_id, id, legal_entity_id)
);
CREATE UNIQUE INDEX cost_center_code_once ON ledger.cost_center (tenant_id, legal_entity_id, code) WHERE status <> 'rejected';
CREATE TRIGGER cost_center_touch BEFORE UPDATE ON ledger.cost_center FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.cost_center'::regclass);

-- A center's code and company are fixed, and its status goes from pending
-- to active or rejected, once.
CREATE FUNCTION ledger.check_center_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.code IS DISTINCT FROM OLD.code THEN
    RAISE EXCEPTION '%: the code of a center is fixed', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF TG_TABLE_NAME = 'cost_center' AND (to_jsonb(NEW) ->> 'legal_entity_id') IS DISTINCT FROM (to_jsonb(OLD) ->> 'legal_entity_id') THEN
    RAISE EXCEPTION 'cost_center: the legal entity of a cost center is fixed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'pending' AND NEW.status IN ('active', 'rejected')) THEN
    RAISE EXCEPTION '%: a center cannot go from % to %', TG_TABLE_NAME, OLD.status, NEW.status USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_center_change() OWNER TO erp_owner;
CREATE TRIGGER cost_center_change BEFORE UPDATE ON ledger.cost_center FOR EACH ROW EXECUTE FUNCTION ledger.check_center_change();
CREATE TRIGGER profit_center_change BEFORE UPDATE ON ledger.profit_center FOR EACH ROW EXECUTE FUNCTION ledger.check_center_change();

-- A requested change to a center (ADR-0050 §11), decided on its approval
-- request (workflow context, object type center_change). One pending
-- change per center. The payload is what was requested; it is applied
-- as requested once approved.
CREATE TABLE ledger.center_change (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  cost_center_id       uuid,
  profit_center_id     uuid,
  kind                 text NOT NULL CHECK (kind IN ('create', 'redate', 'lock', 'move', 'assign')),
  payload              jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  reason               text NOT NULL CHECK (reason <> ''),
  status               text NOT NULL CHECK (status IN ('pending', 'applied', 'rejected', 'cancelled')),
  workflow_request_id  uuid,               -- workflow context
  requested_by         uuid NOT NULL,
  applied_at           timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, cost_center_id) REFERENCES ledger.cost_center (tenant_id, id),
  FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id),
  CONSTRAINT center_change_one_center CHECK (num_nonnulls(cost_center_id, profit_center_id) = 1),
  -- Company assignment is a profit center's.
  CONSTRAINT center_change_assign CHECK (kind <> 'assign' OR profit_center_id IS NOT NULL),
  CONSTRAINT center_change_applied CHECK ((status = 'applied') = (applied_at IS NOT NULL))
);
CREATE UNIQUE INDEX center_change_one_pending ON ledger.center_change (tenant_id, coalesce(cost_center_id, profit_center_id))
  WHERE status = 'pending';
CREATE INDEX center_change_by_cost_center ON ledger.center_change (tenant_id, cost_center_id) WHERE cost_center_id IS NOT NULL;
CREATE INDEX center_change_by_profit_center ON ledger.center_change (tenant_id, profit_center_id) WHERE profit_center_id IS NOT NULL;
CREATE TRIGGER center_change_touch BEFORE UPDATE ON ledger.center_change FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.center_change'::regclass);

-- A change is decided once, from pending, and what it asks for is fixed.
CREATE FUNCTION ledger.check_center_change_decision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.cost_center_id, NEW.profit_center_id, NEW.kind, NEW.payload, NEW.reason, NEW.requested_by)
     IS DISTINCT FROM (OLD.cost_center_id, OLD.profit_center_id, OLD.kind, OLD.payload, OLD.reason, OLD.requested_by) THEN
    RAISE EXCEPTION 'center_change: what a change asks for is fixed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.status IS DISTINCT FROM OLD.status AND OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'center_change: a change cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.workflow_request_id IS DISTINCT FROM OLD.workflow_request_id AND OLD.workflow_request_id IS NOT NULL THEN
    RAISE EXCEPTION 'center_change: the approval request of a change is fixed' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_center_change_decision() OWNER TO erp_owner;
CREATE TRIGGER center_change_decision BEFORE UPDATE ON ledger.center_change FOR EACH ROW EXECUTE FUNCTION ledger.check_center_change_decision();

-- valid_to is exclusive, NULL open-ended (03 §3.1). profit_center_id is
-- assigned to the cost center's legal entity, or NULL: lines of a cost
-- center without one take the company's default profit center (T56c-d).
CREATE TABLE ledger.cost_center_period (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  cost_center_id        uuid NOT NULL,
  legal_entity_id       uuid NOT NULL,
  valid_from            date NOT NULL,
  valid_to              date,
  category              text NOT NULL CHECK (category ~ '^[0-9A-Z_]{1,10}$'),
  profit_center_id      uuid,
  lock_actual_cost      boolean NOT NULL DEFAULT false,
  lock_actual_revenue   boolean NOT NULL DEFAULT false,
  lock_plan             boolean NOT NULL DEFAULT false,
  change_id             uuid NOT NULL,      -- the change that wrote the period
  retired_by_change_id  uuid,               -- the change that replaced it
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  updated_by            uuid NOT NULL,
  version               integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT cost_center_period_company FOREIGN KEY (tenant_id, cost_center_id, legal_entity_id)
    REFERENCES ledger.cost_center (tenant_id, id, legal_entity_id),
  CONSTRAINT cost_center_period_profit_center_company FOREIGN KEY (tenant_id, profit_center_id, legal_entity_id)
    REFERENCES ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id),
  FOREIGN KEY (tenant_id, change_id) REFERENCES ledger.center_change (tenant_id, id),
  FOREIGN KEY (tenant_id, retired_by_change_id) REFERENCES ledger.center_change (tenant_id, id),
  CONSTRAINT cost_center_period_dates CHECK (valid_to IS NULL OR valid_to > valid_from),
  CONSTRAINT cost_center_period_overlap EXCLUDE USING gist (tenant_id WITH =, cost_center_id WITH =, daterange(valid_from, valid_to) WITH &&)
    WHERE (retired_by_change_id IS NULL)
);
CREATE INDEX cost_center_period_of ON ledger.cost_center_period (tenant_id, cost_center_id, valid_from) WHERE retired_by_change_id IS NULL;
-- The cost center periods naming a profit center: its redate's in-use
-- check and, from T56d, its postings' reads.
CREATE INDEX cost_center_period_by_profit_center ON ledger.cost_center_period (tenant_id, profit_center_id)
  WHERE profit_center_id IS NOT NULL AND retired_by_change_id IS NULL;
CREATE TRIGGER cost_center_period_touch BEFORE UPDATE ON ledger.cost_center_period FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.cost_center_period'::regclass);

CREATE TABLE ledger.profit_center_period (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  profit_center_id      uuid NOT NULL,
  valid_from            date NOT NULL,
  valid_to              date,
  locked                boolean NOT NULL DEFAULT false,
  change_id             uuid NOT NULL,
  retired_by_change_id  uuid,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  updated_by            uuid NOT NULL,
  version               integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id),
  FOREIGN KEY (tenant_id, change_id) REFERENCES ledger.center_change (tenant_id, id),
  FOREIGN KEY (tenant_id, retired_by_change_id) REFERENCES ledger.center_change (tenant_id, id),
  CONSTRAINT profit_center_period_dates CHECK (valid_to IS NULL OR valid_to > valid_from),
  CONSTRAINT profit_center_period_overlap EXCLUDE USING gist (tenant_id WITH =, profit_center_id WITH =, daterange(valid_from, valid_to) WITH &&)
    WHERE (retired_by_change_id IS NULL)
);
CREATE INDEX profit_center_period_of ON ledger.profit_center_period (tenant_id, profit_center_id, valid_from) WHERE retired_by_change_id IS NULL;
CREATE TRIGGER profit_center_period_touch BEFORE UPDATE ON ledger.profit_center_period FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.profit_center_period'::regclass);

-- A period is written once; the only change is its retirement, once.
CREATE FUNCTION ledger.check_center_period_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  bookkeeping text[] := ARRAY['retired_by_change_id', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF OLD.retired_by_change_id IS NOT NULL OR NEW.retired_by_change_id IS NULL
     OR (to_jsonb(NEW) - bookkeeping) IS DISTINCT FROM (to_jsonb(OLD) - bookkeeping) THEN
    RAISE EXCEPTION '%: a period is fixed; it can only be retired, once', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_center_period_fixed() OWNER TO erp_owner;
CREATE TRIGGER cost_center_period_fixed BEFORE UPDATE ON ledger.cost_center_period FOR EACH ROW EXECUTE FUNCTION ledger.check_center_period_fixed();
CREATE TRIGGER profit_center_period_fixed BEFORE UPDATE ON ledger.profit_center_period FOR EACH ROW EXECUTE FUNCTION ledger.check_center_period_fixed();

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.cost_center.read', 'View cost centers, profit centers, segments and their changes', false),
  ('ledger.cost_center.change', 'Maintain segments and center texts, and request center changes', false),
  ('ledger.cost_center.approve', 'Approve center changes: creation, validity, locks, profit center, segment and companies', true);
