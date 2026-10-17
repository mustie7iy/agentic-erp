REVOKE SELECT (tenant_id, id, kind, external_issuer, external_subject, display_name, status, version) ON identity.principal FROM erp_control;
DROP TABLE identity.auth_policy;
DROP TABLE identity.identity_provider;
DROP FUNCTION identity.check_identity_provider_update();
REVOKE USAGE ON SCHEMA identity FROM erp_control;
