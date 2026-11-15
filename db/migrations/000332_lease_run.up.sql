-- The lease run (T88c; ADR-0073 §9; 03-data-model §3.33): per company and
-- fiscal period, each active lease contract posts its stored schedule
-- lines whose interval ends on or before the period's last day and that no
-- earlier run posted: the interest against the lease liability and the
-- right-of-use asset's depreciation against its accumulated depreciation,
-- on the contract's cost center. The three accounts are account
-- determination keys. Expand only (06 §6.4): two new tables, a wider key
-- check, an index and one permission, which the previous version neither
-- reads nor writes.

-- The accounts a lease run posts to (ADR-0073 §9): interest expense,
-- depreciation expense and accumulated depreciation of right-of-use
-- assets. The wider check replaces the narrower one NOT VALID, which
-- reads no row under the lock; every row satisfied the narrower check,
-- and 000333 validates the new one.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation')) NOT VALID;

-- A run reads a company's active contracts.
CREATE INDEX contract_by_company ON leases.contract (tenant_id, legal_entity_id, status, document_number, id);

-- A run of a company and a regular fiscal period: posting_date is the
-- period's last day, which the entries are dated. contract_count is the
-- active contracts read, posting_count those that posted; interest_amount
-- and depreciation_amount are what the run's entries posted, in minor
-- units of currency, the company's local currency. legal_entity_id is the
-- tenancy context's (02 §2.1).
CREATE TABLE leases.run (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,    -- tenancy context
  fiscal_year          smallint NOT NULL,
  fiscal_period        smallint NOT NULL CONSTRAINT run_period_check CHECK (fiscal_period > 0),
  posting_date         date NOT NULL,
  currency             char(3) NOT NULL REFERENCES ref.currency,
  contract_count       integer NOT NULL CONSTRAINT run_contract_count_check CHECK (contract_count >= 0),
  posting_count        integer NOT NULL,
  interest_amount      bigint NOT NULL,
  depreciation_amount  bigint NOT NULL CONSTRAINT run_depreciation_check CHECK (depreciation_amount >= 0),
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT run_posting_count_check CHECK (posting_count BETWEEN 0 AND contract_count),
  -- A run that posted nothing moved nothing.
  CONSTRAINT run_nothing_posted_check CHECK (posting_count > 0 OR (interest_amount = 0 AND depreciation_amount = 0)),
  -- One run per company and period (ADR-0073 §9).
  CONSTRAINT lease_run_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, fiscal_period)
);
-- The list: the tenant's runs, latest first, and a company's last run.
CREATE INDEX run_by_date ON leases.run (tenant_id, posting_date DESC, id);
CREATE INDEX run_by_company ON leases.run (tenant_id, legal_entity_id, posting_date DESC);
SELECT platform.secure_table('leases.run'::regclass);
-- A run is written once: it is not changed, reversed or deleted.
REVOKE UPDATE, DELETE ON leases.run FROM erp_app;

-- A company's runs follow one another: each is for a later period than
-- the company's last, since a run posts the lines the runs before it left.
-- The company's runs are held by an advisory lock from here to the
-- transaction's end, the one the application takes before it reads what
-- the last runs posted, so two runs of one company never read the same
-- state.
CREATE FUNCTION leases.check_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('leases.run:' || NEW.tenant_id::text || ':' || NEW.legal_entity_id::text, 0));
  IF EXISTS (SELECT FROM leases.run r WHERE r.tenant_id = NEW.tenant_id AND r.legal_entity_id = NEW.legal_entity_id
      AND r.posting_date >= NEW.posting_date AND (r.fiscal_year, r.fiscal_period) <> (NEW.fiscal_year, NEW.fiscal_period)) THEN
    RAISE EXCEPTION 'lease run of % for %/%: the company has a run for this day or a later one (lease_run_in_order)', NEW.legal_entity_id, NEW.fiscal_year, NEW.fiscal_period
      USING ERRCODE = 'check_violation', CONSTRAINT = 'lease_run_in_order';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION leases.check_run() OWNER TO erp_owner;
