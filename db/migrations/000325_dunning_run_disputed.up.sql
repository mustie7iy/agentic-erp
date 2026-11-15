-- Disputed invoices in dunning (T86b; ADR-0065 As built; 03-data-model
-- §3.27): a dunning run leaves out an item with an open dispute case and
-- names it on the run as disputed, with the case and what the run read
-- of the item. They are kept on the run as a list, written once when the
-- run is proposed (the application's update grant leaves the column out),
-- and read with the run; no table is added (plan-breadth-week3 §T86b).
-- Expand only (06 §6.4): a column with a default, which the previous
-- version neither reads nor writes; its runs have no disputed items.
ALTER TABLE collections.dunning_run
  ADD COLUMN disputed_items jsonb NOT NULL DEFAULT '[]'
    CONSTRAINT dunning_run_disputed_items_check CHECK (jsonb_typeof(disputed_items) = 'array');
