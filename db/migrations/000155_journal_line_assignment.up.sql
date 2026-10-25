-- Account assignment on the journal line (T56d; ADR-0050 §5; 03-data-model
-- §3.7): the cost center, profit center and segment of each line, as SAP's
-- ACDOCA carries RCNTR, PRCTR and SEGMENT. Expand only (06 §6.4): nullable
-- columns without a default rewrite no partition, and the foreign keys are
-- added NOT VALID to each partition, which reads none of its rows; 000156
-- validates them partition by partition and adds them to the parent, and
-- 000157-000158 build the indexes. Lines posted before keep NULL; nothing
-- is backfilled. The commit-time check of ADR-0050 §5's table comes in the
-- release after (T56d2), once no replica of the previous release, which
-- writes lines without centers, posts any more (06 §6.4); until then the
-- ledger derives and validates every line before it writes it.
ALTER TABLE ledger.journal_line ADD COLUMN cost_center_id uuid, ADD COLUMN profit_center_id uuid, ADD COLUMN segment_id uuid;

-- PostgreSQL 17 cannot add a NOT VALID foreign key to a partitioned table,
-- so each partition gets its own under the parent's name; 000156 adds the
-- parent's, which takes the partitions' over once they are validated.
-- The cost center names the line's company with it, so a line never
-- carries another company's cost center; the profit center is one
-- assigned to the line's company.
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
