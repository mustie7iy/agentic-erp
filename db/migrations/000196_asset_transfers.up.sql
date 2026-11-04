-- Asset transfers (T57h; ADR-0057 §5, §6, §8; 03-data-model §3.21): a
-- center transfer moves an asset to another cost or profit center from the
-- first day of its posting period, catching its depreciation up through
-- the period before on its previous centers and, for a profit center
-- change, reposting its booked balances; an asset transfer moves a share of
-- an asset's balances to another asset of the same company. Both post one
-- ledger-specific entry per ledger the company keeps (a center transfer's
-- catch-up its own entry in the period before when that period is open,
-- assets.document_entry's part catch_up) and are reversed while they are
-- the latest posting of every asset they touch. A center transfer writes
-- an assignment row, and its reversal a row restoring the previous one's
-- centers and valid_from (restores_seq). legal_entity_id and the centers
-- are the tenancy and ledger contexts': no foreign key leaves the schema.
-- Expand only: a new table, and a new nullable column and rule on
-- assignments that no previous version writes or breaks.

-- A transfer (§8): kind center, to the cost center (with the profit center
-- derived from it) or profit center named, valued on its posting period's
-- first day, with the centers it moved from; or kind asset, to another
-- asset (to_asset_id, its seq to_seq) of the share of cost given as a
-- percentage or as a cost in local currency (neither: all of it), valued
-- in its posting period on or before its posting date (a regular period is
-- a calendar month, ADR-0057 as built T57e). seq is the asset's posting
-- order (§5). Posted, then reversed.
CREATE TABLE assets.transfer (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,      -- tenancy context
  asset_id               uuid NOT NULL,
  seq                    integer NOT NULL CHECK (seq >= 1),
  kind                   text NOT NULL CHECK (kind IN ('center', 'asset')),
  value_date             date NOT NULL,
  posting_date           date NOT NULL,
  fiscal_year            smallint NOT NULL,  -- the posting date's
  fiscal_period          smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 12),
  currency               char(3) NOT NULL REFERENCES ref.currency,  -- the company's local currency
  currency_gc            char(3) NOT NULL REFERENCES ref.currency,  -- the tenant's group currency
  to_asset_id            uuid,               -- an asset transfer's receiver
  to_seq                 integer CHECK (to_seq >= 1),
  share_percent          numeric CHECK (share_percent > 0 AND share_percent <= 100),
  share_cost             bigint CHECK (share_cost > 0),
  from_cost_center_id    uuid,               -- ledger context: a center transfer's previous centers
  from_profit_center_id  uuid,
  cost_center_id         uuid,               -- ledger context: a center transfer's new centers
  profit_center_id       uuid,
  text                   text CHECK (length(text) <= 200),
  status                 text NOT NULL CHECK (status IN ('posted', 'reversed')),
  reversal_posting_date  date,
  reversal_reason        text CHECK (length(reversal_reason) BETWEEN 1 AND 2000),
  version                integer NOT NULL DEFAULT 1,
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id),
  FOREIGN KEY (tenant_id, to_asset_id) REFERENCES assets.asset (tenant_id, id),
  CONSTRAINT transfer_seq_once UNIQUE (tenant_id, asset_id, seq),
  CONSTRAINT transfer_to_seq_once UNIQUE (tenant_id, to_asset_id, to_seq),
  -- An asset transfer names its receiver and a share, never centers; a
  -- center transfer names centers, never a receiver or a share.
  CONSTRAINT transfer_kind CHECK (CASE kind
    WHEN 'asset' THEN to_asset_id IS NOT NULL AND to_seq IS NOT NULL AND cost_center_id IS NULL AND profit_center_id IS NULL
      AND from_cost_center_id IS NULL AND from_profit_center_id IS NULL
    ELSE to_asset_id IS NULL AND to_seq IS NULL AND share_percent IS NULL AND share_cost IS NULL END),
  CONSTRAINT transfer_same_asset CHECK (to_asset_id <> asset_id),
  CONSTRAINT transfer_share CHECK (share_percent IS NULL OR share_cost IS NULL),
  -- Not backdated (decided by default 13): valued in its posting period, a
  -- center transfer on its first day (decided by default 17).
  CONSTRAINT transfer_value_date CHECK (value_date <= posting_date
    AND date_trunc('month', value_date::timestamp) = date_trunc('month', posting_date::timestamp)
    AND (kind <> 'center' OR value_date = date_trunc('month', posting_date::timestamp)::date)),
  CONSTRAINT transfer_reversal CHECK ((status = 'reversed') = (reversal_posting_date IS NOT NULL)
    AND (reversal_posting_date IS NULL) = (reversal_reason IS NULL)),
  CONSTRAINT transfer_reversal_date CHECK (reversal_posting_date >= posting_date)
);
CREATE TRIGGER transfer_touch BEFORE UPDATE ON assets.transfer FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE INDEX transfer_by_company ON assets.transfer (tenant_id, legal_entity_id, posting_date, id);
CREATE INDEX transfer_by_asset ON assets.transfer (tenant_id, asset_id, seq);
-- An asset's capitalisation date reads its posted transfers in (§5).
CREATE INDEX transfer_by_receiver ON assets.transfer (tenant_id, to_asset_id, to_seq) WHERE to_asset_id IS NOT NULL;
SELECT platform.secure_table('assets.transfer'::regclass);

