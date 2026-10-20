-- Validates the check 000091 added NOT VALID (06 §6.4): rows before it
-- have no payment methods, so it holds on them.
ALTER TABLE partner.supplier_company VALIDATE CONSTRAINT supplier_company_payment_methods;
