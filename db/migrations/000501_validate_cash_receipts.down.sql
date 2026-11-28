-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000500's state by adding both back NOT VALID.
ALTER TABLE cash.snapshot_flow
  DROP CONSTRAINT snapshot_flow_document_kind_check,
  ADD CONSTRAINT snapshot_flow_document_kind_check
    CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record', 'purchase_order',
      'sales_order')) NOT VALID,
  DROP CONSTRAINT snapshot_flow_customer,
  ADD CONSTRAINT snapshot_flow_customer CHECK (customer_id IS NULL OR (supplier_id IS NULL AND bucket_end IS NULL)) NOT VALID;
