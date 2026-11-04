-- An order item's expected delivery date (T53g; ADR-0051 §5, decision P7;
-- ADR-0034; 03-data-model §3.9, §3.18): optional, SAP's EKET-EINDT, copied
-- from the requisition's need date when the order is created from one and
-- the item names none. The liquidity forecast expects an open order item's
-- invoice on it (else on the order date plus the company's
-- cash.order_lead_days) and pays it by the order's terms. A released
-- order's item changes it through an order change, as its quantity and
-- price (check_order_item_editable leaves it free). A snapshot keeps an
-- order's lines naming the order (document_kind purchase_order). Expand
-- only: a nullable column the previous version neither reads nor writes,
-- and a check widened NOT VALID (06 §6.4), validated by 000206.

ALTER TABLE procurement.purchase_order_item ADD COLUMN delivery_date date;

ALTER TABLE cash.snapshot_flow
  DROP CONSTRAINT snapshot_flow_document_kind_check,
  ADD CONSTRAINT snapshot_flow_document_kind_check
    CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record', 'purchase_order')) NOT VALID;
