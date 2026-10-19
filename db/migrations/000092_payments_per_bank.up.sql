-- Proposals and bank files per house bank and payment method (T45c;
-- ADR-0038; 03-data-model §3.10). A run proposed without a house bank pays
-- each supplier through the first of its payment methods that a ranked
-- house bank of the run's currency pays with: its payments and bank files
-- name that bank and method, one file per bank and method. A run proposed
-- for one bank keeps it, and its payments and file leave both null: null
-- is the run's bank, so the rows written before need no backfill and a
-- release by the previous version still writes its one file under the
-- unique index 000094 builds. The previous version cannot release a run
-- without a bank, so such proposals wait until the upgrade completes.
-- Constraints are added NOT VALID and validated by 000093 (06 §6.4). It
-- backfills house_bank, so row-level
-- security is off, as for every migration that changes tenant rows.
SET LOCAL row_security = off;
ALTER TABLE payments.payment_run ALTER COLUMN house_bank_id DROP NOT NULL;

ALTER TABLE payments.payment ADD COLUMN house_bank_id uuid, ADD COLUMN payment_method text;
ALTER TABLE payments.payment ADD CONSTRAINT payment_house_bank
  FOREIGN KEY (tenant_id, house_bank_id) REFERENCES payments.house_bank (tenant_id, id) NOT VALID;
ALTER TABLE payments.payment ADD CONSTRAINT payment_method_of_bank
  CHECK (payment_method IS NULL OR (payment_method ~ '^[A-Z0-9]$' AND house_bank_id IS NOT NULL)) NOT VALID;

ALTER TABLE payments.bank_file ADD COLUMN house_bank_id uuid, ADD COLUMN payment_method text;
ALTER TABLE payments.bank_file ADD CONSTRAINT bank_file_house_bank
  FOREIGN KEY (tenant_id, house_bank_id) REFERENCES payments.house_bank (tenant_id, id) NOT VALID;
ALTER TABLE payments.bank_file ADD CONSTRAINT bank_file_method_of_bank
  CHECK (payment_method IS NULL OR (payment_method ~ '^[A-Z0-9]$' AND house_bank_id IS NOT NULL)) NOT VALID;

-- A supplier, or a down payment request, that no ranked house bank can pay.
ALTER TABLE payments.payment_exclusion DROP CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.payment_exclusion ADD CONSTRAINT payment_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open', 'no_house_bank')) NOT VALID;
ALTER TABLE payments.request_exclusion DROP CONSTRAINT request_exclusion_reason_code_check;
ALTER TABLE payments.request_exclusion ADD CONSTRAINT request_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'tax_code_invalid', 'no_house_bank')) NOT VALID;

-- At most one active house bank per ledger account (house_bank_account_once,
-- 000096-000097): the account while the bank is active, null otherwise,
-- written by the application (payments.HouseBanks). The previous version
-- does not write it, so while it still runs its configuration changes to
-- house banks are refused by the check; configuration changes wait until
-- the upgrade completes (06 §6.4). house_bank holds a few rows per legal
-- entity (the configuration's banks), so the backfill is one statement.
ALTER TABLE payments.house_bank ADD COLUMN active_gl_account_id uuid;
UPDATE payments.house_bank SET active_gl_account_id = gl_account_id, version = version + 1 WHERE status = 'active';
ALTER TABLE payments.house_bank ADD CONSTRAINT house_bank_active_account
  CHECK (active_gl_account_id IS NOT DISTINCT FROM CASE WHEN status = 'active' THEN gl_account_id END) NOT VALID;
