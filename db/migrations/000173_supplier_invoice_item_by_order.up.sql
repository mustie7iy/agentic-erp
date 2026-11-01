-- migrate:no-transaction
-- What documents posted on an order invoiced of each of its items
-- (payables invoicedQuantities) is read at :match, under the order items'
-- locks at :post and at a document's reversal, and by a goods receipt's
-- reversal (03 §3.9): without an index on the order it scanned every
-- invoice item of the tenant while those locks were held. Items without an
-- order are left out. Built concurrently (06 §6.4); a failed build leaves
-- an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY supplier_invoice_item_by_order ON payables.supplier_invoice_item (tenant_id, purchase_order_id, po_item_no)
  WHERE purchase_order_id IS NOT NULL;
