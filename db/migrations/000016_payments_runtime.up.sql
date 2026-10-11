-- Runtime support for payments (T5).

-- A payment settles the supplier's due items net: credit memos and other
-- debit items are settled with the invoices, at a negative amount. The
-- payment itself stays positive.
ALTER TABLE payments.payment_item DROP CONSTRAINT payment_item_amount_check;
ALTER TABLE payments.payment_item ADD CONSTRAINT payment_item_amount_check CHECK (amount <> 0);

-- Releasing a run posts each payment and records its journal entry, once.
CREATE FUNCTION payments.check_payment_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.journal_entry_id IS NOT NULL THEN
    RAISE EXCEPTION 'payment % is already posted', OLD.end_to_end_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION payments.check_payment_update() OWNER TO erp_owner;
CREATE TRIGGER payment_posted_once BEFORE UPDATE ON payments.payment FOR EACH ROW EXECUTE FUNCTION payments.check_payment_update();
GRANT UPDATE (journal_entry_year, journal_entry_id) ON payments.payment TO erp_app;

CREATE INDEX payment_run_operation ON payments.payment_run (tenant_id, operation_id) WHERE operation_id IS NOT NULL;

-- The proposal worker finds queued proposals across tenants and then builds
-- each one as the proposer, in the tenant's scope. Finding them is all this
-- role may do.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_worker') THEN CREATE ROLE erp_worker NOLOGIN BYPASSRLS; END IF;
END $$;
GRANT USAGE ON SCHEMA platform, payments TO erp_worker;
GRANT SELECT (tenant_id, id, kind, status) ON platform.operation TO erp_worker;
GRANT SELECT (tenant_id, operation_id, status) ON payments.payment_run TO erp_worker;
