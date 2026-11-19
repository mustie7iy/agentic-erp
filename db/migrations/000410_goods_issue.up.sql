-- Goods issue of an outbound delivery, cost of goods sold and billing
-- from it (S2b1; ADR-0083 §5; 03-data-model §3.35, §3.36, §3.29). An
-- outbound delivery's goods issue is a material document of movement
-- type 601 (its reversal, 602, is Later), which shipping posts and which
-- names the delivery's number as its reference; its value posts against
-- the valuation class's cost of goods sold account
-- (inventory_cost_of_goods_sold:<class>). The sales delivery the goods
-- issue writes names the outbound delivery and the material document,
-- and each of its items the cost of what it issued, in the company's
-- currency. Expand only (06 §6.4): checks widened NOT VALID (validated by
-- 000411), nullable columns the previous version neither reads nor
-- writes, and a permission.
-- The checks keep what 000407 (S2d) allows, its movement types 701 and
-- 702 and its key inventory_difference:<class>.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '701', '702')) NOT VALID,
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('102', '502', '562', '202', '552', '312', '302', '602'))) NOT VALID,
  -- A goods receipt's and a goods issue's documents, and only they, name
  -- the goods movement or the outbound delivery.
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '601', '602'))) NOT VALID;

-- A valuation class's cost of goods sold account (SAP GBB VAX, from
-- memory) is a key of its own.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation|difference|cost_of_goods_sold):[0-9A-Z]{1,4}$') NOT VALID;

-- The sales delivery a goods issue writes names its outbound delivery and
-- material document, all or none; one a person confirms names neither.
ALTER TABLE sales.delivery
  ADD COLUMN outbound_delivery_id uuid,             -- shipping context
  ADD COLUMN outbound_delivery_number text CONSTRAINT delivery_outbound_delivery_number_check CHECK (length(outbound_delivery_number) BETWEEN 1 AND 40),
  ADD COLUMN material_document_id uuid,             -- inventory context: the 601
  ADD COLUMN material_document_number text CONSTRAINT delivery_material_document_number_check CHECK (length(material_document_number) BETWEEN 1 AND 40),
  ADD CONSTRAINT delivery_goods_issue CHECK ((outbound_delivery_id IS NULL) = (outbound_delivery_number IS NULL)
    AND (outbound_delivery_id IS NULL) = (material_document_id IS NULL) AND (outbound_delivery_id IS NULL) = (material_document_number IS NULL)),
  -- A goods issue delivers once.
  ADD CONSTRAINT delivery_outbound_delivery_once UNIQUE (tenant_id, outbound_delivery_id);
-- The cost of what a goods issue's item issued (cost of goods sold), in
-- the company's currency, with its currency or neither.
ALTER TABLE sales.delivery_item
  ADD COLUMN cost_amount bigint CONSTRAINT delivery_item_cost_check CHECK (cost_amount >= 0),
  ADD COLUMN cost_currency text CONSTRAINT delivery_item_cost_currency_fkey REFERENCES ref.currency,
  ADD CONSTRAINT delivery_item_cost CHECK ((cost_amount IS NULL) = (cost_currency IS NULL));

-- Posting an outbound delivery's goods issue (ADR-0083 §5).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('shipping.goods_issue.post', 'Post the goods issue of outbound deliveries', false);
