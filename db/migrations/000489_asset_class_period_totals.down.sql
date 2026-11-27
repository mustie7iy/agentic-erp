-- The period totals are derived from the movements alone, which stay, so
-- dropping them loses no data (06 §6.4): the up migration totals them
-- again.
DROP TABLE assets.class_period_total;
