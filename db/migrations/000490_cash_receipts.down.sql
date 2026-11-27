-- Snapshot lines of a receipt from a customer, or naming a sales order,
-- which the previous version cannot hold, are refused (06 §6.4: a down
-- migration loses no data); otherwise the down drops what 000490 added.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM cash.snapshot_flow WHERE customer_id IS NOT NULL OR document_kind = 'sales_order'
      OR source IN ('receivable', 'receivable_blocked', 'sales_order')) THEN
    RAISE EXCEPTION 'snapshot lines of receipts from customers exist; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE cash.snapshot_flow
  DROP CONSTRAINT snapshot_flow_customer,
  DROP CONSTRAINT snapshot_flow_document_kind_check,
  ADD CONSTRAINT snapshot_flow_document_kind_check
    CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record', 'purchase_order'));
ALTER TABLE cash.snapshot_flow DROP COLUMN customer_id;
