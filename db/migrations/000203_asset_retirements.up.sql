-- Asset retirements (T57g; ADR-0057 §5, §6, §8; 03-data-model §3.21): a
-- sale with proceeds or a scrapping of a share of an asset, valued in its
-- posting period, posted per ledger the company keeps as one
-- ledger-specific entry (assets.document_entry) with the asset's movements
-- (a depreciation catch-up and the retirement), reversed while it is the
-- asset's latest posting. legal_entity_id is the tenancy context's: no
-- foreign key leaves the schema. Expand only: a new table and a new rule
-- on movements no previous version writes (a share is written only by
-- retirements and transfers, which no earlier version posts).

-- A retirement (§8): the share of cost given as a percentage or as a
-- cost in local currency (neither: all of it, a full retirement), the
-- proceeds of a sale in local currency (none for a scrapping), its value
-- date in its posting date's period (a regular period is a calendar
-- month, ADR-0057 as built T57e), on or before the posting date. seq is
-- the asset's posting order (§5). Posted, then reversed.
CREATE TABLE assets.retirement (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,      -- tenancy context
  asset_id               uuid NOT NULL,
  seq                    integer NOT NULL CHECK (seq >= 1),
  kind                   text NOT NULL CHECK (kind IN ('sale', 'scrapping')),
  value_date             date NOT NULL,
  posting_date           date NOT NULL,
  fiscal_year            smallint NOT NULL,  -- the posting date's
  fiscal_period          smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 12),
  currency               char(3) NOT NULL REFERENCES ref.currency,  -- the company's local currency
  share_percent          numeric CHECK (share_percent > 0 AND share_percent <= 100),
  share_cost             bigint CHECK (share_cost > 0),
  proceeds               bigint NOT NULL DEFAULT 0 CHECK (proceeds >= 0),
  currency_gc            char(3) NOT NULL REFERENCES ref.currency,  -- the tenant's group currency
  proceeds_gc            bigint NOT NULL DEFAULT 0,                 -- translated at the posting date's rate, the same in every ledger
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
  CONSTRAINT retirement_seq_once UNIQUE (tenant_id, asset_id, seq),
  CONSTRAINT retirement_share CHECK (share_percent IS NULL OR share_cost IS NULL),
  CONSTRAINT retirement_proceeds CHECK ((kind = 'sale' OR proceeds = 0) AND (proceeds <> 0 OR proceeds_gc = 0)),
  -- Not backdated (decided by default 13): valued in its posting period.
  CONSTRAINT retirement_value_date CHECK (value_date <= posting_date AND date_trunc('month', value_date::timestamp) = date_trunc('month', posting_date::timestamp)),
  CONSTRAINT retirement_reversal CHECK ((status = 'reversed') = (reversal_posting_date IS NOT NULL)
    AND (reversal_posting_date IS NULL) = (reversal_reason IS NULL)),
  CONSTRAINT retirement_reversal_date CHECK (reversal_posting_date >= posting_date)
);
CREATE TRIGGER retirement_touch BEFORE UPDATE ON assets.retirement FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE INDEX retirement_by_company ON assets.retirement (tenant_id, legal_entity_id, posting_date, id);
CREATE INDEX retirement_by_asset ON assets.retirement (tenant_id, asset_id, seq);
SELECT platform.secure_table('assets.retirement'::regclass);

-- A retirement starts posted and is reversed at most once; what it posted
-- never changes, nor its reversal once recorded.
CREATE FUNCTION assets.check_retirement() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'posted' THEN
      RAISE EXCEPTION 'a retirement starts posted, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'retirement_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status AND (OLD.status, NEW.status) <> ('posted', 'reversed') THEN
    RAISE EXCEPTION 'retirement % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'retirement_transition';
  END IF;
  IF (NEW.legal_entity_id, NEW.asset_id, NEW.seq, NEW.kind, NEW.value_date, NEW.posting_date, NEW.fiscal_year, NEW.fiscal_period, NEW.currency,
      NEW.share_percent, NEW.share_cost, NEW.proceeds, NEW.currency_gc, NEW.proceeds_gc, NEW.text, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.asset_id, OLD.seq, OLD.kind, OLD.value_date, OLD.posting_date, OLD.fiscal_year, OLD.fiscal_period,
      OLD.currency, OLD.share_percent, OLD.share_cost, OLD.proceeds, OLD.currency_gc, OLD.proceeds_gc, OLD.text, OLD.created_at, OLD.created_by)
      OR (OLD.status = 'reversed' AND (NEW.reversal_posting_date, NEW.reversal_reason) IS DISTINCT FROM (OLD.reversal_posting_date, OLD.reversal_reason)) THEN
    RAISE EXCEPTION 'retirement % keeps what it posted', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'retirement_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_retirement() OWNER TO erp_owner;
CREATE TRIGGER retirement_transition BEFORE INSERT OR UPDATE ON assets.retirement FOR EACH ROW EXECUTE FUNCTION assets.check_retirement();

-- The fraction of cost a retirement or transfer took (§5): above zero and
-- at most all of it.
ALTER TABLE assets.movement ADD CONSTRAINT movement_share CHECK (share > 0 AND share <= 1);
