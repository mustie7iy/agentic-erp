-- Drops the check 000458 added; an assignment it gave its delivery's plant
-- keeps it, as 000430's would have.
ALTER TABLE sales.delivery_assignment DROP CONSTRAINT delivery_assignment_plant_required;
