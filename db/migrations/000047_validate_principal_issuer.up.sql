-- Validates principal_issuer_with_subject, which 000046 added NOT VALID and
-- whose rows it bound (F7, 06 §6.4, T31): every principal with a subject
-- has an issuer. VALIDATE takes a SHARE UPDATE EXCLUSIVE lock, so writes go
-- on while the rows are scanned.
ALTER TABLE identity.principal VALIDATE CONSTRAINT principal_issuer_with_subject;
