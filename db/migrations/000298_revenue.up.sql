-- Revenue recognition (T81a; ADR-0068 §1-§5; 02 §2.2, 03-data-model
-- §3.30): the revenue context's schema with its outbox and inbox, and the
-- revenue contract of a confirmed sales order with one performance
-- obligation per order item. The inbox is used: the context reads the
-- sales context's SalesOrderChanged from the event bus and creates the
-- contract of an order when it is confirmed, once (ADR-0068 §4; ADR-0007).
-- A contract is a draft, its transaction price the order's net value,
-- without tax, in the company's local currency; an obligation starts as
-- satisfied at a point in time with the order item's net value as its
-- stand-alone selling price. Nothing changes a contract yet (T81b grants
-- what the revenue accountant sets and what activating it writes).
-- legal_entity_id is the tenancy context's, customer_id the partner
-- context's, sales_order_id the sales context's: no foreign key leaves the
-- schema (02 §2.1). Expand only: a new schema and two permissions, which
-- the previous version neither reads nor writes (06 §6.4).
CREATE SCHEMA revenue;
GRANT USAGE ON SCHEMA revenue TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('revenue');
SELECT platform.secure_table('revenue.outbox'::regclass);
SELECT platform.secure_table('revenue.inbox'::regclass);
GRANT UPDATE (published_at) ON revenue.outbox TO erp_app;
GRANT USAGE ON SCHEMA revenue TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON revenue.outbox TO erp_relay;

-- A revenue contract: the contract with a customer that one confirmed
-- sales order stands for (ADR-0068 §3). sales_order_number is the order's
-- document number when the contract was created, which the contracts are
-- listed by; transaction_price is the order's net value in minor units of
-- currency, the company's local currency.
CREATE TABLE revenue.contract (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,    -- tenancy context
  customer_id         uuid NOT NULL,    -- partner context
  sales_order_id      uuid NOT NULL,    -- sales context
  sales_order_number  text NOT NULL CONSTRAINT contract_sales_order_number_check CHECK (length(sales_order_number) BETWEEN 1 AND 40),
  currency            char(3) NOT NULL REFERENCES ref.currency,
  transaction_price   bigint NOT NULL CONSTRAINT contract_transaction_price_positive CHECK (transaction_price > 0),
  status              text NOT NULL CONSTRAINT contract_status_check CHECK (status IN ('draft')),
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- One contract per sales order: the automatic creation and a person's
  -- request for the same order end with one row (ADR-0068 §4).
  CONSTRAINT contract_one_per_sales_order UNIQUE (tenant_id, sales_order_id)
);
CREATE TRIGGER contract_touch BEFORE UPDATE ON revenue.contract FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: the tenant's contracts by their order's number, whichever
-- companies the caller may act in, and a customer's.
CREATE INDEX contract_by_order_number ON revenue.contract (tenant_id, sales_order_number, id);
CREATE INDEX contract_by_customer ON revenue.contract (tenant_id, customer_id, sales_order_number, id);
SELECT platform.secure_table('revenue.contract'::regclass);
-- Creating is the only write: nothing changes a contract yet, and nothing
-- is deleted (T81b grants what its slice sets).
REVOKE UPDATE ON revenue.contract FROM erp_app;

-- A performance obligation: one per item of the contract's sales order
-- (ADR-0068 §3), numbered as the order's items are. item_key and
-- description name what was promised as the order item does; net_amount
-- is the order item's net value. timing says whether the obligation is
-- satisfied at a point in time or over time, and
-- standalone_selling_price is its stand-alone selling price: both are a
-- person's judgement, which start as point in time and the order item's
-- net value (ADR-0068 §2). Amounts are minor units of the contract's
-- currency.
CREATE TABLE revenue.performance_obligation (
  tenant_id                 uuid NOT NULL,
  contract_id               uuid NOT NULL,
  obligation_no             integer NOT NULL CHECK (obligation_no > 0),
  order_item_no             integer NOT NULL CONSTRAINT performance_obligation_order_item_no_check CHECK (order_item_no > 0),
  item_key                  text CONSTRAINT performance_obligation_item_key_check CHECK (btrim(item_key) <> '' AND length(item_key) <= 40),
  description               text NOT NULL CONSTRAINT performance_obligation_description_check CHECK (btrim(description) <> '' AND length(description) <= 200),
  net_amount                bigint NOT NULL CONSTRAINT performance_obligation_net_positive CHECK (net_amount > 0),
  timing                    text NOT NULL CONSTRAINT performance_obligation_timing_check CHECK (timing IN ('point_in_time', 'over_time')),
  standalone_selling_price  bigint NOT NULL CONSTRAINT performance_obligation_price_positive CHECK (standalone_selling_price > 0),
  PRIMARY KEY (tenant_id, contract_id, obligation_no),
  FOREIGN KEY (tenant_id, contract_id) REFERENCES revenue.contract (tenant_id, id),
  -- An order item gives one obligation.
  CONSTRAINT performance_obligation_one_per_order_item UNIQUE (tenant_id, contract_id, order_item_no)
);
SELECT platform.secure_table('revenue.performance_obligation'::regclass);
REVOKE UPDATE ON revenue.performance_obligation FROM erp_app;

-- Reading revenue contracts and creating them (ADR-0068 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('revenue.contract.read', 'View revenue contracts of the companies one may act in', false),
  ('revenue.contract.write', 'Create the revenue contract of a confirmed sales order and set its obligations'' timing and stand-alone selling prices', false);
