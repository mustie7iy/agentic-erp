ALTER TABLE controlling.plan_batch
  DROP CONSTRAINT plan_batch_copied_from,
  DROP CONSTRAINT plan_batch_copy_other_category,
  DROP CONSTRAINT plan_batch_copy_percentage,
  DROP CONSTRAINT plan_batch_copy_origin,
  DROP COLUMN copy_percentage,
  DROP COLUMN copied_from_category_id;
