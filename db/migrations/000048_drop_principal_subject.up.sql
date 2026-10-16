-- migrate:no-transaction
-- The last of the issuer contract step (F7, ADR-0010, T31): a subject is
-- unique per issuer, not per tenant, so principal_subject (the subject
-- alone unique per tenant), which the previous release relied on, goes.
-- principal_issuer_subject (000038) carries uniqueness. Dropped
-- concurrently, so sign-ins and provisioning go on.
DROP INDEX CONCURRENTLY identity.principal_subject;
