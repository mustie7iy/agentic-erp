-- Statistical key figures and cycles whose receivers' shares come from
-- them (T60j; ADR-0064 As built T60j, plan-depth-3 §T60j). A key figure of
-- a company (employees, floor space) has values posted per cost center and
-- fiscal period; a cost center's value of a period is the sum of its
-- postings' values. A cycle's share basis says what its receivers' shares
-- are: fixed percentages, as before, or a key figure's values of the
-- period run, the receivers then holding no share of their own. The cost
-- centers are the ledger's: no foreign key crosses a schema; the
-- controlling context checks them through ledger.AssignmentBalances.
-- Expand only (06 §6.4): new tables, new columns with a constant default
-- or nullable, a NOT NULL dropped, and rules about values the previous
-- version never writes; the foreign key and the checks on the existing
-- table are added NOT VALID and validated by 000386. The previous version
-- reads a receiver's share as not null: a cycle by key figure defined
-- while it still runs is not read there until it is replaced, the minutes
-- of a rolling update (ADR-0064 As built T60j).

CREATE TABLE controlling.statistical_key_figure (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,     -- tenancy context
  code             text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,12}$'),
  name             text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
  unit             text NOT NULL CONSTRAINT statistical_key_figure_unit REFERENCES ref.unit_of_measure (code),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT statistical_key_figure_code_once UNIQUE (tenant_id, legal_entity_id, code),
  -- Postings and cycles name the key figure with its company.
  UNIQUE (tenant_id, id, legal_entity_id)
);
CREATE TRIGGER statistical_key_figure_touch BEFORE UPDATE ON controlling.statistical_key_figure FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('controlling.statistical_key_figure'::regclass);

-- A key figure keeps its company, code and unit: its values were posted
-- in that unit, and cycles and postings name it in that company.
CREATE FUNCTION controlling.check_statistical_key_figure() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.code, NEW.unit) IS DISTINCT FROM (OLD.legal_entity_id, OLD.code, OLD.unit) THEN
    RAISE EXCEPTION 'statistical key figure % keeps its company, code and unit', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'statistical_key_figure_identity_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION controlling.check_statistical_key_figure() OWNER TO erp_owner;
CREATE TRIGGER statistical_key_figure_identity BEFORE UPDATE ON controlling.statistical_key_figure
  FOR EACH ROW EXECUTE FUNCTION controlling.check_statistical_key_figure();

-- A posting of a key figure's values for a fiscal period of its company.
-- Insert-only: the table has no version, so the application is granted no
-- UPDATE (platform.secure_table), and never a DELETE; a correction is a
-- further posting.
CREATE TABLE controlling.statistical_key_figure_posting (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  key_figure_id    uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,     -- tenancy context
  fiscal_year      smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  fiscal_period    smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT statistical_key_figure_posting_key_figure FOREIGN KEY (tenant_id, key_figure_id, legal_entity_id)
    REFERENCES controlling.statistical_key_figure (tenant_id, id, legal_entity_id)
);
-- The postings of a key figure's period, which its values sum.
CREATE INDEX statistical_key_figure_posting_by_period ON controlling.statistical_key_figure_posting (tenant_id, key_figure_id, fiscal_year, fiscal_period);
SELECT platform.secure_table('controlling.statistical_key_figure_posting'::regclass);

-- A posting's value for a cost center, once per posting, not zero, with
-- up to six decimals; a negative value corrects an earlier one.
-- Insert-only, as its posting.
CREATE TABLE controlling.statistical_key_figure_value (
  tenant_id       uuid NOT NULL,
  posting_id      uuid NOT NULL,
  position        smallint NOT NULL CHECK (position BETWEEN 1 AND 500),
  cost_center_id  uuid NOT NULL,      -- ledger context
  value           numeric(20,6) NOT NULL CONSTRAINT statistical_key_figure_value_not_zero CHECK (value <> 0),
  PRIMARY KEY (tenant_id, posting_id, position),
  CONSTRAINT statistical_key_figure_value_once UNIQUE (tenant_id, posting_id, cost_center_id),
  CONSTRAINT statistical_key_figure_value_posting FOREIGN KEY (tenant_id, posting_id) REFERENCES controlling.statistical_key_figure_posting (tenant_id, id)
);
SELECT platform.secure_table('controlling.statistical_key_figure_value'::regclass);

