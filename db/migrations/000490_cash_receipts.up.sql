-- The liquidity forecast's receipts from customers (T53l1; ADR-0051 §4,
-- As built T53l1; 03-data-model §3.18): customer open items (sources
-- receivable and receivable_blocked) and confirmed sales orders not yet
-- billed (sales_order) are flows of the forecast, so a snapshot keeps them
-- like the others: a line names the customer a receipt is from
-- (customer_id, the partner context's, no foreign key across schemas, as
-- supplier_id) and a sales order's line names the order (document_kind
-- sales_order). A line names a supplier or a customer, never both. Expand
-- only: a nullable column the previous version neither reads nor writes,
-- and checks added and widened NOT VALID (06 §6.4), validated by 000491.
ALTER TABLE cash.snapshot_flow ADD COLUMN customer_id uuid;  -- partner context: the customer a receipt is from

ALTER TABLE cash.snapshot_flow
  DROP CONSTRAINT snapshot_flow_document_kind_check,
  ADD CONSTRAINT snapshot_flow_document_kind_check
    CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record', 'purchase_order',
      'sales_order')) NOT VALID,
  -- A line pays a supplier or receives from a customer; a bucket's sum
  -- names neither (snapshot_flow_bucket's rule for suppliers).
  ADD CONSTRAINT snapshot_flow_customer CHECK (customer_id IS NULL OR (supplier_id IS NULL AND bucket_end IS NULL)) NOT VALID;
