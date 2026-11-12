DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.billing.read', 'sales.billing.write', 'sales.billing.post')
  OR permission_b IN ('sales.billing.read', 'sales.billing.write', 'sales.billing.post');
DELETE FROM identity.role_permission WHERE permission_code IN ('sales.billing.read', 'sales.billing.write', 'sales.billing.post');
DELETE FROM ref.permission WHERE code IN ('sales.billing.read', 'sales.billing.write', 'sales.billing.post');
DROP TABLE sales.billing_document_item;
DROP TABLE sales.billing_document;
DROP FUNCTION sales.record_billing_document_item();
DROP FUNCTION sales.record_billing_document();
