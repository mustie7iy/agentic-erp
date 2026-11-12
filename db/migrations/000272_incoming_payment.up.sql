-- An incoming payment applied (T75c; ADR-0063 §6; ADR-0047; 03-data-model
-- §3.26): the receivables document that applies a receipt waiting on a
-- house bank's incoming payments clearing account to a customer's open
-- items, in full and exactly. The receipt is the journal entry a matched
-- statement line posted; the clearing entry is the ledger's, posted with
-- the application, and clears the receipt and every item. The document is
-- written once with its items and never changed: an application is not
-- undone in the first version. legal_entity_id is the tenancy context's,
-- customer_id the partner context's, the entries, the account and the
-- items the ledger's: no foreign key leaves the schema (02 §2.1).
--
-- Expand only (06 §6.4): two new tables and a permission, which the
-- previous version neither reads nor writes.
CREATE TABLE receivables.incoming_payment (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  document_number      text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id      uuid NOT NULL,       -- tenancy context
  customer_id          uuid NOT NULL,       -- partner context
  receipt_entry_year   smallint NOT NULL,
  receipt_entry_id     uuid NOT NULL,       -- ledger context: the entry the statement line posted
  receipt_account_id   uuid NOT NULL,       -- ledger context: the house bank's incoming payments clearing account
  posting_date         date NOT NULL,
  currency             char(3) NOT NULL REFERENCES ref.currency,
  amount               bigint NOT NULL CONSTRAINT incoming_payment_amount_positive CHECK (amount > 0),
  clearing_entry_year  smallint NOT NULL,
  clearing_entry_id    uuid NOT NULL,       -- ledger context: the entry that cleared receipt and items
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT incoming_payment_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  -- A receipt is applied once, and a clearing entry is one application's.
  CONSTRAINT incoming_payment_receipt_once UNIQUE (tenant_id, receipt_entry_id),
  CONSTRAINT incoming_payment_entry_once UNIQUE (tenant_id, clearing_entry_id)
);
-- The list: a company's applications by date, and a customer's.
CREATE INDEX incoming_payment_by_date ON receivables.incoming_payment (tenant_id, legal_entity_id, posting_date, id);
CREATE INDEX incoming_payment_by_customer ON receivables.incoming_payment (tenant_id, customer_id, posting_date, id);
CREATE TRIGGER incoming_payment_immutable BEFORE UPDATE OR DELETE ON receivables.incoming_payment
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.incoming_payment'::regclass);

-- An item the payment was applied to: an open item of the customer, with
-- the amount it stood at in the payment's currency, debit positive (an
-- invoice is positive, a credit of the customer's negative). The items of
-- a payment add up to its amount; the ledger held the application to that.
CREATE TABLE receivables.incoming_payment_item (
  tenant_id            uuid NOT NULL,
  incoming_payment_id  uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  fiscal_year          smallint NOT NULL,
  journal_entry_id     uuid NOT NULL,       -- ledger context
  line_no              integer NOT NULL CHECK (line_no > 0),
  amount               bigint NOT NULL CONSTRAINT incoming_payment_item_amount_not_zero CHECK (amount <> 0),
  PRIMARY KEY (tenant_id, incoming_payment_id, item_no),
  FOREIGN KEY (tenant_id, incoming_payment_id) REFERENCES receivables.incoming_payment (tenant_id, id),
  -- An open item is cleared once, so one payment is applied to it.
  CONSTRAINT incoming_payment_item_once UNIQUE (tenant_id, journal_entry_id, line_no)
);
CREATE TRIGGER incoming_payment_item_immutable BEFORE UPDATE OR DELETE ON receivables.incoming_payment_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.incoming_payment_item'::regclass);

-- Applying incoming payments (ADR-0063 §8). Reading the applications and
-- the receipts not yet applied takes receivables.invoice.read.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('receivables.payment.apply', 'Apply incoming payments to customers'' open items', false);
