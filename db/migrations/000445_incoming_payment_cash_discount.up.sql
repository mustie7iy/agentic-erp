-- Cash discount granted (ADR-0063 As built, cash discount granted; the
-- product owner's answer of 2026-11-21; 03-data-model §3.26): an item an
-- incoming payment applies may take the cash discount its payment terms
-- grant for a payment received within the discount period. cash_discount
-- is that discount, in the payment's currency: more than zero and less
-- than the item, which is an invoice paid in full (no residual item and
-- nothing written off of it). The discount's lines, its net and its share
-- of each output tax, are lines of the payment's clearing entry, with the
-- tax items they write; this column says which item took how much.
--
-- Expand only (06 §6.4): a nullable column the previous version neither
-- reads nor writes, so every existing row holds the check.
ALTER TABLE receivables.incoming_payment_item
  ADD COLUMN cash_discount bigint,
  ADD CONSTRAINT incoming_payment_item_cash_discount CHECK (
    cash_discount IS NULL OR (cash_discount > 0 AND cash_discount < amount AND residual_amount IS NULL AND write_off_amount IS NULL));
