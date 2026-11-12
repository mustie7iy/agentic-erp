-- A validated check cannot be marked unvalidated again: the down restores
-- 000266's state by adding it back NOT VALID.
ALTER TABLE ledger.journal_line DROP CONSTRAINT journal_line_one_cost_object;
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_one_cost_object
  CHECK (internal_order_id IS NULL OR cost_center_id IS NULL) NOT VALID;
