-- The revert is refused while an order captured from an upload, or an
-- upload for one, exists: the previous version knows neither, and
-- dropping them would lose which document an order was read from.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM sales.sales_order WHERE capture_upload_id IS NOT NULL) OR EXISTS (SELECT FROM payables.upload WHERE purpose = 'sales_order') THEN
    RAISE EXCEPTION 'sales orders were captured from uploads: reverting would lose the documents they were read from'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE 'sales.order.capture' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'sales.order.capture';
DELETE FROM ref.permission WHERE code = 'sales.order.capture';
ALTER TABLE payables.upload DROP CONSTRAINT upload_purpose_check;
ALTER TABLE payables.upload ADD CONSTRAINT upload_purpose_check CHECK (purpose IN ('supplier_invoice'));
DROP INDEX sales.sales_order_one_per_upload;
DROP INDEX partner.tax_identifier_by_fingerprint;
ALTER TABLE sales.sales_order DROP CONSTRAINT sales_order_capture_check, DROP COLUMN capture, DROP COLUMN capture_upload_id;
