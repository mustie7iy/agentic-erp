DELETE FROM identity.sod_rule WHERE permission_a IN ('ledger.entry.park', 'ledger.entry.approve')
   OR permission_b IN ('ledger.entry.park', 'ledger.entry.approve');
DELETE FROM identity.role_permission WHERE permission_code IN ('ledger.entry.park', 'ledger.entry.approve');
DELETE FROM ref.permission WHERE code IN ('ledger.entry.park', 'ledger.entry.approve');
DROP TABLE ledger.parked_entry_line;
DROP TABLE ledger.parked_entry;
DROP FUNCTION ledger.check_parked_entry_change();
