CREATE OR REPLACE FUNCTION identity.check_agent_approvals() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE offending text;
BEGIN
  SELECT rp.permission_code INTO offending
  FROM identity.principal_role pr
  JOIN identity.principal p ON p.tenant_id = pr.tenant_id AND p.id = pr.principal_id AND p.kind = 'agent'
  JOIN identity.role_permission rp ON rp.tenant_id = pr.tenant_id AND rp.role_id = pr.role_id
  JOIN ref.permission perm ON perm.code = rp.permission_code AND perm.is_approval
  WHERE pr.tenant_id = NEW.tenant_id AND pr.role_id = NEW.role_id
    AND daterange(pr.valid_from, pr.valid_to) && daterange(rp.valid_from, rp.valid_to)
    AND (pr.valid_to IS NULL OR pr.valid_to > current_date)
  LIMIT 1;
  IF offending IS NOT NULL THEN
    RAISE EXCEPTION 'agents may not hold approval permission %', offending USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
