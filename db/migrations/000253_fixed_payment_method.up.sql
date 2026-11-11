-- An item's fixed payment method (ADR-0038 As built, "an item's fixed
-- payment method"; SAP BSEG-ZLSCH, SAP specialist 2026-11-09 A4(c);
-- product owner 2026-11-10): a supplier invoice or credit memo may fix
-- the method its open item is paid by. A payment run pays a supplier's
-- items fixing a method in a payment of their own by that method, beside
-- the payment of its items that fix none, and leaves them out with a
-- reason when the method cannot pay them. Expand only (06 §6.4): the
-- checks are added NOT VALID and validated by 000256, the unique indexes
-- that hold one items payment per supplier and fixed method are built
-- concurrently (000254, 000255) before 000257 drops the index they
-- replace. A replica of the release before writes none of these, so its
-- rows keep their meaning: no fixed method.

-- The method the document fixes, while parked and after: one character,
-- as every method reference (payments.payment.payment_method, 000093).
-- That it is an active method of the company is the payables context's
-- check through the payments context's port, as a supplier's methods are
-- checked (000091): no foreign key crosses contexts.
ALTER TABLE payables.supplier_invoice ADD COLUMN payment_method text;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_payment_method
  CHECK (payment_method ~ '^[A-Z0-9]$') NOT VALID;

-- The method frozen with the open item at posting, as its terms are
-- (000068): written in the posting transaction and never changed. A table
-- of its own, so the partitioned journal is not altered; an item that
-- fixes no method has no row.
CREATE TABLE ledger.open_item_payment_method (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL,
  payment_method    text NOT NULL CHECK (payment_method ~ '^[A-Z0-9]$'),  -- payments context: the company's method
  created_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no),
  CONSTRAINT open_item_payment_method_line_fkey FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no)
    REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no)
);
CREATE TRIGGER open_item_payment_method_immutable BEFORE UPDATE OR DELETE ON ledger.open_item_payment_method
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.open_item_payment_method'::regclass);

-- A payment of items that fix its method: only an items payment, and it
-- names the method. The default is what every row written before means.
ALTER TABLE payments.payment ADD COLUMN method_fixed boolean NOT NULL DEFAULT false;
ALTER TABLE payments.payment ADD CONSTRAINT payment_method_fixed
  CHECK (NOT method_fixed OR (kind = 'items' AND payment_method IS NOT NULL)) NOT VALID;

-- A supplier's items fixing a method that a run leaves out, or whose
-- payment its release drops: one row per run, supplier and fixed method,
-- apart from payment_exclusion, which keeps the supplier's items that fix
-- none, one row per run and supplier as before (as request_exclusion
-- keeps the requests, 000080). id is the row's key in the listing.
CREATE TABLE payments.method_exclusion (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  payment_run_id  uuid NOT NULL,
  supplier_id     uuid NOT NULL,  -- partner context
  payment_method  text NOT NULL CHECK (payment_method ~ '^[A-Z0-9]$'),
  reason_code     text NOT NULL CHECK (reason_code IN ('supplier_blocked', 'payment_blocked', 'no_bank_account', 'bank_change_pending',
                    'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open', 'no_house_bank', 'method_not_active',
                    'method_not_allowed', 'fixed_method_in_bank_run')),
  amount          bigint NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT method_exclusion_once UNIQUE (tenant_id, payment_run_id, supplier_id, payment_method),
  FOREIGN KEY (tenant_id, payment_run_id) REFERENCES payments.payment_run (tenant_id, id)
);
SELECT platform.secure_table('payments.method_exclusion'::regclass);
