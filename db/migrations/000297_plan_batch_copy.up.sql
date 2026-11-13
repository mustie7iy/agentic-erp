-- A plan batch written by a copy of another category's plan (T93a;
-- ADR-0050 §8 As built T93a): POST /v1/plan-categories/{id}:copy copies a
-- category's current lines of a company and fiscal year into another
-- category, scaled by a percentage, as one batch of the target. The batch
-- records its origin: the category copied from and the percentage. No new
-- table.
--
-- Expand only (06 §6.4): two nullable columns on an insert-only table.
-- NULL on both: an uploaded batch, which is what every existing batch is
-- and what the previous version writes; nothing is backfilled. The checks
-- and the foreign key are added validated, not NOT VALID first (06 §6.4):
-- every existing row has both columns NULL and satisfies them, and the
-- table holds one row per batch applied, so the validating scan under the
-- ALTER's lock is short (as 000264's).
ALTER TABLE controlling.plan_batch
  ADD COLUMN copied_from_category_id uuid,
  ADD COLUMN copy_percentage         numeric(8,4),
  -- Both or neither: a copy names what it copied and by how much.
  ADD CONSTRAINT plan_batch_copy_origin CHECK ((copied_from_category_id IS NULL) = (copy_percentage IS NULL)),
  -- A percentage above 0 and at most 1000, as the request takes it.
  ADD CONSTRAINT plan_batch_copy_percentage CHECK (copy_percentage > 0 AND copy_percentage <= 1000),
  -- A category is copied into another one, never into itself.
  ADD CONSTRAINT plan_batch_copy_other_category CHECK (copied_from_category_id <> category_id),
  ADD CONSTRAINT plan_batch_copied_from FOREIGN KEY (tenant_id, copied_from_category_id) REFERENCES controlling.plan_category (tenant_id, id);
