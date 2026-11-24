-- Contract step of S2j's delivery assignment plant (ADR-0082 As built
-- S2j; 06 §6.4): sales.delivery_assignment.plant_id, which 000430 added
-- nullable and backfilled, is required. Every server writes it since S2j
-- (sales' assign refuses a delivery without a plant), so only an
-- assignment a server of the release before S2j wrote during the rolling
-- upgrade can lack it: such a one takes its outbound delivery's plant, as
-- 000430 backfilled the assignments before it (the insert-only trigger
-- set aside for the backfill). The check is added NOT VALID, so this file
-- holds the table's lock only briefly; 000459 validates it and sets the
-- column NOT NULL.
ALTER TABLE sales.delivery_assignment DISABLE TRIGGER delivery_assignment_immutable;
-- Tenant rows: under a role that does not bypass row-level security this
-- fails rather than backfill nothing, as 000430.
SET LOCAL row_security = off;
UPDATE sales.delivery_assignment a SET plant_id = d.plant_id
FROM shipping.outbound_delivery d
WHERE a.plant_id IS NULL AND d.tenant_id = a.tenant_id AND d.id = a.outbound_delivery_id;
ALTER TABLE sales.delivery_assignment ENABLE TRIGGER delivery_assignment_immutable;
ALTER TABLE sales.delivery_assignment
  ADD CONSTRAINT delivery_assignment_plant_required CHECK (plant_id IS NOT NULL) NOT VALID;
