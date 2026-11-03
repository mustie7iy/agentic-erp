-- Depreciation runs (T57f; ADR-0057 §5, §7; 03-data-model §3.21): a run
-- of a company, ledger and regular period, queued by the request with its
-- Operation and carried out as a workflow, and its batches of at most 500
-- assets, each posted as one journal entry in the run's ledger alone.
-- legal_entity_id is the tenancy context's, ledger_id, operation_id and the
-- journal entries the ledger's and the platform's: no foreign key leaves the
-- schema. Expand only: new tables the previous version neither reads nor
-- writes.

-- A run (§7): queued -> running (its batches planned) -> posted, or failed
-- with the problem that stopped it (error, the problem document). One run
-- of a company and ledger is queued or running at a time.
CREATE TABLE assets.depreciation_run (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,      -- tenancy context
  ledger_id        uuid NOT NULL,      -- ledger context
  fiscal_year      smallint NOT NULL,
  fiscal_period    smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 12),
  posting_date     date NOT NULL,      -- the period's last day
  currency         char(3) NOT NULL REFERENCES ref.currency,  -- the company's local currency
  status           text NOT NULL CHECK (status IN ('queued', 'running', 'posted', 'failed')),
  operation_id     uuid NOT NULL,      -- platform.operation
  assets_planned   integer CHECK (assets_planned >= 0),
  assets_posted    integer CHECK (assets_posted >= 0),
  assets_skipped   integer CHECK (assets_skipped >= 0),
  amount           bigint,             -- the depreciation posted, local currency
  error            jsonb,
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- Planned once running; its totals once posted; its problem once failed.
  CONSTRAINT depreciation_run_planned CHECK ((status IN ('queued', 'failed')) OR assets_planned IS NOT NULL),
  CONSTRAINT depreciation_run_totals CHECK ((status = 'posted') = (amount IS NOT NULL AND assets_posted IS NOT NULL AND assets_skipped IS NOT NULL)),
  CONSTRAINT depreciation_run_error CHECK ((status = 'failed') = (error IS NOT NULL))
);
CREATE TRIGGER depreciation_run_touch BEFORE UPDATE ON assets.depreciation_run FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- One run of a company and ledger at a time (assets.run_in_progress).
CREATE UNIQUE INDEX depreciation_run_one_at_a_time ON assets.depreciation_run (tenant_id, legal_entity_id, ledger_id)
  WHERE status IN ('queued', 'running');
-- The list, and the latest posted run's period (assets.run_out_of_order).
CREATE INDEX depreciation_run_by_company ON assets.depreciation_run (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period);
SELECT platform.secure_table('assets.depreciation_run'::regclass);

-- A run starts queued and moves queued -> running -> posted, or to failed
-- from queued or running; posted and failed are final. What it runs never
-- changes.
CREATE FUNCTION assets.check_depreciation_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'queued' THEN
      RAISE EXCEPTION 'a depreciation run starts queued, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'depreciation_run_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status AND (OLD.status, NEW.status) NOT IN
      (('queued', 'running'), ('running', 'posted'), ('queued', 'failed'), ('running', 'failed')) THEN
    RAISE EXCEPTION 'depreciation run % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'depreciation_run_transition';
  END IF;
  IF OLD.status IN ('posted', 'failed') OR (NEW.legal_entity_id, NEW.ledger_id, NEW.fiscal_year, NEW.fiscal_period, NEW.posting_date, NEW.currency,
      NEW.operation_id, NEW.created_at, NEW.created_by) IS DISTINCT FROM (OLD.legal_entity_id, OLD.ledger_id, OLD.fiscal_year, OLD.fiscal_period,
      OLD.posting_date, OLD.currency, OLD.operation_id, OLD.created_at, OLD.created_by)
      OR (OLD.assets_planned IS NOT NULL AND NEW.assets_planned IS DISTINCT FROM OLD.assets_planned) THEN
    RAISE EXCEPTION 'depreciation run % keeps what it runs, its plan and its outcome', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'depreciation_run_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_depreciation_run() OWNER TO erp_owner;
CREATE TRIGGER depreciation_run_transition BEFORE INSERT OR UPDATE ON assets.depreciation_run
  FOR EACH ROW EXECUTE FUNCTION assets.check_depreciation_run();

-- A run's batch (§7): at most 500 assets by asset number, pending until
-- posted in one transaction with its journal entry (none when no asset of
-- the batch had a difference), the depreciation it posted, how many of its
-- assets it posted and how many it skipped under their locks.
CREATE TABLE assets.depreciation_batch (
  tenant_id           uuid NOT NULL,
  run_id              uuid NOT NULL,
  batch_no            integer NOT NULL CHECK (batch_no >= 1),
  asset_ids           uuid[] NOT NULL CHECK (cardinality(asset_ids) BETWEEN 1 AND 500),
  status              text NOT NULL CHECK (status IN ('pending', 'posted')),
  amount              bigint,
  assets_posted       integer CHECK (assets_posted >= 0),
  assets_skipped      integer CHECK (assets_skipped >= 0),
  journal_entry_year  smallint,           -- ledger context
  journal_entry_id    uuid,
  document_number     text,
  version             integer NOT NULL DEFAULT 1,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, run_id, batch_no),
  FOREIGN KEY (tenant_id, run_id) REFERENCES assets.depreciation_run (tenant_id, id),
  CONSTRAINT depreciation_batch_posted CHECK ((status = 'posted') = (amount IS NOT NULL AND assets_posted IS NOT NULL AND assets_skipped IS NOT NULL)),
  CONSTRAINT depreciation_batch_entry CHECK ((journal_entry_id IS NULL) = (journal_entry_year IS NULL)
    AND (journal_entry_id IS NULL) = (document_number IS NULL) AND (journal_entry_id IS NULL OR status = 'posted'))
);
CREATE TRIGGER depreciation_batch_touch BEFORE UPDATE ON assets.depreciation_batch FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The movements list names a run's movements by their batch's entry.
CREATE INDEX depreciation_batch_by_entry ON assets.depreciation_batch (tenant_id, journal_entry_id) WHERE journal_entry_id IS NOT NULL;
SELECT platform.secure_table('assets.depreciation_batch'::regclass);

-- A batch starts pending and is posted once; its assets never change, nor
-- what it posted.
CREATE FUNCTION assets.check_depreciation_batch() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending' THEN
      RAISE EXCEPTION 'a depreciation batch starts pending, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'depreciation_batch_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status = 'posted' OR (NEW.status <> OLD.status AND (OLD.status, NEW.status) <> ('pending', 'posted'))
      OR NEW.asset_ids IS DISTINCT FROM OLD.asset_ids THEN
    RAISE EXCEPTION 'depreciation batch % of run % is posted once and keeps its assets', OLD.batch_no, OLD.run_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'depreciation_batch_transition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_depreciation_batch() OWNER TO erp_owner;
CREATE TRIGGER depreciation_batch_transition BEFORE INSERT OR UPDATE ON assets.depreciation_batch
  FOR EACH ROW EXECUTE FUNCTION assets.check_depreciation_batch();

-- Running depreciation (ADR-0057 §10).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('assets.depreciation.run', 'Start depreciation runs, which post depreciation per period and ledger', false);
