-- The reconciliation changed rows, not the schema: the reservations it
-- released stay released (they are released once) and those it added are
-- what the running release keeps. There is nothing to undo.
SELECT 1;
