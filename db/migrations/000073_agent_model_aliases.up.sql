-- An agent's profile names a model alias, which the gateway routes under
-- the tenant's policy (T42f, ADR-0036, PO 2026-10-18). The models agents
-- were given by name before are Claude models, which the platform's
-- erp-default routes to; any other value stays, and the profile's next
-- write refuses it (identity.unknown_model_alias).
UPDATE identity.agent_profile SET model = 'erp-default', updated_at = now(), version = version + 1 WHERE model LIKE 'claude-%';
