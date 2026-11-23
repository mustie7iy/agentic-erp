-- Bad debt relief charged again (ADR-0049 As built batch 91; tracker leaf
-- src po:2026-11-21; 03-data-model §3.26): when the customer later pays
-- what a bad debt relief (migration 000446) wrote off, the share of the
-- tax the relief recovered is charged again by an adjustment the return
-- reports, in the transaction that applies the payment (or that undoes
-- the application, charging back less). receivables.bad_debt_recharge is
-- one such adjustment: the invoice, the incoming payment and its
-- application (0: the payment's own) whose applying or undoing moved
-- what was recovered of the written-off amount, that amount as it then
-- stood (cumulative), the tax charged again (negative: charged back less
-- by an undoing), in the invoice's currency and the company's, and the
-- journal entry that posts it. The entry's lines and tax items are the
-- ledger's.
--
-- Expand only (06 §6.4): a new table the previous version neither reads
-- nor writes. Insert-only: an adjustment posted stays as it was posted.
CREATE TABLE receivables.bad_debt_recharge (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,     -- tenancy context
  customer_invoice_id  uuid NOT NULL,
  incoming_payment_id  uuid NOT NULL,
  application_no       integer NOT NULL CONSTRAINT bad_debt_recharge_application_check CHECK (application_no >= 0),
  reset                boolean NOT NULL,  -- written by the undoing of the application, not by applying it
  currency             char(3) NOT NULL REFERENCES ref.currency (code),
  local_currency       char(3) NOT NULL REFERENCES ref.currency (code), -- the company's, which tax_amount_lc is in
  recovered_amount     bigint NOT NULL CONSTRAINT bad_debt_recharge_recovered_check CHECK (recovered_amount >= 0),
  tax_amount           bigint NOT NULL CONSTRAINT bad_debt_recharge_tax_check CHECK (tax_amount <> 0),
  tax_amount_lc        bigint NOT NULL,
  posting_date         date NOT NULL,
  journal_entry_year   smallint NOT NULL,
  journal_entry_id     uuid NOT NULL,     -- ledger context: the adjustment's entry
  journal_entry_number text NOT NULL CONSTRAINT bad_debt_recharge_entry_number_check CHECK (length(journal_entry_number) BETWEEN 1 AND 40),
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT bad_debt_recharge_invoice_fkey FOREIGN KEY (tenant_id, customer_invoice_id) REFERENCES receivables.customer_invoice (tenant_id, id),
  CONSTRAINT bad_debt_recharge_payment_fkey FOREIGN KEY (tenant_id, incoming_payment_id) REFERENCES receivables.incoming_payment (tenant_id, id),
  CONSTRAINT bad_debt_recharge_entry_once UNIQUE (tenant_id, journal_entry_id),
  -- An application, or its undoing, charges an invoice's tax again once.
  CONSTRAINT bad_debt_recharge_once UNIQUE (tenant_id, customer_invoice_id, incoming_payment_id, application_no, reset),
  -- The local tax moves with the tax, or is nothing (an invoice whose
  -- tax items carried none).
  CONSTRAINT bad_debt_recharge_tax_lc_sign CHECK (tax_amount_lc = 0 OR sign(tax_amount_lc) = sign(tax_amount))
);
CREATE INDEX bad_debt_recharge_by_invoice ON receivables.bad_debt_recharge (tenant_id, customer_invoice_id, created_at, id);
CREATE INDEX bad_debt_recharge_by_payment ON receivables.bad_debt_recharge (tenant_id, incoming_payment_id, application_no, created_at, id);
CREATE TRIGGER bad_debt_recharge_immutable BEFORE UPDATE OR DELETE ON receivables.bad_debt_recharge
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.bad_debt_recharge'::regclass);
