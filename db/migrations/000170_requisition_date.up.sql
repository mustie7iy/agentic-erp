-- A requisition's date (T72n5, product owner 2026-10-31): the company's
-- date, in its time zone (companies[].time_zone, T72n1), when the
-- requisition was raised. Its number is drawn for it, and its amount is
-- converted into the company's currency at that day's rate to route it to
-- an approval tier, on every submission. Stored rather than derived from
-- created_at, so a later change of the zone does not move the day a
-- requisition already raised is routed at.
--
-- Expand (06-engineering-process §6.4): the column is added without a
-- default, rewriting nothing, and the requisitions raised before are dated
-- as they were routed then, created_at's date in UTC. Their version and
-- updated_at stay: nothing a user did changed them, so the touch trigger
-- is off while they are dated, and every tenant's rows are dated
-- (row_security off; under a role that does not bypass row-level security
-- this fails rather than date none). The previous release inserts without
-- the column; the default dates its rows by its own rule, UTC. The rule
-- that every requisition has a date is added NOT VALID, reading none of
-- the rows under the lock; 000171 validates it.
SET LOCAL row_security = off;
ALTER TABLE procurement.requisition ADD COLUMN requisition_date date;
ALTER TABLE procurement.requisition DISABLE TRIGGER requisition_touch;
UPDATE procurement.requisition SET requisition_date = (created_at AT TIME ZONE 'UTC')::date WHERE requisition_date IS NULL;
ALTER TABLE procurement.requisition ENABLE TRIGGER requisition_touch;
ALTER TABLE procurement.requisition ALTER COLUMN requisition_date SET DEFAULT (now() AT TIME ZONE 'UTC')::date,
  ADD CONSTRAINT requisition_dated CHECK (requisition_date IS NOT NULL) NOT VALID;
