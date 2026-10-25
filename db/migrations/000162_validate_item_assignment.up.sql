-- Validates the rules 000161 added NOT VALID (T56e, 06-engineering-process
-- §6.4: expand, then validate in a later migration). Validation takes only
-- a SHARE UPDATE EXCLUSIVE lock, so writes go on while the rows are
-- scanned; every row has met them since 000161 (its defaults).
ALTER TABLE procurement.requisition_item VALIDATE CONSTRAINT requisition_item_account_assignment;
ALTER TABLE procurement.purchase_order_item VALIDATE CONSTRAINT purchase_order_item_account_assignment;
