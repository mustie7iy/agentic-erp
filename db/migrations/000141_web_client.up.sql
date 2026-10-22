-- The web workspace's sign-in (T73b, ADR-0052 §5, G8 and G9).
--
-- G8: an identity provider may name the workspace's OAuth client there: the
-- client id, how erp-web authenticates at the token endpoint, a reference
-- to the credential (ADR-0017: where the key or secret lives, never the
-- value), the login code people sign in with (/login/<code>) and the scope
-- that yields a token for the registration's audience. All five or none.
-- The cell projects them, with the issuer, into the global directory,
-- where erp-web finds a provider by its login code before anyone has
-- signed in; the directory keeps codes unique across cells, this index
-- within the cell.
ALTER TABLE identity.identity_provider
  ADD COLUMN web_client_id      text CHECK (length(web_client_id) BETWEEN 1 AND 255 AND web_client_id ~ '^[!-~]+$'),
  ADD COLUMN web_auth_method    text CHECK (web_auth_method IN ('private_key_jwt', 'client_secret_basic')),
  ADD COLUMN web_credential_ref text CHECK (length(web_credential_ref) <= 512 AND web_credential_ref ~ '^(env|file|vault):[!-~]+$'),
  ADD COLUMN web_login_code     text CHECK (web_login_code ~ '^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$'),
  ADD COLUMN web_scope          text CHECK (length(web_scope) <= 1024 AND web_scope ~ '^[!-~]+( [!-~]+)*$'
                                            AND 'openid' = ANY (string_to_array(web_scope, ' '))),
  ADD CONSTRAINT identity_provider_web_client CHECK (num_nulls(web_client_id, web_auth_method, web_credential_ref, web_login_code, web_scope) IN (0, 5));
-- Like the issuer and audience, a login code names a tenant, so it is
-- unique across tenants: a key not led by tenant_id (ADR-0031).
CREATE UNIQUE INDEX identity_provider_login_code ON identity.identity_provider (web_login_code)
  WHERE status = 'active' AND web_login_code IS NOT NULL;
-- The control role projects the web client into the directory with the
-- issuer (directory.Sync).
GRANT SELECT (web_client_id, web_auth_method, web_credential_ref, web_login_code, web_scope) ON identity.identity_provider TO erp_control;

-- G9: a person ends their own session (endCurrentSession). Every person
-- holds the permission, whatever their roles (authz.Permissions), so it is
-- granted to no role.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('identity.session.end', 'End your own session (every person holds it)', false);
