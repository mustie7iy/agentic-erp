-- The last day a payment's cash discounts are earned (T36d, 03-data-model
-- §3.10): the earliest last day of the tiers its items' discounts were
-- taken in, null when it takes none. A release after it is refused, so a
-- run never takes a discount its payment no longer earns; the run is
-- proposed again. Existing rows take no discount.
ALTER TABLE payments.payment ADD COLUMN discount_until date;
