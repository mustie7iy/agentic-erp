-- Validates the check 000174 added NOT VALID (ADR-0047; 06 §6.4): no gap
-- was accepted before it.
ALTER TABLE payments.bank_statement VALIDATE CONSTRAINT bank_statement_gap_accepted;
