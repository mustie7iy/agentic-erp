-- Accounts receivable (T75a; ADR-0063 §1-§3; 02 §2.2, 03-data-model §3.26):
-- the receivables context's schema with its outbox and inbox, and the
-- customer invoice an AR accountant enters without a sales order: a
-- header and its items by account, each item's output tax calculated at
-- the invoice date and kept with it. T75a only enters documents: a
-- document is a draft invoice, and what was entered is fixed (the status
-- and kind checks are widened, and the update grants given, by the slices
-- that post it and add credit memos, T75b and T75d). legal_entity_id is
-- the tenancy context's, customer_id the partner context's, the items'
-- accounts and centers the ledger's: no foreign key leaves the schema
-- (02 §2.1). Expand only: a new schema and three permissions, which the
-- previous version neither reads nor writes (06 §6.4).
CREATE SCHEMA receivables;
GRANT USAGE ON SCHEMA receivables TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('receivables');
SELECT platform.secure_table('receivables.outbox'::regclass);
SELECT platform.secure_table('receivables.inbox'::regclass);
GRANT UPDATE (published_at) ON receivables.outbox TO erp_app;
GRANT USAGE ON SCHEMA receivables TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON receivables.outbox TO erp_relay;

-- A customer invoice: the accounting record of an invoice the company
-- issued (ADR-0063 §2). Amounts are minor units of currency, never
-- negative: the kind gives their direction. net_amount and tax_amount are
-- the sums of the items', gross_amount their sum.
CREATE TABLE receivables.customer_invoice (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  document_number  text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  customer_id      uuid NOT NULL,       -- partner context
  kind             text NOT NULL CONSTRAINT customer_invoice_kind_check CHECK (kind IN ('invoice')),
  invoice_date     date NOT NULL,
  posting_date     date,
  currency         char(3) NOT NULL REFERENCES ref.currency,
  net_amount       bigint NOT NULL,
  tax_amount       bigint NOT NULL,
  gross_amount     bigint NOT NULL,
  status           text NOT NULL CONSTRAINT customer_invoice_status_check CHECK (status IN ('draft')),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT customer_invoice_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  CONSTRAINT customer_invoice_amounts_not_negative CHECK (net_amount >= 0 AND tax_amount >= 0),
  CONSTRAINT customer_invoice_totals CHECK (gross_amount = net_amount + tax_amount)
);
CREATE TRIGGER customer_invoice_touch BEFORE UPDATE ON receivables.customer_invoice FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's documents by date, and a customer's.
CREATE INDEX customer_invoice_by_date ON receivables.customer_invoice (tenant_id, legal_entity_id, invoice_date, id);
CREATE INDEX customer_invoice_by_customer ON receivables.customer_invoice (tenant_id, customer_id, invoice_date, id);
SELECT platform.secure_table('receivables.customer_invoice'::regclass);
-- Entering is the only write: nothing changes a draft yet, and nothing is
-- deleted (T75b grants what posting sets).
REVOKE UPDATE ON receivables.customer_invoice FROM erp_app;

-- An item: a net amount on an account (revenue, as a rule), with the tax
-- its output tax code gives at the invoice date. An item without a code
-- bears no tax.
CREATE TABLE receivables.customer_invoice_item (
  tenant_id            uuid NOT NULL,
  customer_invoice_id  uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  description          text CHECK (length(description) BETWEEN 1 AND 500),
  account_id           uuid NOT NULL,   -- ledger context
  net_amount           bigint NOT NULL CONSTRAINT customer_invoice_item_net_positive CHECK (net_amount > 0),
  tax_code             text CHECK (length(tax_code) BETWEEN 1 AND 20),
  tax_amount           bigint NOT NULL CONSTRAINT customer_invoice_item_tax_not_negative CHECK (tax_amount >= 0),
  cost_center_id       uuid,            -- ledger context
  profit_center_id     uuid,            -- ledger context
  PRIMARY KEY (tenant_id, customer_invoice_id, item_no),
  FOREIGN KEY (tenant_id, customer_invoice_id) REFERENCES receivables.customer_invoice (tenant_id, id),
  CONSTRAINT customer_invoice_item_tax_needs_code CHECK (tax_code IS NOT NULL OR tax_amount = 0)
);
-- What was entered is fixed.
CREATE TRIGGER customer_invoice_item_immutable BEFORE UPDATE OR DELETE ON receivables.customer_invoice_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.customer_invoice_item'::regclass);

-- Reading, entering and posting customer invoices (ADR-0063 §8); posting
-- is used from T75b.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('receivables.invoice.read', 'View customer invoices and credit memos of the companies one may act in', false),
  ('receivables.invoice.write', 'Enter customer invoices and credit memos', false),
  ('receivables.invoice.post', 'Post customer invoices and credit memos to the ledger', false);
