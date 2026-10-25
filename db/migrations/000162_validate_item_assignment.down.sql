-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000161's state by adding each one back NOT VALID.
ALTER TABLE procurement.purchase_order_item DROP CONSTRAINT purchase_order_item_account_assignment;
ALTER TABLE procurement.purchase_order_item ADD CONSTRAINT purchase_order_item_account_assignment
  CHECK (account_assignment IN ('none', 'cost_center') AND (account_assignment = 'cost_center') = (cost_center_id IS NOT NULL)) NOT VALID;
ALTER TABLE procurement.requisition_item DROP CONSTRAINT requisition_item_account_assignment;
ALTER TABLE procurement.requisition_item ADD CONSTRAINT requisition_item_account_assignment
  CHECK (account_assignment IN ('none', 'cost_center') AND (account_assignment = 'cost_center') = (cost_center_id IS NOT NULL)) NOT VALID;
