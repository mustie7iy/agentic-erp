-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000471's state by adding them back NOT VALID.
ALTER TABLE shipping.outbound_delivery
  DROP CONSTRAINT outbound_delivery_status_check,
  ADD CONSTRAINT outbound_delivery_status_check CHECK (status IN ('created', 'goods_issued', 'goods_issue_reversed')) NOT VALID,
  DROP CONSTRAINT outbound_delivery_goods_issue,
  ADD CONSTRAINT outbound_delivery_goods_issue CHECK ((status IN ('goods_issued', 'goods_issue_reversed')) = (goods_issue_date IS NOT NULL)
    AND (status IN ('goods_issued', 'goods_issue_reversed')) = (material_document_id IS NOT NULL)
    AND (status IN ('goods_issued', 'goods_issue_reversed')) = (material_document_number IS NOT NULL)
    AND (status IN ('goods_issued', 'goods_issue_reversed')) = (currency IS NOT NULL)) NOT VALID,
  DROP CONSTRAINT outbound_delivery_reversal_material_document_number_check,
  ADD CONSTRAINT outbound_delivery_reversal_material_document_number_check CHECK (length(reversal_material_document_number) BETWEEN 1 AND 40) NOT VALID,
  DROP CONSTRAINT outbound_delivery_goods_issue_reversal,
  ADD CONSTRAINT outbound_delivery_goods_issue_reversal CHECK ((status = 'goods_issue_reversed') = (goods_issue_reversal_date IS NOT NULL)
    AND (status = 'goods_issue_reversed') = (reversal_material_document_id IS NOT NULL)
    AND (status = 'goods_issue_reversed') = (reversal_material_document_number IS NOT NULL)
    AND (goods_issue_reversal_date IS NULL OR goods_issue_reversal_date >= goods_issue_date)) NOT VALID;
