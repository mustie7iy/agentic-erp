-- The revenue recognition run (T81d; ADR-0068 §9; 03-data-model §3.30):
-- per company and fiscal period, each active revenue contract is brought
-- to its cumulative position, revenue recognized to the period's end
-- less revenue billed, as one contract asset or one contract liability,
-- posted through the ledger. A run keeps, per contract it posted, the
-- cumulative amounts and the entry, and per obligation the amounts the
-- next run takes off. The two accounts are account determination keys.
-- Expand only (06 §6.4): three new tables, two wider checks and one
-- permission, which the previous version neither reads nor writes.

-- The contract asset and the contract liability account of a company,
-- applied from companies[].account_determination. NOT VALID: the next
-- migration validates it without blocking writes.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings',
                 'contract_asset', 'contract_liability')) NOT VALID;

-- The revenue context posts journal entries (through ledger.Postings
-- with source_context revenue), so the journal's list of source contexts
-- gains it, as it gained controlling (000280): the wider check replaces
-- the narrower one NOT VALID, which reads no row under the lock; every
-- row there satisfied the narrower check, and the next migration
-- validates the new one partition by partition.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling', 'revenue')) NOT VALID;

-- A run of a company and a regular fiscal period: posting_date is the
-- period's last day, which revenue is recognized to and the entries are
-- dated. contract_count is the active contracts read, posting_count
-- those that moved and were posted; the amounts are the company's totals
-- to the period's end over the contracts read, in minor units of
-- currency, the company's local currency. legal_entity_id is the
-- tenancy context's (02 §2.1).
CREATE TABLE revenue.recognition_run (
  tenant_id                  uuid NOT NULL,
  id                         uuid NOT NULL,
  legal_entity_id            uuid NOT NULL,    -- tenancy context
  fiscal_year                smallint NOT NULL,
  fiscal_period              smallint NOT NULL CONSTRAINT recognition_run_period_check CHECK (fiscal_period > 0),
  posting_date               date NOT NULL,
  currency                   char(3) NOT NULL REFERENCES ref.currency,
  contract_count             integer NOT NULL CONSTRAINT recognition_run_contract_count_check CHECK (contract_count >= 0),
  posting_count              integer NOT NULL,
  recognized_amount          bigint NOT NULL CONSTRAINT recognition_run_recognized_check CHECK (recognized_amount >= 0),
  billed_amount              bigint NOT NULL CONSTRAINT recognition_run_billed_check CHECK (billed_amount >= 0),
  contract_asset_amount      bigint NOT NULL CONSTRAINT recognition_run_asset_check CHECK (contract_asset_amount >= 0),
  contract_liability_amount  bigint NOT NULL CONSTRAINT recognition_run_liability_check CHECK (contract_liability_amount >= 0),
  created_at                 timestamptz NOT NULL DEFAULT now(),
  created_by                 uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT recognition_run_posting_count_check CHECK (posting_count BETWEEN 0 AND contract_count),
  -- The contracts' positions added up are what was recognized less what
  -- was billed.
  CONSTRAINT recognition_run_positions_check CHECK (contract_asset_amount - contract_liability_amount = recognized_amount - billed_amount),
  -- One run per company and period (ADR-0068 §9).
  CONSTRAINT recognition_run_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, fiscal_period)
);
-- The list: the tenant's runs, latest first, and a company's last run.
CREATE INDEX recognition_run_by_date ON revenue.recognition_run (tenant_id, posting_date DESC, id);
CREATE INDEX recognition_run_by_company ON revenue.recognition_run (tenant_id, legal_entity_id, posting_date DESC);
SELECT platform.secure_table('revenue.recognition_run'::regclass);
-- A run is written once: it is not changed, reversed or deleted.
REVOKE UPDATE, DELETE ON revenue.recognition_run FROM erp_app;

