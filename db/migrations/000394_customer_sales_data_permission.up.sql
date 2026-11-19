-- Customers' sales data under their own permissions (ADR-0067 §7 and As
-- built; product owner, 2026-11-18): GET /v1/customer-sales-data and
-- /{id} need sales.customer_sales_data.read, POST and PATCH .write, in
-- place of sales.price.read and .write, so a tenant can keep price
-- maintenance and assigning customers to price groups apart.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.customer_sales_data.read', 'View customers'' sales data (their price groups) in the companies one may act in', false),
  ('sales.customer_sales_data.write', 'Set up and change customers'' sales data (their price groups)', false);

-- Expand (06 §6.4): no one loses access the day this ships. What is
-- copied: every role grant of sales.price.read (to .read) or .write (to
-- .write) in force today or from a later day, each from its own grant,
-- for the same days but from today at the earliest (the database's day,
-- as a grant's default; the new permission exists from today). A grant
-- ended today or before is not copied. The copy names whoever made the
-- grant it copies; its id is v4 (gen_random_uuid), not the application's
-- v7, which no reader orders grants by creation with.
--
-- The write goes to every role that held sales.price.write, pricing roles
-- too: a role's purpose is not recorded, so nothing here can tell them
-- apart. A tenant then ends the write on its pricing roles
-- (endRolePermission) or adds the rule with createSodRule (ADR-0067, As
-- built).
--
-- The grants are read under a SHARE lock, so one made meanwhile waits and
-- is not missed (a lock not had within the timeout fails the migration,
-- which the next start retries), and with row-level security off, so
-- every tenant's are copied or, run by a role that does not bypass it, the
-- migration fails instead of copying none.
SET LOCAL lock_timeout = '5s';
SET LOCAL row_security = off;
LOCK TABLE identity.role_permission IN SHARE MODE;

INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, valid_to, created_by)
SELECT rp.tenant_id, gen_random_uuid(), rp.role_id,
  CASE rp.permission_code WHEN 'sales.price.read' THEN 'sales.customer_sales_data.read' ELSE 'sales.customer_sales_data.write' END,
  greatest(rp.valid_from, current_date), rp.valid_to, rp.created_by
FROM identity.role_permission rp
WHERE rp.permission_code IN ('sales.price.read', 'sales.price.write')
  AND (rp.valid_to IS NULL OR rp.valid_to > current_date);
