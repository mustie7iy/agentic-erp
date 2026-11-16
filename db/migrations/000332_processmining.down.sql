DELETE FROM identity.sod_rule WHERE permission_a = 'processmining.log.read' OR permission_b = 'processmining.log.read';
DELETE FROM identity.role_permission WHERE permission_code = 'processmining.log.read';
DELETE FROM ref.permission WHERE code = 'processmining.log.read';
