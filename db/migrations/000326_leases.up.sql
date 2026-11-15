-- Lease accounting (T88a; ADR-0073 §1-§5; 02 §2.2, 03-data-model §3.33):
-- the leases context's schema with its outbox and inbox, the lease
-- contract a company enters as lessee and its payment schedule, written
-- when the contract is created. A contract is a draft and posts nothing
-- (T88b activates it). legal_entity_id is the tenancy context's,
-- lessor_id the partner context's and cost_center_id the ledger's: no
-- foreign key leaves the schema (02 §2.1). Expand only: a new schema and
-- two permissions, which the previous version neither reads nor writes
-- (06 §6.4).
CREATE SCHEMA leases;
GRANT USAGE ON SCHEMA leases TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('leases');
SELECT platform.secure_table('leases.outbox'::regclass);
SELECT platform.secure_table('leases.inbox'::regclass);
GRANT UPDATE (published_at) ON leases.outbox TO erp_app;
GRANT USAGE ON SCHEMA leases TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON leases.outbox TO erp_relay;

-- A lease contract (ADR-0073 §3): a company leases from a lessor, from
-- the commencement date for term_months, paying payment_amount (minor
-- units of currency, the company's local currency) every month, quarter
-- or year, at the start (advance) or the end (arrears) of each interval.
-- annual_rate is the discount rate a person chose, a percentage kept as
-- an exact decimal. A frequency divides the term into whole intervals.
-- document_number is drawn from the company's lease_contract range.
CREATE TABLE leases.contract (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  document_number    text NOT NULL CONSTRAINT contract_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id    uuid NOT NULL,    -- tenancy context
  lessor_id          uuid NOT NULL,    -- partner context: a supplier
  commencement_date  date NOT NULL,
  term_months        integer NOT NULL CONSTRAINT contract_term_months_check CHECK (term_months BETWEEN 1 AND 600),
  payment_amount     bigint NOT NULL CONSTRAINT contract_payment_amount_positive CHECK (payment_amount > 0),
  currency           char(3) NOT NULL REFERENCES ref.currency,
  payment_frequency  text NOT NULL CONSTRAINT contract_payment_frequency_check CHECK (payment_frequency IN ('monthly', 'quarterly', 'yearly')),
  payment_timing     text NOT NULL CONSTRAINT contract_payment_timing_check CHECK (payment_timing IN ('advance', 'arrears')),
  annual_rate        numeric(9, 6) NOT NULL CONSTRAINT contract_annual_rate_check CHECK (annual_rate > 0 AND annual_rate <= 100),
  cost_center_id     uuid NOT NULL,    -- ledger context
  status             text NOT NULL CONSTRAINT contract_status_check CHECK (status IN ('draft')),
  version            integer NOT NULL DEFAULT 1 CONSTRAINT contract_version_check CHECK (version >= 1),
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT contract_frequency_divides_term CHECK (term_months % CASE payment_frequency WHEN 'quarterly' THEN 3 WHEN 'yearly' THEN 12 ELSE 1 END = 0),
  -- A company's range numbers each contract once.
  CONSTRAINT contract_number_once UNIQUE (tenant_id, legal_entity_id, document_number)
);
CREATE TRIGGER contract_touch BEFORE UPDATE ON leases.contract FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: the tenant's contracts by number, whichever companies the
-- caller may act in, and a lessor's.
CREATE INDEX contract_by_number ON leases.contract (tenant_id, document_number, id);
CREATE INDEX contract_by_lessor ON leases.contract (tenant_id, lessor_id, document_number, id);
SELECT platform.secure_table('leases.contract'::regclass);
-- Creating is the only write: nothing changes a contract yet, and nothing
-- is deleted (T88b grants what activating it writes).
REVOKE UPDATE ON leases.contract FROM erp_app;

-- The contract's payments (ADR-0073 §4), written when it is created: one
-- per interval, numbered from 1 in date order, each due at the start or
-- the end of its interval, in minor units of the contract's currency.
-- Insert-only: no version, so erp_app is granted no update.
CREATE TABLE leases.payment (
  tenant_id    uuid NOT NULL,
  contract_id  uuid NOT NULL,
  payment_no   integer NOT NULL CONSTRAINT payment_no_check CHECK (payment_no BETWEEN 1 AND 600),
  due_date     date NOT NULL,
  amount       bigint NOT NULL CONSTRAINT payment_amount_positive CHECK (amount > 0),
  PRIMARY KEY (tenant_id, contract_id, payment_no),
  CONSTRAINT payment_contract_fkey FOREIGN KEY (tenant_id, contract_id) REFERENCES leases.contract (tenant_id, id),
  -- One payment a day: the schedule's dates follow one another.
  CONSTRAINT payment_date_once UNIQUE (tenant_id, contract_id, due_date)
);
SELECT platform.secure_table('leases.payment'::regclass);

-- Reading lease contracts and creating them (ADR-0073 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('leases.contract.read', 'View lease contracts of the companies one may act in, with their payments', false),
  ('leases.contract.write', 'Create lease contracts as lessee', false);
