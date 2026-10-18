-- Cash discount tiers on payment terms (T36a, 03-data-model §3.6). A
-- term has up to two tiers, as SAP's T052 has (ZTAG1/ZPRZ1, ZTAG2/ZPRZ2):
-- discount_days/discount_pct, there since 000004, are the first, and the
-- second comes only after it, later and smaller, both within the net days.
--
-- The new columns are null on every existing row, so the checks hold on
-- them; the table is small (one row per term and tenant), and adding the
-- columns takes its lock anyway.
ALTER TABLE partner.payment_term
  ADD COLUMN discount2_days smallint CHECK (discount2_days BETWEEN 0 AND 365),
  ADD COLUMN discount2_pct  numeric(9,6) CHECK (discount2_pct > 0 AND discount2_pct < 100),
  ADD CONSTRAINT payment_term_discount2_pair CHECK ((discount2_days IS NULL) = (discount2_pct IS NULL)),
  ADD CONSTRAINT payment_term_discount2_tiers CHECK (discount2_days IS NULL
    OR (discount_days IS NOT NULL AND discount_days < discount2_days AND discount2_days < net_days AND discount2_pct < discount_pct));
