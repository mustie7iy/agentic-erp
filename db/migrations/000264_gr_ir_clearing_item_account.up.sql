-- The account a GR/IR clearing's item clears against, when it is not the
-- clearing's price difference account (accounting specialist B10, product
-- owner 2026-11-10; ADR-0057 §6 and As built B10 step 3; 03-data-model
-- §3.8). A closed order's residual GR/IR position of an order item that
-- is an asset's (on an asset clearing account of the company) is cleared
-- against that asset clearing account, as part of the asset's cost,
-- rather than against the price difference account in profit or loss.
-- The approver is shown the account each item clears against, so the
-- clearing keeps it per item, as shown when it was proposed.
--
-- Expand only (06 §6.4): three nullable columns on an insert-only table.
-- NULL: the clearing's own account (procurement.gr_ir_clearing
-- .offset_account_*), which is what every item proposed before, and every
-- item the previous version proposes, clears against; nothing is
-- backfilled. The check is added validated, not NOT VALID first (06
-- §6.4): every existing row has the three new columns NULL and satisfies
-- it, and the table holds a handful of rows per cleared order, so the
-- validating scan under the ALTER's lock is short (as 000250's).
ALTER TABLE procurement.gr_ir_clearing_item
  ADD COLUMN offset_account_id     uuid,   -- ledger context
  ADD COLUMN offset_account_number text,
  ADD COLUMN offset_account_name   text,
  -- The account is named whole, as shown, or not at all.
  ADD CONSTRAINT gr_ir_clearing_item_account CHECK (
    (offset_account_id IS NULL AND offset_account_number IS NULL AND offset_account_name IS NULL)
    OR (offset_account_id IS NOT NULL AND offset_account_number IS NOT NULL AND offset_account_name IS NOT NULL));
