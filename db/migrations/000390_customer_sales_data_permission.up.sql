-- Customers' sales data under their own permissions (ADR-0067 §7 and As
-- built; product owner, 2026-11-18): GET /v1/customer-sales-data and
-- /{id} need sales.customer_sales_data.read, POST and PATCH .write, in
-- place of sales.price.read and .write, so a tenant can keep price
-- maintenance and assigning customers to price groups apart.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.customer_sales_data.read', 'View customers'' sales data (their price groups) in the companies one may act in', false),
  ('sales.customer_sales_data.write', 'Set up and change customers'' sales data (their price groups)', false);

-- Expand (06 §6.4): no one loses access the day this ships. Every role
-- grant of sales.price.read or .write in force today or from a later day
-- is copied to the new permission for the same days, from today at the
-- earliest (the database's day, as a grant's own default; the copy gives
-- what the old permission gave, so it starts when the new one exists).
-- A grant ended before today is not copied. The copy names the person
-- who made the grant it copies. A tenant then takes the write from its
-- pricing roles (the product owner's separation); nothing does that for
-- it, as a role's purpose is not recorded.
--
-- The grants are read under a SHARE lock, so one made while this runs
-- waits and is not missed (a lock not had within the timeout fails the
-- migration, which the next start retries), and with row-level security
-- off, so every tenant's are copied or, run by a role that does not
-- bypass it, the migration fails instead of copying none.
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
