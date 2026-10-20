-- Files queued, in flight or failed cannot be kept by the previous schema;
-- the down refuses rather than change what was asked of the bank. The
-- narrower status check comes back NOT VALID, as 000093's down does.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM payments.bank_file WHERE status IN ('to_send', 'sending', 'failed')) THEN
    RAISE EXCEPTION 'bank files waiting for or failing transmission exist';
  END IF;
END $$;
REVOKE SELECT (tenant_id, status, next_attempt_at) ON payments.bank_file FROM erp_worker;
ALTER TABLE payments.bank_file DROP CONSTRAINT bank_file_transmission, DROP CONSTRAINT bank_file_status_check;
ALTER TABLE payments.bank_file ADD CONSTRAINT bank_file_status_check CHECK (status IN ('created', 'transmitted', 'acknowledged', 'rejected')) NOT VALID;
ALTER TABLE payments.bank_file DROP COLUMN last_error, DROP COLUMN next_attempt_at, DROP COLUMN attempts,
  DROP COLUMN transmit_requested_by, DROP COLUMN transmit_operation_id;
