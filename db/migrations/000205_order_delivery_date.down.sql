-- A snapshot line naming an order is not one 000198's check admits: the
-- down keeps the line and its figures, naming no document, as a line
-- beyond the document level does not.
UPDATE cash.snapshot_flow SET document_kind = NULL, document_id = NULL, document_number = NULL WHERE document_kind = 'purchase_order';
ALTER TABLE cash.snapshot_flow
  DROP CONSTRAINT snapshot_flow_document_kind_check,
  ADD CONSTRAINT snapshot_flow_document_kind_check
    CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record'));
ALTER TABLE procurement.purchase_order_item DROP COLUMN delivery_date;
