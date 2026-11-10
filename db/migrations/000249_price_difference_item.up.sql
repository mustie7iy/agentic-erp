-- The purchase order item and the tax borne of a price difference line
-- (accounting specialist B10, po:2026-11-10; ADR-0057 §6, ADR-0049 §8;
-- 03-data-model §3.7). An invoice or credit memo on an order valued
-- through GR/IR posts, per order item, what its amount and the input tax
-- the item bears (a non-deductible component's, a part not recovered)
-- differ from the GR/IR line as ONE line on the price_difference account.
-- That line names no order item (journal_line.purchase_order_id marks a
-- GR/IR line, which the positions add up) and the tax in it is not kept.
-- From now on the ledger records both beside the line: the order item it
-- belongs to and the tax borne in it, in the entry's currency and in
-- local currency at the entry's rate, signed as the line (debit positive).
-- The line's amount less the tax borne is the price difference. A
-- reversal's lines record their originals' with the tax negated.
--
-- Expand only (06 §6.4): a new insert-only table the previous version
-- neither reads nor writes; no posted line, amount or account changes.
-- Lines posted before have no row: their order item and tax are unknown
-- and nothing is backfilled.
CREATE TABLE ledger.price_difference_item (
  tenant_id          uuid NOT NULL,
  fiscal_year        smallint NOT NULL,
  journal_entry_id   uuid NOT NULL,
  line_no            integer NOT NULL,
  purchase_order_id  uuid NOT NULL,     -- procurement context
  po_item_no         integer NOT NULL,
  tax_borne_tc       bigint NOT NULL,
  tax_borne_lc       bigint NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no)
    REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no),
  -- Tax in both currencies or in neither, and of one sign: the local
  -- amount is the transaction amount at the entry's rate.
  CONSTRAINT price_difference_item_tax_sign CHECK (sign(tax_borne_tc) * sign(tax_borne_lc) >= 0)
);

-- The line recorded is a price difference line, never a GR/IR line: that
-- one names its order item itself and bears no tax.
CREATE FUNCTION ledger.check_price_difference_item() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM ledger.journal_line l WHERE l.tenant_id = NEW.tenant_id AND l.fiscal_year = NEW.fiscal_year
             AND l.journal_entry_id = NEW.journal_entry_id AND l.line_no = NEW.line_no AND l.purchase_order_id IS NOT NULL) THEN
    RAISE EXCEPTION 'line % of entry % is a GR/IR line, not a price difference line', NEW.line_no, NEW.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'price_difference_item_not_gr_ir';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER price_difference_item_line BEFORE INSERT ON ledger.price_difference_item
  FOR EACH ROW EXECUTE FUNCTION ledger.check_price_difference_item();
CREATE TRIGGER price_difference_item_insert_only BEFORE UPDATE OR DELETE ON ledger.price_difference_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.price_difference_item'::regclass);
