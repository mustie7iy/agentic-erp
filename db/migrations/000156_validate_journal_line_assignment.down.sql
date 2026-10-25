-- Dropping the parent's foreign keys drops the partitions' it took over; a
-- validated key cannot be marked unvalidated again, so the down restores
-- 000155's state by adding each partition's back NOT VALID.
ALTER TABLE ledger.journal_line
  DROP CONSTRAINT journal_line_segment_fkey,
  DROP CONSTRAINT journal_line_profit_center_fkey,
  DROP CONSTRAINT journal_line_cost_center_fkey;
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT journal_line_cost_center_fkey FOREIGN KEY (tenant_id, cost_center_id, legal_entity_id)
      REFERENCES ledger.cost_center (tenant_id, id, legal_entity_id) NOT VALID', p);
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT journal_line_profit_center_fkey FOREIGN KEY (tenant_id, profit_center_id, legal_entity_id)
      REFERENCES ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id) NOT VALID', p);
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT journal_line_segment_fkey FOREIGN KEY (tenant_id, segment_id)
      REFERENCES ledger.segment (tenant_id, id) NOT VALID', p);
  END LOOP;
END $$;
