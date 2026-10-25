-- Validates the foreign keys 000155 added NOT VALID to each journal line
-- partition (06 §6.4), one partition at a time: VALIDATE scans a partition
-- under a lock that lets postings go on. The parent's foreign keys are
-- then added: PostgreSQL takes over each partition's validated key of the
-- same definition without scanning it again, and partitions created later
-- get the keys from the parent.
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_line_cost_center_fkey', p);
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_line_profit_center_fkey', p);
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_line_segment_fkey', p);
  END LOOP;
END $$;
-- Adding the parent's keys takes a lock that holds up postings: wait at
-- most 10 s for it, and run the migration again later rather than queue
-- every posting behind a long transaction.
SET LOCAL lock_timeout = '10s';
ALTER TABLE ledger.journal_line
  ADD CONSTRAINT journal_line_cost_center_fkey FOREIGN KEY (tenant_id, cost_center_id, legal_entity_id)
    REFERENCES ledger.cost_center (tenant_id, id, legal_entity_id),
  ADD CONSTRAINT journal_line_profit_center_fkey FOREIGN KEY (tenant_id, profit_center_id, legal_entity_id)
    REFERENCES ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id),
  ADD CONSTRAINT journal_line_segment_fkey FOREIGN KEY (tenant_id, segment_id) REFERENCES ledger.segment (tenant_id, id);
