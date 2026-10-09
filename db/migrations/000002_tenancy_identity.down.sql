DROP TABLE IF EXISTS identity.inbox, identity.outbox, identity.sod_rule, identity.company_access, identity.principal_role,
  identity.role_permission, identity.role, identity.agent_profile, identity.principal CASCADE;
DROP FUNCTION IF EXISTS identity.check_agent_approvals();
DROP TABLE IF EXISTS tenancy.inbox, tenancy.outbox, tenancy.org_unit, tenancy.legal_entity, tenancy.tenant CASCADE;
