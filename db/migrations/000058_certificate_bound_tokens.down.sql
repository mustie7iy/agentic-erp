REVOKE SELECT (certificate_bound) ON identity.identity_provider FROM erp_control;
ALTER TABLE identity.identity_provider DROP COLUMN certificate_bound;
