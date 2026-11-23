-- Intercompany billing (S2i2; ADR-0082 As built S2i; 03-data-model §3.29).
-- The supplying company of an intercompany stock transport order (000443)
-- bills the order's posted goods issue (643) to the business partner that
-- is the receiving company, at the order's prices, and posts it at once
-- as a customer invoice through receivables (SAP's intercompany billing,
-- billing type IV, from memory). It is no sales billing document: those
-- bill a sales order's items, which a goods issue of a purchase order has
-- none of. A goods issue is billed once (intercompany_billing_once), and
-- a billing is written only when its invoice has posted, so it names the
-- invoice and its entry from the start and is never changed: cancelling
-- it is Later. Its number is of the company's billing_document range, as
-- a billing document's. purchase_order_id, issue_document_id,
-- customer_invoice_id and journal_entry_id are other contexts' ids: no
-- foreign key leaves a schema (02 §2.1).
-- Expand only (06 §6.4): two tables the previous version neither reads
-- nor writes.
CREATE TABLE sales.intercompany_billing (
  tenant_id                  uuid NOT NULL,
  id                         uuid NOT NULL,
  document_number            text NOT NULL CONSTRAINT intercompany_billing_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id            uuid NOT NULL,   -- tenancy context: the supplying company, which bills
  customer_id                uuid NOT NULL,   -- partner context: the business partner that is the receiving company
  purchase_order_id          uuid NOT NULL,   -- procurement context: the receiving company's intercompany order
  purchase_order_number      text NOT NULL CONSTRAINT intercompany_billing_purchase_order_number_check CHECK (length(purchase_order_number) BETWEEN 1 AND 40),
  receiving_legal_entity_id  uuid NOT NULL,   -- tenancy context: the order's company
  issue_document_id          uuid NOT NULL,   -- inventory context: the order's goods issue (643)
  issue_document_number      text NOT NULL CONSTRAINT intercompany_billing_issue_document_number_check CHECK (length(issue_document_number) BETWEEN 1 AND 40),
  billing_date               date NOT NULL,
  posting_date               date NOT NULL,
  currency                   char(3) NOT NULL REFERENCES ref.currency,
  net_amount                 bigint NOT NULL CONSTRAINT intercompany_billing_net_positive CHECK (net_amount > 0),
  tax_amount                 bigint NOT NULL CONSTRAINT intercompany_billing_tax_not_negative CHECK (tax_amount >= 0),
  customer_invoice_id        uuid NOT NULL,   -- receivables context
  customer_invoice_number    text NOT NULL CONSTRAINT intercompany_billing_customer_invoice_number_check CHECK (length(customer_invoice_number) BETWEEN 1 AND 40),
  journal_entry_year         smallint NOT NULL,
  journal_entry_id           uuid NOT NULL,   -- ledger context
  created_at                 timestamptz NOT NULL DEFAULT now(),
  created_by                 uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- Another company is billed: an order supplied by a plant of its own
  -- company is a stock transport order within it (S2h), never billed.
  CONSTRAINT intercompany_billing_other_company CHECK (receiving_legal_entity_id <> legal_entity_id),
  CONSTRAINT intercompany_billing_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  -- A goods issue is billed once, and an invoice is of one billing.
  CONSTRAINT intercompany_billing_once UNIQUE (tenant_id, issue_document_id),
  CONSTRAINT intercompany_billing_invoice_once UNIQUE (tenant_id, customer_invoice_id)
);
-- The receiving company finds the billing of its order (S2i3).
CREATE INDEX intercompany_billing_by_order ON sales.intercompany_billing (tenant_id, purchase_order_id);
SELECT platform.secure_table('sales.intercompany_billing'::regclass);
-- What was billed and posted is fixed.
CREATE TRIGGER intercompany_billing_immutable BEFORE UPDATE OR DELETE ON sales.intercompany_billing
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
REVOKE UPDATE ON sales.intercompany_billing FROM erp_app;

-- An item of an intercompany billing: an item of the order, at most once,
-- with the quantity issued, the order's price, its amount (more than
-- zero), the revenue account, output tax code and jurisdiction the
-- supplying company bills it on and the tax its invoice item bore.
CREATE TABLE sales.intercompany_billing_item (
  tenant_id         uuid NOT NULL,
  billing_id        uuid NOT NULL,
  item_no           integer NOT NULL CONSTRAINT intercompany_billing_item_no_check CHECK (item_no > 0),
  po_item_no        integer NOT NULL CONSTRAINT intercompany_billing_item_po_item_no_check CHECK (po_item_no > 0),
  material_id       uuid NOT NULL,   -- materials context
  description       text NOT NULL,       -- the order item's
  quantity          numeric(20,6) NOT NULL CONSTRAINT intercompany_billing_item_quantity_positive CHECK (quantity > 0),
  uom_code          text NOT NULL REFERENCES ref.unit_of_measure,
  unit_price        bigint NOT NULL CONSTRAINT intercompany_billing_item_unit_price_not_negative CHECK (unit_price >= 0),
  price_unit        integer NOT NULL CONSTRAINT intercompany_billing_item_price_unit_positive CHECK (price_unit > 0),
  net_amount        bigint NOT NULL CONSTRAINT intercompany_billing_item_net_positive CHECK (net_amount > 0),
  account_id        uuid NOT NULL,   -- ledger context
  tax_code          text,            -- the supplying company's output tax code (ledger context)
  tax_jurisdiction  text,
  tax_amount        bigint NOT NULL CONSTRAINT intercompany_billing_item_tax_not_negative CHECK (tax_amount >= 0),
  PRIMARY KEY (tenant_id, billing_id, item_no),
  CONSTRAINT intercompany_billing_item_once UNIQUE (tenant_id, billing_id, po_item_no),
  -- An item names a jurisdiction only with a tax code, which bears no tax
  -- without one.
  CONSTRAINT intercompany_billing_item_tax CHECK ((tax_code IS NOT NULL OR tax_jurisdiction IS NULL) AND (tax_code IS NOT NULL OR tax_amount = 0)),
  CONSTRAINT intercompany_billing_item_billing_fkey FOREIGN KEY (tenant_id, billing_id) REFERENCES sales.intercompany_billing (tenant_id, id)
);
SELECT platform.secure_table('sales.intercompany_billing_item'::regclass);
CREATE TRIGGER intercompany_billing_item_immutable BEFORE UPDATE OR DELETE ON sales.intercompany_billing_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
REVOKE UPDATE ON sales.intercompany_billing_item FROM erp_app;
