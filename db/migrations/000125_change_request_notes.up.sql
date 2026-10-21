-- Notes for the reviewers of a generated proposal (T62d; ADR-0048;
-- 03-data-model §3.5): applying a country pack records the merge's notes
-- (conflicts with the tenant's settings, items kept that the pack no longer
-- has, rates that come later) on the change request it creates, so they are
-- read where the change is reviewed. A constant default adds the column
-- without rewriting the table (06 §6.4); the previous version writes none.
ALTER TABLE config.change_request ADD COLUMN notes jsonb NOT NULL DEFAULT '[]';
