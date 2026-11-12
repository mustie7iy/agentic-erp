DELETE FROM identity.sod_rule WHERE permission_a = 'receivables.payment.apply' OR permission_b = 'receivables.payment.apply';
DELETE FROM identity.role_permission WHERE permission_code = 'receivables.payment.apply';
DELETE FROM ref.permission WHERE code = 'receivables.payment.apply';
-- The applications go with their tables; their clearing entries stay in
-- the ledger, where the receipts and items remain cleared.
DROP TABLE receivables.incoming_payment_item;
DROP TABLE receivables.incoming_payment;
