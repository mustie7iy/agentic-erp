-- The day a point-in-time obligation is satisfied (T81c; ADR-0068 §8;
-- 03-data-model §3.30). By default an obligation satisfied at a point in
-- time is satisfied as its order item's delivery confirmations say; the
-- revenue accountant may name another day on the obligation while the
-- contract is a draft (product owner, 2026-11-12), and the recognition
-- schedule then has the obligation's whole allocated amount on that day.
-- An over-time obligation has its service period and no such day. Expand
-- only (06 §6.4): one nullable column the previous version neither reads
-- nor writes.
ALTER TABLE revenue.performance_obligation
  ADD COLUMN satisfied_on date,
  ADD CONSTRAINT performance_obligation_satisfied_on CHECK (satisfied_on IS NULL OR timing = 'point_in_time');
GRANT UPDATE (satisfied_on) ON revenue.performance_obligation TO erp_app;
