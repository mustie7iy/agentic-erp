-- The principals bound here keep their issuer: it is what the new release
-- resolves them by, and the previous one ignores it.
ALTER TABLE identity.principal DROP CONSTRAINT principal_issuer_with_subject;
