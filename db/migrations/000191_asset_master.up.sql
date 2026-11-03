-- The asset master (T57b; ADR-0057 §2, §5, §7, §8; 03-data-model §3.21):
-- the assets context's schema with its assets, their depreciation terms
-- per ledger and their cost and profit center assignments. legal_entity_id
-- is the tenancy context's, ledger_id and the centers the ledger's: no
-- foreign key crosses a schema; the assets context checks them through its
-- ports. Every posting, reversal and assignment touching an asset takes
-- the asset's next seq (asset.last_seq) under its lock, so "posted after"
-- is a higher seq, never an id order (§5). Expand only: a new schema, which
-- the previous version neither reads nor writes.
CREATE SCHEMA assets;
GRANT USAGE ON SCHEMA assets TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('assets');
SELECT platform.secure_table('assets.outbox'::regclass);
SELECT platform.secure_table('assets.inbox'::regclass);
GRANT UPDATE (published_at) ON assets.outbox TO erp_app;
GRANT USAGE ON SCHEMA assets TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON assets.outbox TO erp_relay;

-- An asset (SAP ANLA): numbered from its company's asset range in the
-- fiscal year it is created, of a class of the configuration (which never
-- changes: a reclassification is a transfer to another asset). Its status
-- and capitalisation date are derived from its movements (T57c on); it
-- starts planned and only a planned asset is cancelled.
CREATE TABLE assets.asset (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,      -- tenancy context
  asset_number     text NOT NULL CHECK (btrim(asset_number) <> '' AND length(asset_number) <= 40),
  class_code       text NOT NULL CHECK (class_code ~ '^[A-Z0-9_]{1,20}$'),
  description      text NOT NULL CHECK (btrim(description) <> '' AND length(description) <= 200),
  status           text NOT NULL CHECK (status IN ('planned', 'active', 'retired', 'cancelled')),
  capitalised_on   date,
  last_seq         integer NOT NULL CHECK (last_seq >= 1),  -- the seq of the asset's latest posting, reversal or assignment
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT asset_number_once UNIQUE (tenant_id, legal_entity_id, asset_number),
  -- Capitalised exactly while it holds a posted acquisition or transfer in
  -- (active) or did (retired); planned and cancelled assets hold none.
  CONSTRAINT asset_capitalised CHECK ((status IN ('active', 'retired')) = (capitalised_on IS NOT NULL))
);
CREATE TRIGGER asset_touch BEFORE UPDATE ON assets.asset FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list reads a company's assets by class and status.
CREATE INDEX asset_by_class ON assets.asset (tenant_id, legal_entity_id, class_code, asset_number);
SELECT platform.secure_table('assets.asset'::regclass);

-- An asset starts planned, and moves planned -> active (its first
-- acquisition or transfer in) -> retired (all its cost gone by a
-- retirement or transfer out), back from retired to active and from active
-- to planned by reversals, and from planned to cancelled; a cancelled asset
-- is final. Its company, number, class and creation never change, and its
-- last_seq never goes back (§5).
CREATE FUNCTION assets.check_asset() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'planned' THEN
      RAISE EXCEPTION 'an asset starts planned, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status = 'cancelled' OR (NEW.status <> OLD.status AND (OLD.status, NEW.status) NOT IN
      (('planned', 'active'), ('planned', 'cancelled'), ('active', 'planned'), ('active', 'retired'), ('retired', 'active'))) THEN
    RAISE EXCEPTION 'asset % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_transition';
  END IF;
  IF (NEW.legal_entity_id, NEW.asset_number, NEW.class_code, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.asset_number, OLD.class_code, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'asset % keeps its company, number, class and creation', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_fixed';
  END IF;
  IF NEW.last_seq < OLD.last_seq THEN
    RAISE EXCEPTION 'asset %: last_seq goes from % back to %', OLD.id, OLD.last_seq, NEW.last_seq
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_seq_forward';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_asset() OWNER TO erp_owner;
CREATE TRIGGER asset_transition BEFORE INSERT OR UPDATE ON assets.asset FOR EACH ROW EXECUTE FUNCTION assets.check_asset();

-- An asset's depreciation terms in one ledger its company keeps (one
-- depreciation area, §7): from its class's defaults when it is created,
-- changeable until depreciation is first posted in that ledger
-- (booked_through set by a run or catch-up, T57f on); residual_value in
-- minor units of the company's local currency. depreciation_start null:
-- the asset's capitalisation date.
CREATE TABLE assets.asset_terms (
  tenant_id              uuid NOT NULL,
  asset_id               uuid NOT NULL,
  ledger_id              uuid NOT NULL,      -- ledger context
  method                 text NOT NULL CHECK (method IN ('straight_line', 'declining_balance')),
  useful_life_months     integer NOT NULL CHECK (useful_life_months BETWEEN 1 AND 1200),
  declining_factor       numeric(6,4) CHECK (declining_factor > 1 AND declining_factor <= 10),
  residual_value         bigint NOT NULL DEFAULT 0 CHECK (residual_value >= 0),
  depreciation_start     date,
  booked_through_year    smallint,
  booked_through_period  smallint CHECK (booked_through_period BETWEEN 1 AND 12),
  version                integer NOT NULL DEFAULT 1,
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, asset_id, ledger_id),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id),
  -- The declining balance method has a factor; straight-line has none.
  CONSTRAINT asset_terms_declining_factor CHECK ((method = 'declining_balance') = (declining_factor IS NOT NULL)),
  CONSTRAINT asset_terms_booked_through CHECK ((booked_through_year IS NULL) = (booked_through_period IS NULL))
);
CREATE TRIGGER asset_terms_touch BEFORE UPDATE ON assets.asset_terms FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('assets.asset_terms'::regclass);

