-- Validates what 000142 added NOT VALID (06 §6.4): every supplier before it
-- has no planning group.
ALTER TABLE partner.supplier_company VALIDATE CONSTRAINT supplier_company_planning_group;
