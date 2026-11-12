-- The internal order on journal lines (T60b; ADR-0064 §2; 03-data-model
-- §3.7): the second real cost object beside the cost center. An order id
-- without a foreign key: the order master is the controlling context's
-- (controlling.internal_order, 000267), and the ledger asks it through its
-- port ledger.CostObjects before it posts (known, of the line's company,
-- released). The line's check below holds what the database can: a line
-- carries one real cost object, a cost center or an order, never both.
-- Expand only (06 §6.4): a nullable column without a default rewrites no
-- partition, and the check is added NOT VALID, reading none of the rows
-- under the lock; 000269 validates it partition by partition and 000270
-- indexes the lines by order. Lines posted before keep NULL; nothing is
-- backfilled.
ALTER TABLE ledger.journal_line ADD COLUMN internal_order_id uuid;
-- Added to the parent, it is each partition's too, and a partition created
-- later (db.EnsurePartitions) gets it validated, being empty.
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_one_cost_object
  CHECK (internal_order_id IS NULL OR cost_center_id IS NULL) NOT VALID;
