-- A validated key cannot be marked unvalidated again; the down restores
-- 000159's state by adding each one back NOT VALID.
ALTER TABLE ledger.parked_entry_line
  DROP CONSTRAINT parked_entry_line_segment_fkey,
  ADD CONSTRAINT parked_entry_line_segment_fkey FOREIGN KEY (tenant_id, segment_id) REFERENCES ledger.segment (tenant_id, id) NOT VALID,
  DROP CONSTRAINT parked_entry_line_profit_center_fkey,
  ADD CONSTRAINT parked_entry_line_profit_center_fkey FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id) NOT VALID,
  DROP CONSTRAINT parked_entry_line_cost_center_fkey,
  ADD CONSTRAINT parked_entry_line_cost_center_fkey FOREIGN KEY (tenant_id, cost_center_id) REFERENCES ledger.cost_center (tenant_id, id) NOT VALID;
