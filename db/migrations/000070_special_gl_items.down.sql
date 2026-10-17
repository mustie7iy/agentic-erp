DROP TRIGGER account_determination_down_payment_fixed ON ledger.account_determination;
DROP FUNCTION ledger.check_down_payment_fixed();
DROP TRIGGER journal_line_special_gl ON ledger.journal_line;
DROP FUNCTION ledger.check_special_gl_line();
DROP INDEX ledger.journal_line_special_gl;
ALTER TABLE ledger.journal_line DROP CONSTRAINT journal_line_special_gl, DROP COLUMN special_gl;

-- Tenant rows: under a role that does not bypass row-level security this
-- fails rather than delete nothing (06-engineering-process §6.4).
SET LOCAL row_security = off;
DELETE FROM ledger.account_determination WHERE key = 'down_payment';
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding'));
