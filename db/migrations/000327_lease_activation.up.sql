-- The lease schedule and the contract activated (T88b; ADR-0073 §7-§9;
-- 03-data-model §3.33): activating a draft lease contract posts the
-- right-of-use asset against the lease liability on the commencement
-- date and keeps the schedule the amounts are taken from, so what later
-- runs post is what was shown. The ledger's account determination takes
-- the two accounts the activation posts to. Expand only (06 §6.4): nullable
-- columns, a new table, a wider status, two more determination keys, one
-- more source context of the journal and one permission, which the
-- previous version neither reads nor writes.

-- An active contract keeps what its activation measured and posted, in
-- minor units of its currency: the lease liability at commencement (the
-- present value of the payments not paid on the commencement date) and
-- the right-of-use asset (that and a payment on the commencement date),
-- and the journal entry that posted them. A draft has none of them.
ALTER TABLE leases.contract
  DROP CONSTRAINT contract_status_check,
  ADD CONSTRAINT contract_status_check CHECK (status IN ('draft', 'active')),
  ADD COLUMN lease_liability bigint CONSTRAINT contract_lease_liability_check CHECK (lease_liability >= 0),
  ADD COLUMN right_of_use_amount bigint CONSTRAINT contract_right_of_use_amount_check CHECK (right_of_use_amount > 0),
  ADD COLUMN journal_entry_id uuid,        -- ledger context
  ADD COLUMN journal_entry_year smallint,
  ADD COLUMN journal_entry_number text CONSTRAINT contract_journal_entry_number_check CHECK (length(journal_entry_number) BETWEEN 1 AND 40),
  ADD CONSTRAINT contract_activation_check CHECK ((status = 'active') = (lease_liability IS NOT NULL)
    AND (status = 'active') = (right_of_use_amount IS NOT NULL) AND (status = 'active') = (journal_entry_id IS NOT NULL)
    AND (status = 'active') = (journal_entry_year IS NOT NULL) AND (status = 'active') = (journal_entry_number IS NOT NULL)),
  ADD CONSTRAINT contract_right_of_use_covers_liability CHECK (right_of_use_amount >= lease_liability);

-- An active contract is fixed: activating it is its last change.
CREATE FUNCTION leases.check_contract_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' THEN
    RAISE EXCEPTION 'lease contract %: only a draft changes (lease_contract_fixed_once_active)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'lease_contract_fixed_once_active';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION leases.check_contract_change() OWNER TO erp_owner;
CREATE TRIGGER lease_contract_fixed_once_active BEFORE UPDATE ON leases.contract
  FOR EACH ROW EXECUTE FUNCTION leases.check_contract_change();
GRANT UPDATE (status, version, updated_by, lease_liability, right_of_use_amount, journal_entry_id, journal_entry_year, journal_entry_number)
  ON leases.contract TO erp_app;

-- The schedule an active contract keeps (ADR-0073 §8), one line per
-- payment interval, numbered as its payment: the interval's dates, the
-- liability at its start (opening) and end (closing), the payment due in
-- it, the interest (closing - opening + payment), the right-of-use
-- asset's depreciation and its carrying amount at the interval's end, in
-- minor units of the contract's currency. Written once, at activation;
-- insert-only (no version, so no update grant, and no delete).
CREATE TABLE leases.schedule_line (
  tenant_id             uuid NOT NULL,
  contract_id           uuid NOT NULL,
  line_no               integer NOT NULL CONSTRAINT schedule_line_no_check CHECK (line_no BETWEEN 1 AND 600),
  period_start          date NOT NULL,
  period_end            date NOT NULL,
  opening_liability     bigint NOT NULL CONSTRAINT schedule_line_opening_check CHECK (opening_liability >= 0),
  payment               bigint NOT NULL CONSTRAINT schedule_line_payment_check CHECK (payment >= 0),
  interest              bigint NOT NULL,
  closing_liability     bigint NOT NULL CONSTRAINT schedule_line_closing_check CHECK (closing_liability >= 0),
  depreciation          bigint NOT NULL CONSTRAINT schedule_line_depreciation_check CHECK (depreciation >= 0),
  right_of_use_closing  bigint NOT NULL CONSTRAINT schedule_line_right_of_use_check CHECK (right_of_use_closing >= 0),
  PRIMARY KEY (tenant_id, contract_id, line_no),
  CONSTRAINT schedule_line_contract_fkey FOREIGN KEY (tenant_id, contract_id) REFERENCES leases.contract (tenant_id, id),
  CONSTRAINT schedule_line_period_check CHECK (period_start <= period_end),
  -- The liability moves by its interest and its payment, nothing else.
  CONSTRAINT schedule_line_balance_check CHECK (closing_liability = opening_liability + interest - payment)
);
SELECT platform.secure_table('leases.schedule_line'::regclass);

-- The accounts a lease's activation posts to (ADR-0073 §7), and the
-- journal's source contexts gain leases, whose activation posts through
-- ledger.Postings. Each wider check replaces the narrower one NOT VALID,
-- which reads no row under the lock; every row satisfied the narrower
-- check, and 000328 validates the new ones (06 §6.4). Added to the
-- journal's parent, the check is each partition's too.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability')) NOT VALID;
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling', 'revenue', 'leases')) NOT VALID;

-- Activating lease contracts (ADR-0073 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('leases.contract.activate', 'Activate lease contracts, which posts the right-of-use asset against the lease liability and fixes the schedule', false);
