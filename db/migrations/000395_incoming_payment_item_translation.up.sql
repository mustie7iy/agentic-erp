-- A receipt in another currency than the items (T75n; ADR-0063 §6 As
-- built; 03-data-model §3.26): an incoming payment may apply its receipt
-- to items in another currency, each translated into the payment's at the
-- average rate valid on the posting date. amount stays the item's amount
-- in the payment's currency, as the clearing entry offset it; item_currency
-- and item_amount are the item's own currency and amount, debit positive
-- as amount is, and exchange_rate the rate it was translated at, units of
-- the payment's currency per unit of the item's. All three are NULL for an
-- item in the payment's currency, and on every row written before this
-- migration or by the previous version during a rolling upgrade, which
-- applies only items in the receipt's currency.
--
-- Expand only (06 §6.4): nullable columns and a check, which the previous
-- version neither reads nor writes.
ALTER TABLE receivables.incoming_payment_item
  ADD COLUMN item_currency char(3) REFERENCES ref.currency,
  ADD COLUMN item_amount bigint,
  ADD COLUMN exchange_rate numeric(28,12),
  -- The three together or none; the item's own amount has the sign of its
  -- amount in the payment's currency, and the rate is above zero.
  ADD CONSTRAINT incoming_payment_item_translation CHECK (
    (item_currency IS NULL) = (item_amount IS NULL) AND (item_currency IS NULL) = (exchange_rate IS NULL)
    AND (item_amount IS NULL OR sign(item_amount) = sign(amount)) AND (exchange_rate IS NULL OR exchange_rate > 0));
