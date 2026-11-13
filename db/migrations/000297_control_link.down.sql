DROP TABLE grc.control_link;
DROP FUNCTION grc.check_control_link_change();
REVOKE UPDATE (framework_references) ON grc.control FROM erp_app;
ALTER TABLE grc.control DROP COLUMN framework_references;
DROP FUNCTION grc.framework_references_valid(text[]);