-- A company's runs follow one another: each is for a later period than
-- the company's last, since the amounts are cumulative and a run takes
-- off what the runs before it posted. The company's runs are held by an
-- advisory lock from here to the transaction's end, the one the
-- application takes before it reads the last run's postings, so two runs
-- of one company never read the same state.
CREATE FUNCTION revenue.check_recognition_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('revenue.recognition:' || NEW.tenant_id::text || ':' || NEW.legal_entity_id::text, 0));
  IF EXISTS (SELECT FROM revenue.recognition_run r WHERE r.tenant_id = NEW.tenant_id AND r.legal_entity_id = NEW.legal_entity_id
      AND r.posting_date >= NEW.posting_date AND (r.fiscal_year, r.fiscal_period) <> (NEW.fiscal_year, NEW.fiscal_period)) THEN
    RAISE EXCEPTION 'revenue recognition run of % for %/%: the company has a run for this day or a later one', NEW.legal_entity_id, NEW.fiscal_year, NEW.fiscal_period
      USING ERRCODE = 'check_violation', CONSTRAINT = 'recognition_run_in_order';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION revenue.check_recognition_run() OWNER TO erp_owner;
CREATE TRIGGER recognition_run_in_order BEFORE INSERT ON revenue.recognition_run
  FOR EACH ROW EXECUTE FUNCTION revenue.check_recognition_run();

-- What a run posted for one contract (ADR-0068 §9): the contract's
-- cumulative revenue recognized and billed to the run's posting date,
-- what the entry moved its revenue by, and the entry. id is the entry's
-- source document. The contract's position is recognized less billed: a
-- contract asset when positive, a contract liability when negative.
-- journal_entry_id is the ledger context's (02 §2.1).
CREATE TABLE revenue.recognition_posting (
  tenant_id           uuid NOT NULL,
  run_id              uuid NOT NULL,
  contract_id         uuid NOT NULL,
  id                  uuid NOT NULL,
  recognized_amount   bigint NOT NULL CONSTRAINT recognition_posting_recognized_check CHECK (recognized_amount >= 0),
  billed_amount       bigint NOT NULL CONSTRAINT recognition_posting_billed_check CHECK (billed_amount >= 0),
  revenue_amount      bigint NOT NULL,
  journal_entry_id    uuid NOT NULL,    -- ledger context
  journal_entry_year  smallint NOT NULL,
  document_number     text NOT NULL CONSTRAINT recognition_posting_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  PRIMARY KEY (tenant_id, run_id, contract_id),
  FOREIGN KEY (tenant_id, run_id) REFERENCES revenue.recognition_run (tenant_id, id),
  FOREIGN KEY (tenant_id, contract_id) REFERENCES revenue.contract (tenant_id, id),
  CONSTRAINT recognition_posting_id_once UNIQUE (tenant_id, id)
);
-- A contract's last posting, which the next run takes off.
CREATE INDEX recognition_posting_by_contract ON revenue.recognition_posting (tenant_id, contract_id);
SELECT platform.secure_table('revenue.recognition_posting'::regclass);
REVOKE UPDATE, DELETE ON revenue.recognition_posting FROM erp_app;

-- A posting's obligations: per obligation of the contract its cumulative
-- revenue recognized and billed to the run's posting date, what the
-- entry moved its revenue account by, and that account, the order
-- item's (ledger context). The next run posts the change of recognized
-- less billed since.
CREATE TABLE revenue.recognition_posting_obligation (
  tenant_id          uuid NOT NULL,
  run_id             uuid NOT NULL,
  contract_id        uuid NOT NULL,
  obligation_no      integer NOT NULL,
  account_id         uuid NOT NULL,    -- ledger context
  recognized_amount  bigint NOT NULL CONSTRAINT recognition_posting_obligation_recognized_check CHECK (recognized_amount >= 0),
  billed_amount      bigint NOT NULL CONSTRAINT recognition_posting_obligation_billed_check CHECK (billed_amount >= 0),
  revenue_amount     bigint NOT NULL,
  PRIMARY KEY (tenant_id, run_id, contract_id, obligation_no),
  FOREIGN KEY (tenant_id, run_id, contract_id) REFERENCES revenue.recognition_posting (tenant_id, run_id, contract_id),
  FOREIGN KEY (tenant_id, contract_id, obligation_no) REFERENCES revenue.performance_obligation (tenant_id, contract_id, obligation_no)
);
SELECT platform.secure_table('revenue.recognition_posting_obligation'::regclass);
REVOKE UPDATE, DELETE ON revenue.recognition_posting_obligation FROM erp_app;

-- Running revenue recognition (ADR-0068 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('revenue.recognition.run', 'Run revenue recognition for a company and a period, which posts to the ledger', false);
