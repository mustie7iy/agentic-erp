-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000205's state by adding it back NOT VALID.
ALTER TABLE cash.snapshot_flow
  DROP CONSTRAINT snapshot_flow_document_kind_check,
  ADD CONSTRAINT snapshot_flow_document_kind_check
    CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record', 'purchase_order')) NOT VALID;
