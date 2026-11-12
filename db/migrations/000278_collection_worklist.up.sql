-- The collection worklist (T76c; ADR-0065 §7, §9; 03-data-model §3.27): a
-- company's customers with overdue items, computed on read from the
-- ledger's customer open items and the recorded dunning levels. It stores
-- nothing: the migration adds its permission alone, which the previous
-- version does not know (expand only, 06 §6.4).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('collections.worklist.read', 'View the collection worklist of the companies one may act in', false);
