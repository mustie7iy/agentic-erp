-- A customer's return received into stock (S2b4; ADR-0083 As built S2b4,
-- ADR-0067 As built T85b; 03-data-model §3.29, §3.35). A return
-- receipt's items of materials kept in stock are posted into stock by
-- inventory as a material document of movement type 653 (SAP's return
-- delivery into unrestricted-use stock, from memory), its reference the
-- returns order's number, its value posted back from the valuation
-- class's cost of goods sold account to its stock account. The 653 has no
-- reversal yet (Later), so it names no original. The receipt, written
-- once and never changed (000307), is joined by the posting that took its
-- stock items in: return_receipt_posting names the 653, once per receipt,
-- and return_receipt_item_cost the cost each of its items took back from
-- the cost of goods sold, in the company's currency, which the posting
-- names. Both are insert-only.
-- Expand only (06 §6.4): the checks widened NOT VALID (validated by
-- 000505), which the previous version, writing no 653, always passes, and
-- two tables the previous version neither reads nor writes. The checks
-- keep the types 122 and 123 that 000502 (S1e2), applied before it, added.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '122', '123', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602',
                             '643', '653', '701', '702')) NOT VALID,
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '122', '123', '351', '601', '602', '643', '653'))) NOT VALID;

CREATE TABLE sales.return_receipt_posting (
  tenant_id                 uuid NOT NULL,
  receipt_id                uuid NOT NULL,
  returns_order_id          uuid NOT NULL,
  material_document_id      uuid NOT NULL,  -- inventory context: the 653
  material_document_number  text NOT NULL CONSTRAINT return_receipt_posting_material_document_number_check
                              CHECK (length(material_document_number) BETWEEN 1 AND 40),
  currency                  text NOT NULL CONSTRAINT return_receipt_posting_currency_fkey REFERENCES ref.currency,
  created_at                timestamptz NOT NULL DEFAULT now(),
  created_by                uuid NOT NULL,
  -- A receipt posts its stock items once, by one 653.
  PRIMARY KEY (tenant_id, receipt_id),
  CONSTRAINT return_receipt_posting_document_once UNIQUE (tenant_id, material_document_id),
  CONSTRAINT return_receipt_posting_receipt_fkey FOREIGN KEY (tenant_id, receipt_id, returns_order_id)
    REFERENCES sales.return_receipt (tenant_id, id, returns_order_id)
);
CREATE TRIGGER return_receipt_posting_immutable BEFORE UPDATE OR DELETE ON sales.return_receipt_posting
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.return_receipt_posting'::regclass);

-- What an item of a posted receipt took back from the cost of goods sold:
-- an item of the receipt, of the receipt's posting, once.
CREATE TABLE sales.return_receipt_item_cost (
  tenant_id    uuid NOT NULL,
  receipt_id   uuid NOT NULL,
  item_no      integer NOT NULL,
  cost_amount  bigint NOT NULL CONSTRAINT return_receipt_item_cost_check CHECK (cost_amount >= 0),
  PRIMARY KEY (tenant_id, receipt_id, item_no),
  CONSTRAINT return_receipt_item_cost_item_fkey FOREIGN KEY (tenant_id, receipt_id, item_no)
    REFERENCES sales.return_receipt_item (tenant_id, receipt_id, item_no),
  CONSTRAINT return_receipt_item_cost_posting_fkey FOREIGN KEY (tenant_id, receipt_id)
    REFERENCES sales.return_receipt_posting (tenant_id, receipt_id)
);
CREATE TRIGGER return_receipt_item_cost_immutable BEFORE UPDATE OR DELETE ON sales.return_receipt_item_cost
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.return_receipt_item_cost'::regclass);
