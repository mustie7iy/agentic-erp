-- Replenishment lead time (S2j2; ADR-0082 As built S2j2, ADR-0084 As
-- built; 03-data-model §3.37): a material's planning data at a plant may
-- name how many calendar days the plant takes to have more of it (SAP
-- MARC-WZEIT, from memory). The availability check of a sales order
-- delivering from the plant reads it: an item asked for on or after the
-- company's date today plus the lead time is confirmed in full, what the
-- stock does not cover resting on it. Planning runs do not read it.
-- Expand only (06 §6.4): a nullable column, null on every existing plan,
-- which the previous version neither reads nor writes.
ALTER TABLE mrp.material_plan
  ADD COLUMN replenishment_lead_time_days integer
    CONSTRAINT material_plan_replenishment_lead_time_days_check CHECK (replenishment_lead_time_days BETWEEN 1 AND 999);
