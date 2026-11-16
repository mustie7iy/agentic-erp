DELETE FROM identity.sod_rule WHERE permission_a = 'audit.evidence.export' OR permission_b = 'audit.evidence.export';
DELETE FROM identity.role_permission WHERE permission_code = 'audit.evidence.export';
DELETE FROM ref.permission WHERE code = 'audit.evidence.export';
DROP TABLE audit.evidence_export;
DROP FUNCTION audit.check_evidence_export_change();
