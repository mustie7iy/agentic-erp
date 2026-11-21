-- Submissions the previous version cannot hold are refused rather than
-- dropped (06 §6.4: a down migration loses no data). The guard reads
-- every tenant's rows, under a role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM einvoicing.outgoing_submission) THEN
    RAISE EXCEPTION 'e-invoice submissions exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a = 'einvoicing.outgoing.send' OR permission_b = 'einvoicing.outgoing.send';
DELETE FROM identity.role_permission WHERE permission_code = 'einvoicing.outgoing.send';
DELETE FROM ref.permission WHERE code = 'einvoicing.outgoing.send';
DROP TABLE einvoicing.outgoing_submission;
DROP FUNCTION einvoicing.check_outgoing_submission();
