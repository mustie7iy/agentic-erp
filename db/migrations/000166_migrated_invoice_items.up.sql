-- What a source system invoiced and credited on purchase order items before
-- migration (T72b2, 03-data-model §3.9 and §3.17). SAP's posted invoice
-- documents (RBKP RBSTAT 5) are carried by their journal entries, not as
-- supplier invoices, so the three-way match, which counts what was posted
-- on an order item before, saw nothing invoiced on a migrated order: every
-- migrated credit memo was held with quantity_exceeds_invoiced and every
-- subsequent debit or credit with nothing_invoiced. The load takes over the
-- quantity of each ordinary item (RSEG-TBTKZ blank) of a posted document,
-- by its direction (RBKP-XRECH), and the match counts these rows beside the
-- documents posted on the platform.
--
-- One row per item of a taken-over document; the document has one id for
-- its rows and is taken over once (migrated_invoice_item_once), so a load
-- run again, which skips it by its external reference, cannot count it
-- twice either. Insert-only: what SAP invoiced before cutover never
-- changes. No foreign key to the order (procurement context): payables
-- checks the order item through procurement's published API, as it does
-- for its own items. Expand only: the previous release neither reads nor
-- writes the table.
CREATE TABLE payables.migrated_invoice_item (
  tenant_id            uuid NOT NULL,
  migrated_invoice_id  uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  reference            text NOT NULL CHECK (btrim(reference) <> '' AND length(reference) <= 100),
  kind                 text NOT NULL CHECK (kind IN ('invoice', 'credit_memo')),
  purchase_order_id    uuid NOT NULL,    -- procurement context
  po_item_no           integer NOT NULL CHECK (po_item_no > 0),
  quantity             numeric(20,6) NOT NULL CHECK (quantity > 0),
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, migrated_invoice_id, item_no),
  CONSTRAINT migrated_invoice_item_once UNIQUE (tenant_id, reference, item_no)
);
CREATE INDEX migrated_invoice_item_by_order ON payables.migrated_invoice_item (tenant_id, purchase_order_id, po_item_no);
CREATE TRIGGER migrated_invoice_item_insert_only BEFORE UPDATE OR DELETE ON payables.migrated_invoice_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('payables.migrated_invoice_item'::regclass);
