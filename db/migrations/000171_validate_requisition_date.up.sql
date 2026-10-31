-- Validates the rule 000170 added NOT VALID (T72n5, 06-engineering-process
-- §6.4: expand, then validate in a later migration). Validation takes only
-- a SHARE UPDATE EXCLUSIVE lock, so writes go on while the rows are
-- scanned; every row has had a date since 000170 (its backfill and
-- default).
ALTER TABLE procurement.requisition VALIDATE CONSTRAINT requisition_dated;
