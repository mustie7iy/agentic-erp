-- Net revenue by profitability segment's read permission (T92a; ADR-0075
-- §5): the report is computed on read from the sales context's posted
-- billing documents and credit memos, so this slice adds no table. Expand
-- only: a new permission, which no role holds until it is granted.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.profitability.read', 'View net revenue by profitability segment', false);
