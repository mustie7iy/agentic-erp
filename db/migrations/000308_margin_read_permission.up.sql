-- The margin report's read permission (T92b; ADR-0075 §5): the report is
-- computed on read from the ledger's balances, and the accounts counted
-- as cost of sales are configuration, so this slice adds no table. Expand
-- only: a new permission, which no role holds until it is granted.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.margin.read', 'View the margin report by profit center', false);
