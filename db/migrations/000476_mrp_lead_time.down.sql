-- Drops what 000476 added: the availability check counts the stock alone
-- again, as the previous version does.
ALTER TABLE mrp.material_plan DROP COLUMN replenishment_lead_time_days;
