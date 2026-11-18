-- Payments on account (T75g; ADR-0063 §6 As built; 03-data-model §3.26):
-- what a receipt leaves after the items it is applied to, or the whole
-- receipt applied to none, is posted to the customer's reconciliation
-- account as a credit of the customer's, a line of the payment's clearing
-- entry: on_account_amount is it, in the payment's currency,
-- on_account_line_no its line and on_account_account_id the account (the
-- ledger context's). A later application applies that credit to the
-- customer's open items, leaving what they do not take on account again
-- as a credit line of its own clearing entry: one row of
-- incoming_payment_application per application, numbered in the payment
-- from 1, each consuming the credit line the one before left (the
-- payment's own for the first). The items an application applied are the
-- payment's items with its application_no (0: applied with the receipt).
--
-- Expand only (06 §6.4): defaulted or nullable columns and a new table,
-- which the previous version neither reads nor writes.
ALTER TABLE receivables.incoming_payment
  ADD COLUMN on_account_amount bigint NOT NULL DEFAULT 0,
  ADD COLUMN on_account_line_no integer,
  ADD COLUMN on_account_account_id uuid,     -- ledger context: the customer's reconciliation account
  -- What is on account is more than zero with its line and account, or
  -- zero without either.
  ADD CONSTRAINT incoming_payment_on_account CHECK (
    on_account_amount >= 0 AND (on_account_amount = 0) = (on_account_line_no IS NULL) AND (on_account_line_no IS NULL) = (on_account_account_id IS NULL)
    AND (on_account_line_no IS NULL OR on_account_line_no > 0));

ALTER TABLE receivables.incoming_payment_item
  ADD COLUMN application_no integer NOT NULL DEFAULT 0 CONSTRAINT incoming_payment_item_application_no_check CHECK (application_no >= 0);

-- An application of a payment's credit on account: the credit line it
-- consumed (credit_*), amount, what that line stood at; difference, what
-- the items, net of their residuals, exceed the credit by and was written
-- off; what it left on account, with its line in the clearing entry.
CREATE TABLE receivables.incoming_payment_application (
  tenant_id             uuid NOT NULL,
  incoming_payment_id   uuid NOT NULL,
  application_no        integer NOT NULL CHECK (application_no > 0),
  posting_date          date NOT NULL,
  credit_entry_year     smallint NOT NULL,
  credit_entry_id       uuid NOT NULL,       -- ledger context: the entry whose line was the credit
  credit_line_no        integer NOT NULL CHECK (credit_line_no > 0),
  amount                bigint NOT NULL CONSTRAINT incoming_payment_application_amount_positive CHECK (amount > 0),
  difference            bigint NOT NULL DEFAULT 0,
  on_account_amount     bigint NOT NULL,
  on_account_line_no    integer,
  clearing_entry_year   smallint NOT NULL,
  clearing_entry_id     uuid NOT NULL,       -- ledger context: the entry that cleared the credit and the items
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  PRIMARY KEY (tenant_id, incoming_payment_id, application_no),
  FOREIGN KEY (tenant_id, incoming_payment_id) REFERENCES receivables.incoming_payment (tenant_id, id),
  CONSTRAINT incoming_payment_application_on_account CHECK (
    on_account_amount >= 0 AND (on_account_amount = 0) = (on_account_line_no IS NULL) AND (on_account_line_no IS NULL OR on_account_line_no > 0)),
  -- A credit line is consumed once, and a clearing entry is one
  -- application's.
  CONSTRAINT incoming_payment_application_credit_once UNIQUE (tenant_id, credit_entry_id, credit_line_no),
  CONSTRAINT incoming_payment_application_entry_once UNIQUE (tenant_id, clearing_entry_id)
);
CREATE TRIGGER incoming_payment_application_immutable BEFORE UPDATE OR DELETE ON receivables.incoming_payment_application
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.incoming_payment_application'::regclass);