-- Terms are fixed once depreciation is posted in their ledger (decided by
-- default 8): a change in estimate is out of scope (§1).
CREATE FUNCTION assets.check_asset_terms() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.booked_through_year IS NOT NULL AND (NEW.method, NEW.useful_life_months, NEW.declining_factor, NEW.residual_value, NEW.depreciation_start)
      IS DISTINCT FROM (OLD.method, OLD.useful_life_months, OLD.declining_factor, OLD.residual_value, OLD.depreciation_start) THEN
    RAISE EXCEPTION 'asset %: its terms in ledger % are fixed, depreciation having been posted', OLD.asset_id, OLD.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_terms_in_use';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_asset_terms() OWNER TO erp_owner;
CREATE TRIGGER asset_terms_in_use BEFORE UPDATE ON assets.asset_terms FOR EACH ROW EXECUTE FUNCTION assets.check_asset_terms();

-- An asset's cost center (optional) and profit center from valid_from on
-- (§8): the first row given at creation, valid from -infinity, each later
-- one from a center transfer (transfer_id, T57h) or its reversal.
-- Insert-only. Each row carries the asset's seq when it was written, so
-- the row with the highest seq names the asset's centers.
CREATE TABLE assets.asset_assignment (
  tenant_id         uuid NOT NULL,
  asset_id          uuid NOT NULL,
  seq               integer NOT NULL CHECK (seq >= 1),
  valid_from        date NOT NULL,
  cost_center_id    uuid,              -- ledger context
  profit_center_id  uuid,              -- ledger context
  transfer_id       uuid,              -- the center transfer that wrote it (T57h)
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, asset_id, seq),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id)
);
CREATE TRIGGER asset_assignment_insert_only BEFORE UPDATE OR DELETE ON assets.asset_assignment
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- The list filters assets by their latest row's cost center.
CREATE INDEX asset_assignment_by_cost_center ON assets.asset_assignment (tenant_id, cost_center_id) WHERE cost_center_id IS NOT NULL;
SELECT platform.secure_table('assets.asset_assignment'::regclass);

-- A row takes the asset's current last_seq, which its writer has just
-- raised under the asset's lock, so rows are written in seq order and the
-- latest row is the one with the highest seq. The first row is valid from
-- -infinity, every later one from a day.
CREATE FUNCTION assets.check_asset_assignment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE current integer; first boolean;
BEGIN
  SELECT a.last_seq INTO current FROM assets.asset a WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.asset_id;
  SELECT NOT EXISTS (SELECT FROM assets.asset_assignment r WHERE r.tenant_id = NEW.tenant_id AND r.asset_id = NEW.asset_id) INTO first;
  IF NEW.seq IS DISTINCT FROM current THEN
    RAISE EXCEPTION 'asset %: an assignment takes the asset''s last seq %, not %', NEW.asset_id, current, NEW.seq
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_seq';
  END IF;
  IF first <> (NEW.valid_from = '-infinity'::date) THEN
    RAISE EXCEPTION 'asset %: only its first assignment is valid from -infinity', NEW.asset_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_valid_from';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_asset_assignment() OWNER TO erp_owner;
CREATE TRIGGER asset_assignment_seq BEFORE INSERT ON assets.asset_assignment FOR EACH ROW EXECUTE FUNCTION assets.check_asset_assignment();

-- Creating, changing and cancelling assets (ADR-0057 §10); posting their
-- documents and running depreciation come with their slices.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('assets.asset.change', 'Create, change and cancel assets', false);
