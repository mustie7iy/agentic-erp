-- Validates the version checks 000040 (match_result.invoice_version) and
-- 000041 (supplier_invoice.exception_version) added NOT VALID (06 §6.4,
-- T31). Every row meets them: the rows before those migrations are NULL,
-- which a check passes, and the rows since were checked as they were
-- written. VALIDATE takes a SHARE UPDATE EXCLUSIVE lock, so writes go on
-- while the rows are scanned.
ALTER TABLE payables.match_result VALIDATE CONSTRAINT match_result_invoice_version_positive;
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_exception_version_valid;
