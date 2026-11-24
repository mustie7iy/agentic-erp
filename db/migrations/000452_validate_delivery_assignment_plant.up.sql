-- Validates the check 000451 added NOT VALID on
-- sales.delivery_assignment (06 §6.4), scanning the table without
-- blocking writes, then makes plant_id NOT NULL: the validated check
-- proves no row is null, so SET NOT NULL reads no row again, and the
-- check, now the column's own rule, is dropped.
ALTER TABLE sales.delivery_assignment VALIDATE CONSTRAINT delivery_assignment_plant_required;
ALTER TABLE sales.delivery_assignment ALTER COLUMN plant_id SET NOT NULL;
ALTER TABLE sales.delivery_assignment DROP CONSTRAINT delivery_assignment_plant_required;
