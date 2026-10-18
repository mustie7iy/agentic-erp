-- The last day a payment's cash discounts are earned (T36d, 03-data-model
-- §3.10): the earliest last day of the tiers its items' discounts were
-- taken in, null when it takes none. A release after it is refused, so a
-- run never takes a discount its payment no longer earns; the run is
-- proposed again. Payments of runs proposed before it have none, so their
-- release is not refused; T36c, which takes discounts, ships with this.
ALTER TABLE payments.payment ADD COLUMN discount_until date;
