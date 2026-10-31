-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000170's state by adding it back NOT VALID.
ALTER TABLE procurement.requisition DROP CONSTRAINT requisition_dated;
ALTER TABLE procurement.requisition ADD CONSTRAINT requisition_dated CHECK (requisition_date IS NOT NULL) NOT VALID;
