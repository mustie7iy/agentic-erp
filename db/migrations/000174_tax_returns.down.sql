DELETE FROM identity.sod_rule WHERE permission_a = 'tax.return.read' OR permission_b = 'tax.return.read';
DELETE FROM identity.role_permission WHERE permission_code = 'tax.return.read';
DELETE FROM ref.permission WHERE code = 'tax.return.read';
DROP INDEX ledger.tax_item_unreported;
