-- Transmission out of the request (T44e; ADR-0038; 03-data-model §3.10):
-- POST /v1/bank-files/{id}:transmit marks the file to_send in its own
-- transaction, as the outbox records an event, and the transmitter
-- (payments.Transmitter) claims due files, moves each to sending while it
-- uploads with no transaction open, and marks it transmitted, or, after
-- its last attempt or a refusal, failed. next_attempt_at is when a to_send
-- file is due, and for a sending file when its claim lapses and the sweep
-- queues it again. attempts counts the claims of the current request.
-- The new statuses are added NOT VALID and validated by 000103 (06 §6.4);
-- the previous version writes none of them, and a file it reads in one is
-- not created, so it neither transmits nor reports on it.
ALTER TABLE payments.bank_file
  ADD COLUMN transmit_operation_id uuid,
  ADD COLUMN transmit_requested_by uuid,
  ADD COLUMN attempts              smallint NOT NULL DEFAULT 0,
  ADD COLUMN next_attempt_at       timestamptz,
  ADD COLUMN last_error            text;
ALTER TABLE payments.bank_file DROP CONSTRAINT bank_file_status_check;
ALTER TABLE payments.bank_file ADD CONSTRAINT bank_file_status_check
  CHECK (status IN ('created', 'to_send', 'sending', 'transmitted', 'failed', 'acknowledged', 'rejected')) NOT VALID;
ALTER TABLE payments.bank_file ADD CONSTRAINT bank_file_transmission
  CHECK (attempts >= 0 AND (status NOT IN ('to_send', 'sending')
    OR (transmit_operation_id IS NOT NULL AND transmit_requested_by IS NOT NULL AND next_attempt_at IS NOT NULL))) NOT VALID;
-- The transmitter finds the tenants with due files across tenants, as the
-- proposer finds proposals (000016); it reads nothing else of a file.
GRANT SELECT (tenant_id, status, next_attempt_at) ON payments.bank_file TO erp_worker;
