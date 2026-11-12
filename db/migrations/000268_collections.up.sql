-- Dunning and collections (T76a; ADR-0065 §1-§5; 02 §2.2, 03-data-model
-- §3.27): the collections context's schema with its outbox and inbox, and
-- the dunning run a credit controller starts for a company at a key
-- date. T76a only proposes: a run is a proposal, one notice per customer
-- with the overdue items and the dunning level each would reach, and
-- nothing changes on the items, which stay the ledger's. What a run
-- proposed is fixed (confirming it, T76b, changes the run's status alone
-- and records the levels in a table of its own). legal_entity_id is the
-- tenancy context's, customer_id the partner context's, an item's
-- journal line the ledger's: no foreign key leaves the schema (02 §2.1).
-- Expand only: a new schema and two permissions, which the previous
-- version neither reads nor writes (06 §6.4).
CREATE SCHEMA collections;
GRANT USAGE ON SCHEMA collections TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('collections');
SELECT platform.secure_table('collections.outbox'::regclass);
SELECT platform.secure_table('collections.inbox'::regclass);
GRANT UPDATE (published_at) ON collections.outbox TO erp_app;
GRANT USAGE ON SCHEMA collections TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON collections.outbox TO erp_relay;

-- A dunning run: a company's customer items overdue on the key date,
-- proposed for dunning (ADR-0065 §3). Its totals count its notices and
-- their items; overdue_amount is the items' amounts in the company's
-- currency, as they were booked.
CREATE TABLE collections.dunning_run (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  key_date         date NOT NULL,
  status           text NOT NULL CONSTRAINT dunning_run_status_check CHECK (status IN ('proposed')),
  currency         char(3) NOT NULL REFERENCES ref.currency,
  notice_count     integer NOT NULL CONSTRAINT dunning_run_notice_count_check CHECK (notice_count >= 0),
  item_count       integer NOT NULL CONSTRAINT dunning_run_item_count_check CHECK (item_count >= notice_count),
  overdue_amount   bigint NOT NULL CONSTRAINT dunning_run_overdue_amount_check CHECK (overdue_amount >= 0),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE TRIGGER dunning_run_touch BEFORE UPDATE ON collections.dunning_run FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's runs by key date.
CREATE INDEX dunning_run_by_key_date ON collections.dunning_run (tenant_id, legal_entity_id, key_date, id);
SELECT platform.secure_table('collections.dunning_run'::regclass);
-- Proposing is the only write: nothing changes a proposal yet, and
-- nothing is deleted (T76b grants what confirming sets).
REVOKE UPDATE ON collections.dunning_run FROM erp_app;

-- A notice: what a run proposes for one customer. Its level is the
-- highest its items would reach; overdue_amount is their amounts in the
-- run's currency. customer_number is the customer's business partner
-- number when the run proposed, which the notices are listed by and the
-- events of T76b name the customer with.
CREATE TABLE collections.dunning_notice (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  dunning_run_id   uuid NOT NULL,
  customer_id      uuid NOT NULL,       -- partner context
  customer_number  text NOT NULL CONSTRAINT dunning_notice_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  level            smallint NOT NULL CONSTRAINT dunning_notice_level_check CHECK (level BETWEEN 1 AND 9),
  item_count       integer NOT NULL CONSTRAINT dunning_notice_item_count_check CHECK (item_count > 0),
  overdue_amount   bigint NOT NULL CONSTRAINT dunning_notice_overdue_amount_check CHECK (overdue_amount >= 0),
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, dunning_run_id) REFERENCES collections.dunning_run (tenant_id, id),
  CONSTRAINT dunning_notice_customer_once UNIQUE (tenant_id, dunning_run_id, customer_id),
  -- What an item's key names: its notice within its run.
  CONSTRAINT dunning_notice_in_run UNIQUE (tenant_id, dunning_run_id, id)
);
-- What was proposed is fixed.
CREATE TRIGGER dunning_notice_immutable BEFORE UPDATE OR DELETE ON collections.dunning_notice
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- The list: a run's notices by customer number.
CREATE INDEX dunning_notice_by_customer_number ON collections.dunning_notice (tenant_id, dunning_run_id, customer_number, id);
SELECT platform.secure_table('collections.dunning_notice'::regclass);

-- An item of a notice: a customer's open item overdue on the run's key
-- date, named by its journal line (the ledger's item), with what the run
-- read of it and the level it would reach. amount is what the customer
-- owes in the item's currency, more than zero: credit items are not
-- dunned and reduce nothing (ADR-0065 §4). amount_lc is the same in the
-- run's currency, as booked. previous_level is the level the item was
-- last dunned at, 0 for one never dunned; an item rises at most one level
-- per run and never falls.
CREATE TABLE collections.dunning_notice_item (
  tenant_id          uuid NOT NULL,
  dunning_run_id     uuid NOT NULL,
  dunning_notice_id  uuid NOT NULL,
  item_no            integer NOT NULL CONSTRAINT dunning_notice_item_item_no_check CHECK (item_no > 0),
  fiscal_year        smallint NOT NULL, -- ledger context: the journal line
  journal_entry_id   uuid NOT NULL,
  line_no            integer NOT NULL,
  document_number    text NOT NULL CONSTRAINT dunning_notice_item_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  reference          text,
  document_date      date NOT NULL,
  net_due_date       date NOT NULL,
  days_overdue       integer NOT NULL CONSTRAINT dunning_notice_item_overdue CHECK (days_overdue > 0),
  currency           char(3) NOT NULL REFERENCES ref.currency,
  amount             bigint NOT NULL CONSTRAINT dunning_notice_item_amount_positive CHECK (amount > 0),
  amount_lc          bigint NOT NULL CONSTRAINT dunning_notice_item_amount_lc_check CHECK (amount_lc >= 0),
  previous_level     smallint NOT NULL,
  level              smallint NOT NULL CONSTRAINT dunning_notice_item_level_check CHECK (level BETWEEN 1 AND 9),
  PRIMARY KEY (tenant_id, dunning_notice_id, item_no),
  CONSTRAINT dunning_notice_item_notice_fkey FOREIGN KEY (tenant_id, dunning_run_id, dunning_notice_id)
    REFERENCES collections.dunning_notice (tenant_id, dunning_run_id, id),
  -- With the level's own range, previous_level is 0 to 9 too.
  CONSTRAINT dunning_notice_item_one_level CHECK (level BETWEEN previous_level AND previous_level + 1),
  -- An open item is proposed once in a run.
  CONSTRAINT dunning_notice_item_once_per_run UNIQUE (tenant_id, dunning_run_id, fiscal_year, journal_entry_id, line_no)
);
CREATE TRIGGER dunning_notice_item_immutable BEFORE UPDATE OR DELETE ON collections.dunning_notice_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('collections.dunning_notice_item'::regclass);

-- Reading dunning runs and starting them (ADR-0065 §7).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('collections.dunning.read', 'View dunning runs and their notices of the companies one may act in', false),
  ('collections.dunning.run', 'Start dunning runs', false);
