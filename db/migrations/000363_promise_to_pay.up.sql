-- Promises to pay (T76f; ADR-0065 As built; 03-data-model §3.27): a
-- collections specialist records a customer's promise to pay a company an
-- amount by a date for some of its open items. Its status (open, kept,
-- broken) is judged when it is read, from whether its items are still
-- open and the company's today, and is not stored. A dunning run at a key
-- date up to the promised date leaves the promised items out of its
-- notices and names them on the run (promised_items). customer_id is the
-- partner context's, legal_entity_id the tenancy context's and the items'
-- journal lines the ledger's: no foreign key leaves the schema (02 §2.1).
-- Nothing is posted.
-- Expand only (06 §6.4): new tables, a column with a default and a
-- permission, which the previous version neither reads nor writes.

-- The promise: what the customer promised (promised_amount, in the
-- company's currency, which is currency) and by when. customer_number is
-- the customer's when the promise was recorded. A promise is not changed
-- or deleted: version stays 1.
CREATE TABLE collections.promise_to_pay (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  legal_entity_id   uuid NOT NULL,     -- tenancy context
  customer_id       uuid NOT NULL,     -- partner context
  customer_number   text NOT NULL CONSTRAINT promise_to_pay_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  currency          char(3) NOT NULL REFERENCES ref.currency,
  promised_amount   bigint NOT NULL CONSTRAINT promise_to_pay_amount_positive CHECK (promised_amount > 0),
  promised_date     date NOT NULL CONSTRAINT promise_to_pay_date_check CHECK (promised_date <= '2100-12-31'),
  version           integer NOT NULL DEFAULT 1 CONSTRAINT promise_to_pay_version_check CHECK (version >= 1),
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE TRIGGER promise_to_pay_insert_only BEFORE UPDATE OR DELETE ON collections.promise_to_pay
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- What a run and the worklist read: a company's promises by date, those
-- not yet past a key date; the worklist's broken promises of a page's
-- customers.
CREATE INDEX promise_to_pay_by_date ON collections.promise_to_pay (tenant_id, legal_entity_id, promised_date);
CREATE INDEX promise_to_pay_by_customer ON collections.promise_to_pay (tenant_id, legal_entity_id, customer_id, promised_date);
-- The list: the tenant's promises, the latest first, in the order its pages are read.
CREATE INDEX promise_to_pay_by_created_at ON collections.promise_to_pay (tenant_id, created_at, id);
SELECT platform.secure_table('collections.promise_to_pay'::regclass);

-- An item a promise is for, by its journal line, with what the promise
-- read of it: the document number and reference, the net due date and
-- what the customer owed on it, in its currency (amount, more than zero:
-- a promise is for debits) and in the company's as booked (amount_lc).
CREATE TABLE collections.promise_to_pay_item (
  tenant_id          uuid NOT NULL,
  promise_to_pay_id  uuid NOT NULL,
  fiscal_year        smallint NOT NULL, -- ledger context: the journal line
  journal_entry_id   uuid NOT NULL,
  line_no            integer NOT NULL CONSTRAINT promise_to_pay_item_line_no_check CHECK (line_no > 0),
  document_number    text NOT NULL CONSTRAINT promise_to_pay_item_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  reference          text,
  net_due_date       date NOT NULL,
  currency           char(3) NOT NULL REFERENCES ref.currency,
  amount             bigint NOT NULL CONSTRAINT promise_to_pay_item_amount_positive CHECK (amount > 0),
  amount_lc          bigint NOT NULL CONSTRAINT promise_to_pay_item_amount_lc_check CHECK (amount_lc >= 0),
  -- An item is named once in a promise.
  PRIMARY KEY (tenant_id, promise_to_pay_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, promise_to_pay_id) REFERENCES collections.promise_to_pay (tenant_id, id)
);
CREATE TRIGGER promise_to_pay_item_insert_only BEFORE UPDATE OR DELETE ON collections.promise_to_pay_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- The promises of an item, which a new promise of it is checked against.
CREATE INDEX promise_to_pay_item_by_item ON collections.promise_to_pay_item (tenant_id, fiscal_year, journal_entry_id, line_no);
SELECT platform.secure_table('collections.promise_to_pay_item'::regclass);

-- The items a run left out for a promise to pay, a list written once with
-- the run, as its disputed and blocked items are (the update grant leaves
-- it out).
ALTER TABLE collections.dunning_run
  ADD COLUMN promised_items jsonb NOT NULL DEFAULT '[]'
    CONSTRAINT dunning_run_promised_items_check CHECK (jsonb_typeof(promised_items) = 'array');

-- Recording a collection worklist's work: promises to pay here, contacts
-- with customers in T76g (ADR-0065 As built, T76f); both are read with
-- collections.worklist.read.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('collections.worklist.write', 'Record promises to pay and contacts with customers in the collection worklist', false);
