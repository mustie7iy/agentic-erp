-- The approval permission of purchase orders (T47c): the release of an
-- order without an approved requisition, and a change that raises a
-- released order's total, are decided on approval requests of the types
-- purchase_order and order_change by holders of the order tiers' roles
-- with this permission. Agents never hold it (is_approval).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('procurement.order.approve', 'Approve the release and changes of purchase orders', true);
