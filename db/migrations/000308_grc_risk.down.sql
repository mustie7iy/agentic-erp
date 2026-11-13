DELETE FROM identity.sod_rule WHERE permission_a IN ('grc.risk.read', 'grc.risk.write') OR permission_b IN ('grc.risk.read', 'grc.risk.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('grc.risk.read', 'grc.risk.write');
DELETE FROM ref.permission WHERE code IN ('grc.risk.read', 'grc.risk.write');
DROP TABLE grc.risk_control;
DROP FUNCTION grc.check_risk_control_change();
DROP TABLE grc.risk_rating;
DROP TABLE grc.risk;
DROP FUNCTION grc.check_risk_change();