CREATE TRIGGER lease_run_in_order BEFORE INSERT ON leases.run
  FOR EACH ROW EXECUTE FUNCTION leases.check_run();

-- What a run posted for one contract (ADR-0073 §9): its schedule lines
-- first_line_no to last_line_no, the interest and the depreciation they
-- add up to, and the entry. id is the entry's source document.
-- journal_entry_id is the ledger context's (02 §2.1).
CREATE TABLE leases.run_posting (
  tenant_id             uuid NOT NULL,
  run_id                uuid NOT NULL,
  contract_id           uuid NOT NULL,
  id                    uuid NOT NULL,
  first_line_no         integer NOT NULL,
  last_line_no          integer NOT NULL,
  interest_amount       bigint NOT NULL,
  depreciation_amount   bigint NOT NULL CONSTRAINT run_posting_depreciation_check CHECK (depreciation_amount >= 0),
  journal_entry_id      uuid NOT NULL,    -- ledger context
  journal_entry_year    smallint NOT NULL,
  journal_entry_number  text NOT NULL CONSTRAINT run_posting_journal_entry_number_check CHECK (length(journal_entry_number) BETWEEN 1 AND 40),
  PRIMARY KEY (tenant_id, run_id, contract_id),
  CONSTRAINT run_posting_run_fkey FOREIGN KEY (tenant_id, run_id) REFERENCES leases.run (tenant_id, id),
  CONSTRAINT run_posting_contract_fkey FOREIGN KEY (tenant_id, contract_id) REFERENCES leases.contract (tenant_id, id),
  -- The lines posted are lines of the contract's stored schedule.
  CONSTRAINT run_posting_first_line_fkey FOREIGN KEY (tenant_id, contract_id, first_line_no) REFERENCES leases.schedule_line (tenant_id, contract_id, line_no),
  CONSTRAINT run_posting_last_line_fkey FOREIGN KEY (tenant_id, contract_id, last_line_no) REFERENCES leases.schedule_line (tenant_id, contract_id, line_no),
  CONSTRAINT run_posting_lines_check CHECK (first_line_no <= last_line_no),
  -- A posting moves something.
  CONSTRAINT run_posting_moves_check CHECK (interest_amount <> 0 OR depreciation_amount <> 0),
  CONSTRAINT run_posting_id_once UNIQUE (tenant_id, id)
);
-- A contract's postings, whose last line the next run starts after.
CREATE INDEX run_posting_by_contract ON leases.run_posting (tenant_id, contract_id, last_line_no);
SELECT platform.secure_table('leases.run_posting'::regclass);
REVOKE UPDATE, DELETE ON leases.run_posting FROM erp_app;

-- A schedule line is posted once: a contract's posting starts after the
-- last line its earlier postings posted. The contract's company's runs
-- are held by the advisory lock its run's insert took, so two postings of
-- one contract are never checked at once.
CREATE FUNCTION leases.check_run_posting() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('leases.run:' || NEW.tenant_id::text || ':' || k.legal_entity_id::text, 0))
    FROM leases.contract k WHERE k.tenant_id = NEW.tenant_id AND k.id = NEW.contract_id;
  IF EXISTS (SELECT FROM leases.run_posting p WHERE p.tenant_id = NEW.tenant_id AND p.contract_id = NEW.contract_id
      AND p.last_line_no >= NEW.first_line_no) THEN
    RAISE EXCEPTION 'lease run posting of contract % from line %: a line is posted once (lease_line_posted_once)', NEW.contract_id, NEW.first_line_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'lease_line_posted_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION leases.check_run_posting() OWNER TO erp_owner;
CREATE TRIGGER lease_line_posted_once BEFORE INSERT ON leases.run_posting
  FOR EACH ROW EXECUTE FUNCTION leases.check_run_posting();

-- Running lease accounting (ADR-0073 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('leases.run.post', 'Run lease accounting for a company and a period, which posts interest and depreciation to the ledger', false);
