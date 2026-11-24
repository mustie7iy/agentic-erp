-- Restores 000451's state: plant_id nullable again under the check, which
-- a validated constraint cannot become again, so it is added back NOT
-- VALID.
ALTER TABLE sales.delivery_assignment ALTER COLUMN plant_id DROP NOT NULL;
ALTER TABLE sales.delivery_assignment
  ADD CONSTRAINT delivery_assignment_plant_required CHECK (plant_id IS NOT NULL) NOT VALID;