-- A cycle's share basis, fixed percentages for every cycle before, and
-- exactly with a key figure basis its key figure, one of the cycle's
-- company.
ALTER TABLE controlling.allocation_cycle ADD COLUMN share_basis text NOT NULL DEFAULT 'fixed_percentages',
  ADD COLUMN statistical_key_figure_id uuid;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_share_basis
  CHECK (share_basis IN ('fixed_percentages', 'statistical_key_figure')) NOT VALID;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_key_figure
  CHECK ((share_basis = 'statistical_key_figure') = (statistical_key_figure_id IS NOT NULL)) NOT VALID;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_key_figure_company FOREIGN KEY (tenant_id, statistical_key_figure_id, legal_entity_id)
  REFERENCES controlling.statistical_key_figure (tenant_id, id, legal_entity_id) NOT VALID;

-- A cycle keeps its share basis and key figure too (000284's function
-- with them).
CREATE OR REPLACE FUNCTION controlling.check_allocation_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.code, NEW.kind, NEW.sender_cost_center_id, NEW.assessment_account_id, NEW.share_basis, NEW.statistical_key_figure_id)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.code, OLD.kind, OLD.sender_cost_center_id, OLD.assessment_account_id, OLD.share_basis,
        OLD.statistical_key_figure_id) THEN
    RAISE EXCEPTION 'allocation cycle % keeps its company, code, kind, sender, assessment account, share basis and key figure', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_cycle_identity_kept';
  END IF;
  RETURN NEW;
END $$;

-- A receiver of a cycle by key figure holds no share: its share is its
-- value of the period run.
ALTER TABLE controlling.allocation_receiver ALTER COLUMN share_bp DROP NOT NULL;

-- 000283's rule by the cycle's basis: with fixed percentages every
-- receiver has its share and the shares add up to the whole, as before;
-- with a key figure no receiver has one and the cycle has a receiver.
-- No receiver is the sender, whatever the basis.
CREATE OR REPLACE FUNCTION controlling.allocation_shares_whole(tenant uuid, cycle uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  total bigint;
  shared bigint;
  receivers bigint;
  sender uuid;
  basis text;
BEGIN
  SELECT sender_cost_center_id, share_basis INTO sender, basis FROM controlling.allocation_cycle WHERE tenant_id = tenant AND id = cycle;
  SELECT coalesce(sum(share_bp), 0), count(share_bp), count(*) INTO total, shared, receivers
    FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle;
  IF basis = 'fixed_percentages' AND (total <> 10000 OR shared <> receivers) THEN
    RAISE EXCEPTION 'the shares of allocation cycle % add up to % basis points, not 10000, or a receiver has none', cycle, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_whole';
  END IF;
  IF basis <> 'fixed_percentages' AND (shared <> 0 OR receivers = 0) THEN
    RAISE EXCEPTION 'allocation cycle % takes its shares from %, so it has receivers and none has a share of its own', cycle, basis
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_by_basis';
  END IF;
  IF EXISTS (SELECT 1 FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle AND cost_center_id = sender) THEN
    RAISE EXCEPTION 'allocation cycle % names its sender as a receiver', cycle
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_not_sender';
  END IF;
END $$;

-- Reading key figures and their values, defining one and posting its
-- values: a person's, by permission, with no approval request.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.key_figure.read', 'View statistical key figures and their values', false),
  ('controlling.key_figure.write', 'Define statistical key figures and post their values', false);
