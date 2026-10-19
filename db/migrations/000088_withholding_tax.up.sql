-- Withholding tax deducted from suppliers (T46f, ADR-0037;
-- 03-data-model §3.7, §3.10). A supplier open item keeps the withholding
-- taxes of its supplier's codes when it posts (SAP WITH_ITEM): the code,
-- its rate and account then, the base in the sign the item is paid in,
-- and, for a code deducted at invoice, the amount the invoice withheld.
-- A code deducted at payment is withheld by the run that pays the item, on
-- the base less the cash discount's share; the payment item records it.
-- The rows are fixed with the item, as its terms are.
CREATE TABLE ledger.open_item_withholding (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL,
  code              text NOT NULL,      -- a ledger.withholding_tax_code of the item's legal entity at posting
  posting           text NOT NULL CHECK (posting IN ('invoice', 'payment')),
  rate_pct          numeric(6,3) NOT NULL CHECK (rate_pct > 0 AND rate_pct < 100),
  base_amount       bigint NOT NULL,    -- transaction currency, in the sign the item is paid in
  amount            bigint,             -- withheld at invoice, of the base's sign; null for a code deducted at payment
  account_id        uuid NOT NULL,      -- the withholding tax payable
  created_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no, code),
  CONSTRAINT open_item_withholding_line_fkey FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no)
    REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no),
  CONSTRAINT open_item_withholding_amount CHECK ((posting = 'invoice') = (amount IS NOT NULL)
    AND (amount IS NULL OR amount = 0 OR (sign(amount) = sign(base_amount) AND abs(amount) < abs(base_amount))))
);
CREATE TRIGGER open_item_withholding_immutable BEFORE UPDATE OR DELETE ON ledger.open_item_withholding FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.open_item_withholding'::regclass);

-- A payment item records the withholding tax the run deducts from it, of
-- the item's sign, and the payment their total; its amount is what is
-- paid after discounts and withholding. Existing rows withheld nothing, and
-- the constant default adds the columns without rewriting the tables; the
-- check is added NOT VALID and validated in 000089 (06 §6.4), new rows are
-- checked from here on.
ALTER TABLE payments.payment_item ADD COLUMN withholding_amount bigint NOT NULL DEFAULT 0;
ALTER TABLE payments.payment_item ADD CONSTRAINT payment_item_withholding_within CHECK (withholding_amount = 0
  OR (sign(withholding_amount) = sign(amount) AND abs(withholding_amount) + abs(discount_amount) < abs(amount))) NOT VALID;
ALTER TABLE payments.payment ADD COLUMN withholding_amount bigint NOT NULL DEFAULT 0;

-- Only is_active may change, and only from true to false; the withholding
-- is fixed like the amount and the discount.
CREATE OR REPLACE FUNCTION payments.check_item_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.tenant_id, NEW.payment_id, NEW.item_fiscal_year, NEW.item_entry_id, NEW.item_line_no, NEW.amount, NEW.discount_amount, NEW.withholding_amount, NEW.due_date)
     IS DISTINCT FROM (OLD.tenant_id, OLD.payment_id, OLD.item_fiscal_year, OLD.item_entry_id, OLD.item_line_no, OLD.amount, OLD.discount_amount, OLD.withholding_amount, OLD.due_date)
     OR (NEW.is_active AND NOT OLD.is_active) THEN
    RAISE EXCEPTION 'payment items can only be deactivated' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
