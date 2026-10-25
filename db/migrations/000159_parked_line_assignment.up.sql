-- The account assignment entered on a parked entry's lines (T56d2,
-- ADR-0050 §5): the cost center, profit center and segment a person or an
-- agent gave, kept as entered; the rest is derived when the entry is
-- posted, as for a manual entry. Expand only (06 §6.4): nullable columns
-- without a default rewrite nothing, and the foreign keys are added NOT
-- VALID, reading none of the rows under the lock; 000160 validates them.
-- The company is the entry's: posting checks it, and journal_line's keys
-- hold it for what is posted.
ALTER TABLE ledger.parked_entry_line
  ADD COLUMN cost_center_id uuid,
  ADD COLUMN profit_center_id uuid,
  ADD COLUMN segment_id uuid,
  ADD CONSTRAINT parked_entry_line_cost_center_fkey FOREIGN KEY (tenant_id, cost_center_id) REFERENCES ledger.cost_center (tenant_id, id) NOT VALID,
  ADD CONSTRAINT parked_entry_line_profit_center_fkey FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id) NOT VALID,
  ADD CONSTRAINT parked_entry_line_segment_fkey FOREIGN KEY (tenant_id, segment_id) REFERENCES ledger.segment (tenant_id, id) NOT VALID;
