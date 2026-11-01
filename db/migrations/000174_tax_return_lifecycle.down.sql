DELETE FROM identity.sod_rule WHERE permission_a IN ('tax.return.prepare', 'tax.return.approve', 'tax.return.file')
  OR permission_b IN ('tax.return.prepare', 'tax.return.approve', 'tax.return.file');
DELETE FROM identity.role_permission WHERE permission_code IN ('tax.return.prepare', 'tax.return.approve', 'tax.return.file');
DELETE FROM ref.permission WHERE code IN ('tax.return.prepare', 'tax.return.approve', 'tax.return.file');
DROP TRIGGER tax_item_reported ON ledger.tax_item;
DROP FUNCTION tax.check_item_reported();
DROP INDEX ledger.tax_item_reported_in;
CREATE OR REPLACE FUNCTION ledger.check_tax_item_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.reported_in IS NOT NULL OR NEW.reported_in IS NULL
     OR to_jsonb(NEW) - 'reported_in' IS DISTINCT FROM to_jsonb(OLD) - 'reported_in' THEN
    RAISE EXCEPTION 'tax item % of entry % is fixed but for the return that reports it, set once', OLD.item_no, OLD.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_fixed';
  END IF;
  RETURN NEW;
END $$;
DROP TABLE tax.tax_return_item;
DROP TABLE tax.tax_return_box;
DROP FUNCTION tax.check_snapshot_part();
DROP TABLE tax.tax_return;
DROP FUNCTION tax.check_tax_return();
REVOKE ALL ON SCHEMA tax FROM erp_relay;
DROP TABLE tax.inbox;
DROP TABLE tax.outbox;