-- A transfer starts posted and is reversed at most once; what it posted
-- never changes, nor its reversal once recorded.
CREATE FUNCTION assets.check_transfer() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'posted' THEN
      RAISE EXCEPTION 'a transfer starts posted, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'transfer_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status AND (OLD.status, NEW.status) <> ('posted', 'reversed') THEN
    RAISE EXCEPTION 'transfer % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'transfer_transition';
  END IF;
  IF (NEW.legal_entity_id, NEW.asset_id, NEW.seq, NEW.kind, NEW.value_date, NEW.posting_date, NEW.fiscal_year, NEW.fiscal_period, NEW.currency,
      NEW.currency_gc, NEW.to_asset_id, NEW.to_seq, NEW.share_percent, NEW.share_cost, NEW.from_cost_center_id, NEW.from_profit_center_id,
      NEW.cost_center_id, NEW.profit_center_id, NEW.text, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.asset_id, OLD.seq, OLD.kind, OLD.value_date, OLD.posting_date, OLD.fiscal_year, OLD.fiscal_period,
      OLD.currency, OLD.currency_gc, OLD.to_asset_id, OLD.to_seq, OLD.share_percent, OLD.share_cost, OLD.from_cost_center_id,
      OLD.from_profit_center_id, OLD.cost_center_id, OLD.profit_center_id, OLD.text, OLD.created_at, OLD.created_by)
      OR (OLD.status = 'reversed' AND (NEW.reversal_posting_date, NEW.reversal_reason) IS DISTINCT FROM (OLD.reversal_posting_date, OLD.reversal_reason)) THEN
    RAISE EXCEPTION 'transfer % keeps what it posted', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'transfer_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_transfer() OWNER TO erp_owner;
CREATE TRIGGER transfer_transition BEFORE INSERT OR UPDATE ON assets.transfer FOR EACH ROW EXECUTE FUNCTION assets.check_transfer();

-- A center transfer's reversal restores the previous centers with the
-- previous row's valid_from (§8), -infinity for the asset's first row:
-- restores_seq names that row, whose centers and valid_from it repeats.
ALTER TABLE assets.asset_assignment ADD COLUMN restores_seq integer,
  ADD FOREIGN KEY (tenant_id, asset_id, restores_seq) REFERENCES assets.asset_assignment (tenant_id, asset_id, seq);

-- A row takes the asset's current last_seq, which its writer has just
-- raised under the asset's lock, so rows are written in seq order and the
-- latest row is the one with the highest seq. The first row is valid from
-- -infinity, every later one from a day, except a row restoring an
-- earlier one, which repeats that row's centers and valid_from.
CREATE OR REPLACE FUNCTION assets.check_asset_assignment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE current integer; first boolean; restored assets.asset_assignment;
BEGIN
  SELECT a.last_seq INTO current FROM assets.asset a WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.asset_id;
  SELECT NOT EXISTS (SELECT FROM assets.asset_assignment r WHERE r.tenant_id = NEW.tenant_id AND r.asset_id = NEW.asset_id) INTO first;
  IF NEW.seq IS DISTINCT FROM current THEN
    RAISE EXCEPTION 'asset %: an assignment takes the asset''s last seq %, not %', NEW.asset_id, current, NEW.seq
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_seq';
  END IF;
  IF NEW.restores_seq IS NOT NULL THEN
    SELECT * INTO restored FROM assets.asset_assignment r WHERE r.tenant_id = NEW.tenant_id AND r.asset_id = NEW.asset_id AND r.seq = NEW.restores_seq;
    IF first OR NEW.restores_seq >= NEW.seq OR (NEW.valid_from, NEW.cost_center_id, NEW.profit_center_id)
        IS DISTINCT FROM (restored.valid_from, restored.cost_center_id, restored.profit_center_id) THEN
      RAISE EXCEPTION 'asset %: a restoring assignment repeats the centers and valid_from of the earlier row % it restores', NEW.asset_id, NEW.restores_seq
        USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_restores';
    END IF;
    RETURN NEW;
  END IF;
  IF first <> (NEW.valid_from = '-infinity'::date) THEN
    RAISE EXCEPTION 'asset %: only its first assignment is valid from -infinity', NEW.asset_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_valid_from';
  END IF;
  RETURN NEW;
END $$;
