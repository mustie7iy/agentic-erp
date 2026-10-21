-- Validates the checks 000112 added NOT VALID (T49h; ADR-0047; 06 §6.4):
-- no statement was reconciled before them, and every target was a payment
-- or a posting rule, which the new check still takes.
ALTER TABLE payments.bank_statement VALIDATE CONSTRAINT bank_statement_reconciled;
ALTER TABLE payments.statement_match_target VALIDATE CONSTRAINT statement_match_target_one;
