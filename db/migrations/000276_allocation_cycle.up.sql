-- Distribution cycles and their runs (T60f; ADR-0064 §5,
-- plan-breadth-week1 §T60f). A cycle of a company has one sender cost
-- center and receiver cost centers with fixed shares adding up to the
-- whole; a run of the cycle for a fiscal period moves the sender's primary
-- costs of the period to the receivers on their original accounts, in one
-- journal entry through the ledger's posting port (source context
-- controlling). The centers and the entry are the ledger's: no foreign
-- key crosses a schema; the controlling context checks the centers
-- through ledger.AssignmentBalances. Expand only: new tables, which the
-- previous version neither reads nor writes.
--
-- kind is the cycle's way of charging: distribution alone here. An
-- assessment (T60g) adds its kind and the assessment account to this
-- table; receivers and runs are shared by both.
CREATE TABLE controlling.allocation_cycle (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,     -- tenancy context
  code                   text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,12}$'),
  name                   text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
  kind                   text NOT NULL CHECK (kind IN ('distribution')),
  sender_cost_center_id  uuid NOT NULL,     -- ledger context
  version                integer NOT NULL DEFAULT 1,
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT allocation_cycle_code_once UNIQUE (tenant_id, legal_entity_id, code),
  -- Runs name their cycle with its company.
  UNIQUE (tenant_id, id, legal_entity_id)
);
CREATE TRIGGER allocation_cycle_touch BEFORE UPDATE ON controlling.allocation_cycle FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('controlling.allocation_cycle'::regclass);

-- A receiver of a cycle: a cost center other than the sender, once, with
-- its share in basis points (a percentage with two decimals) and its
-- place in the cycle's list. Insert-only: the table has no version, so
-- the application is granted no UPDATE and never a DELETE.
CREATE TABLE controlling.allocation_receiver (
  tenant_id       uuid NOT NULL,
  cycle_id        uuid NOT NULL,
  position        smallint NOT NULL CHECK (position BETWEEN 1 AND 100),
  cost_center_id  uuid NOT NULL,            -- ledger context
  share_bp        integer NOT NULL CHECK (share_bp BETWEEN 1 AND 10000),
  PRIMARY KEY (tenant_id, cycle_id, position),
  CONSTRAINT allocation_receiver_once UNIQUE (tenant_id, cycle_id, cost_center_id),
  CONSTRAINT allocation_receiver_cycle FOREIGN KEY (tenant_id, cycle_id) REFERENCES controlling.allocation_cycle (tenant_id, id)
);
SELECT platform.secure_table('controlling.allocation_receiver'::regclass);

-- A cycle keeps its company, code, kind and sender, and its receivers'
-- shares add up to the whole and name no sender: checked when the
-- transaction that wrote the cycle or a receiver commits, so a cycle and
-- its receivers are written together in any order.
CREATE FUNCTION controlling.check_allocation_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.code, NEW.kind, NEW.sender_cost_center_id) IS DISTINCT FROM (OLD.legal_entity_id, OLD.code, OLD.kind, OLD.sender_cost_center_id) THEN
    RAISE EXCEPTION 'allocation cycle % keeps its company, code, kind and sender', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_cycle_identity_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION controlling.check_allocation_cycle() OWNER TO erp_owner;
CREATE TRIGGER allocation_cycle_identity BEFORE UPDATE ON controlling.allocation_cycle
  FOR EACH ROW EXECUTE FUNCTION controlling.check_allocation_cycle();

CREATE FUNCTION controlling.allocation_shares_whole(tenant uuid, cycle uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  total bigint;
  sender uuid;
BEGIN
  SELECT sender_cost_center_id INTO sender FROM controlling.allocation_cycle WHERE tenant_id = tenant AND id = cycle;
  SELECT coalesce(sum(share_bp), 0) INTO total FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle;
  IF total <> 10000 THEN
    RAISE EXCEPTION 'the shares of allocation cycle % add up to % basis points, not 10000', cycle, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_whole';
  END IF;
  IF EXISTS (SELECT 1 FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle AND cost_center_id = sender) THEN
    RAISE EXCEPTION 'allocation cycle % names its sender as a receiver', cycle
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_not_sender';
  END IF;
END $$;
ALTER FUNCTION controlling.allocation_shares_whole(uuid, uuid) OWNER TO erp_owner;
CREATE FUNCTION controlling.check_allocation_cycle_shares() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM controlling.allocation_shares_whole(NEW.tenant_id, NEW.id);
  RETURN NULL;
END $$;
ALTER FUNCTION controlling.check_allocation_cycle_shares() OWNER TO erp_owner;
CREATE FUNCTION controlling.check_allocation_receiver_shares() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM controlling.allocation_shares_whole(NEW.tenant_id, NEW.cycle_id);
  RETURN NULL;
END $$;
ALTER FUNCTION controlling.check_allocation_receiver_shares() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER allocation_cycle_shares AFTER INSERT ON controlling.allocation_cycle
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION controlling.check_allocation_cycle_shares();
CREATE CONSTRAINT TRIGGER allocation_receiver_shares AFTER INSERT ON controlling.allocation_receiver
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION controlling.check_allocation_receiver_shares();

-- A run of a cycle for a fiscal period: the entry it posted, the amount
-- it moved off the sender in minor units of the company's local currency
-- and the number of accounts. One run per cycle and period: a run is not
-- reversed in the first version. Insert-only.
CREATE TABLE controlling.allocation_run (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  cycle_id                 uuid NOT NULL,
  legal_entity_id          uuid NOT NULL,     -- tenancy context
  fiscal_year              smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  fiscal_period            smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  posting_date             date NOT NULL,
  journal_entry_id         uuid NOT NULL,     -- ledger context
  document_number          text NOT NULL CHECK (document_number <> ''),
  amount                   bigint NOT NULL,   -- local currency, minor units: the sender's costs moved, debit positive
  account_count            integer NOT NULL CHECK (account_count > 0),
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT allocation_run_cycle FOREIGN KEY (tenant_id, cycle_id, legal_entity_id)
    REFERENCES controlling.allocation_cycle (tenant_id, id, legal_entity_id),
  CONSTRAINT allocation_run_once UNIQUE (tenant_id, cycle_id, fiscal_year, fiscal_period),
  CONSTRAINT allocation_run_entry_once UNIQUE (tenant_id, journal_entry_id)
);
-- The runs of a company, latest first (GET /v1/allocation-runs).
CREATE INDEX allocation_run_by_company ON controlling.allocation_run (tenant_id, legal_entity_id, posting_date, id);
SELECT platform.secure_table('controlling.allocation_run'::regclass);

-- Reading cycles and runs, defining a cycle and running one (ADR-0064
-- §5): a person's, by permission, with no approval request.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.allocation.read', 'View allocation cycles and their runs', false),
  ('controlling.allocation.write', 'Define allocation cycles', false),
  ('controlling.allocation.run', 'Run allocation cycles', false);
