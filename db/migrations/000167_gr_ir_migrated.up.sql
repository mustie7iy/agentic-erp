-- GR/IR positions migrated from SAP (T72f, 03-data-model §3.7 and §3.17,
-- ADR-0034 As built). An order item SAP received and did not invoice in
-- full leaves an open credit on SAP's GR/IR account, its lines naming the
-- order item (ACDOCA EBELN/EBELP). A migration run takes the position over
-- onto the platform's order item in an entry of its own (source context
-- integration): the item's line on the GR/IR account with SAP's balance
-- and its open quantity, against the account's lines SAP's documents
-- loaded without their order items, so an invoice after cutover clears it
-- instead of booking the expense again.
--
-- The quantity a take-over posts is neither received nor invoiced on the
-- platform: it is kept apart, as a clearing's is (000165), so a position's
-- quantity is invoiced + cleared + migrated - received and the report shows
-- each. Expand only: the previous version neither reads migrated (default
-- 0, every position stays as it was) nor posts a take-over; is_open is the
-- same for every existing row.
ALTER TABLE ledger.gr_ir_position ADD COLUMN migrated numeric(20,6) NOT NULL DEFAULT 0;
ALTER TABLE ledger.gr_ir_position ALTER COLUMN is_open
  SET EXPRESSION AS (received <> invoiced + cleared + migrated OR amount_tc <> 0 OR amount_lc <> 0 OR amount_gc <> 0);
