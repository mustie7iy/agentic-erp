-- Contract step of 000170 (06-engineering-process §6.4; ADR-0051 As
-- built, T72n5; review:batch26): a requisition's date no longer defaults
-- to UTC's today. The default bridged the release before T72n5, which
-- inserted without the column, in a mixed cell; no such release runs (no
-- cell has been deployed: the product owner put deployment last on
-- 2026-11-02), and the application has dated every requisition in its
-- company's time zone since T72n5. Without the default an insert that
-- leaves the date out fails the validated requisition_dated rule
-- (000171) instead of dating the requisition on UTC's day.
ALTER TABLE procurement.requisition ALTER COLUMN requisition_date DROP DEFAULT;
