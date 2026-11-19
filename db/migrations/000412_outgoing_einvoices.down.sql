-- Outgoing e-invoices the previous version cannot hold are refused rather
-- than dropped (06 §6.4: a down migration loses no data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM einvoicing.outgoing_einvoice) THEN
    RAISE EXCEPTION 'outgoing e-invoices exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a IN ('einvoicing.outgoing.read', 'einvoicing.outgoing.render')
  OR permission_b IN ('einvoicing.outgoing.read', 'einvoicing.outgoing.render');
DELETE FROM identity.role_permission WHERE permission_code IN ('einvoicing.outgoing.read', 'einvoicing.outgoing.render');
DELETE FROM ref.permission WHERE code IN ('einvoicing.outgoing.read', 'einvoicing.outgoing.render');
DROP TABLE einvoicing.outgoing_einvoice;
DROP FUNCTION einvoicing.check_outgoing_einvoice();
