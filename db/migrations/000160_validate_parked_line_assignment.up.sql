-- Validates the keys 000159 added NOT VALID (06 §6.4), scanning the table
-- without blocking writes.
ALTER TABLE ledger.parked_entry_line VALIDATE CONSTRAINT parked_entry_line_cost_center_fkey;
ALTER TABLE ledger.parked_entry_line VALIDATE CONSTRAINT parked_entry_line_profit_center_fkey;
ALTER TABLE ledger.parked_entry_line VALIDATE CONSTRAINT parked_entry_line_segment_fkey;
