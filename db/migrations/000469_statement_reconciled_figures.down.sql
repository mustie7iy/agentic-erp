-- Drops the figures 000469 added; the figures read every statement's
-- lines again, as before it.
ALTER TABLE payments.bank_statement DROP COLUMN reconciled_excluded, DROP COLUMN reconciled_posted_until;
