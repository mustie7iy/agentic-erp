DELETE FROM identity.sod_rule WHERE 'identity.session.end' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'identity.session.end';
DELETE FROM ref.permission WHERE code = 'identity.session.end';
REVOKE SELECT (web_client_id, web_auth_method, web_credential_ref, web_login_code, web_scope) ON identity.identity_provider FROM erp_control;
DROP INDEX identity.identity_provider_login_code;
ALTER TABLE identity.identity_provider
  DROP CONSTRAINT identity_provider_web_client,
  DROP COLUMN web_scope,
  DROP COLUMN web_login_code,
  DROP COLUMN web_credential_ref,
  DROP COLUMN web_auth_method,
  DROP COLUMN web_client_id;
