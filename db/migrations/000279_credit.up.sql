-- Credit management (T77a; ADR-0066; 02 §2.2, 03-data-model §3.28): the
-- credit context's schema with its outbox and inbox, and a customer's
-- credit limit in a company. The limit is one amount per customer and
-- company in the company's currency; what the customer owes is not stored
-- here: the exposure is read from the ledger's open customer items when
-- it is asked for. legal_entity_id is the tenancy context's, customer_id
-- the partner context's: no foreign key leaves the schema (02 §2.1).
-- Expand only: a new schema and two permissions, which the previous
-- version neither reads nor writes (06 §6.4).
CREATE SCHEMA credit;
GRANT USAGE ON SCHEMA credit TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('credit');
SELECT platform.secure_table('credit.outbox'::regclass);
SELECT platform.secure_table('credit.inbox'::regclass);
GRANT UPDATE (published_at) ON credit.outbox TO erp_app;
GRANT USAGE ON SCHEMA credit TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON credit.outbox TO erp_relay;

-- A credit limit: the most a customer may owe a company (ADR-0066 §2).
-- amount is minor units of currency, the company's when the limit was
-- set; zero is a limit, of nothing on credit. customer_number is the
-- customer's business partner number, which never changes: the limits
-- are listed by it and CreditLimitChanged names the customer with it.
CREATE TABLE credit.credit_limit (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  customer_id      uuid NOT NULL,       -- partner context
  customer_number  text NOT NULL CONSTRAINT credit_limit_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  currency         char(3) NOT NULL REFERENCES ref.currency,
  amount           bigint NOT NULL CONSTRAINT credit_limit_amount_check CHECK (amount >= 0),
  version          integer NOT NULL DEFAULT 1 CONSTRAINT credit_limit_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- One limit per customer and company; the exposure read finds it by them.
  CONSTRAINT credit_limit_once UNIQUE (tenant_id, legal_entity_id, customer_id)
);
CREATE TRIGGER credit_limit_touch BEFORE UPDATE ON credit.credit_limit FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: the tenant's limits by customer number, whichever companies
-- the caller may act in, in the order its pages are read.
CREATE INDEX credit_limit_by_customer_number ON credit.credit_limit (tenant_id, customer_number, id);
SELECT platform.secure_table('credit.credit_limit'::regclass);
-- A change sets the amount alone: the customer, the company and the
-- currency of a limit stay, and no limit is deleted.
REVOKE UPDATE ON credit.credit_limit FROM erp_app;
GRANT UPDATE (amount, version, updated_by) ON credit.credit_limit TO erp_app;

-- Reading credit limits and a customer's exposure, and setting limits
-- (ADR-0066 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('credit.limit.read', 'View credit limits and customers'' credit exposure of the companies one may act in', false),
  ('credit.limit.write', 'Set and change customers'' credit limits', false);
