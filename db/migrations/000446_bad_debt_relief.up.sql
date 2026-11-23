-- Bad debt relief (ADR-0049 As built, bad debt relief; ADR-0063 As built;
-- 03-data-model §3.26; the tax specialist's confirmation of 2026-11-21):
-- the output tax of a customer invoice written off as a bad debt (a
-- dispute case's write-off, ADR-0065 T86d) is recovered by an adjustment
-- the tax return reports, its own step, never a plain write-off or a short
-- payment. receivables.bad_debt_relief is one such recovery: the invoice,
-- the write-off it relieves (one relief per write-off of an invoice), what
-- that write-off wrote off of the invoice, the tax recovered, the
-- adjustment code it was recovered under, the date the customer was
-- notified where one is given, and the journal entry that posts it. The
-- entry's lines and tax items are the ledger's; this row says which
-- invoice and write-off they relieve.
--
-- Expand only (06 §6.4): a new table the previous version neither reads
-- nor writes. Insert-only: a relief posted stays as it was posted.
CREATE TABLE receivables.bad_debt_relief (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,     -- tenancy context
  customer_invoice_id    uuid NOT NULL,
  write_off_entry_year   smallint NOT NULL,
  write_off_entry_id     uuid NOT NULL,     -- ledger context: the write-off's entry
  write_off_entry_number text NOT NULL CONSTRAINT bad_debt_relief_write_off_number_check CHECK (length(write_off_entry_number) BETWEEN 1 AND 40),
  dispute_case_id        uuid NOT NULL,     -- collections context: the case the write-off resolved
  currency               char(3) NOT NULL REFERENCES ref.currency (code),
  local_currency         char(3) NOT NULL REFERENCES ref.currency (code), -- the company's, which the _lc amounts are in
  written_off_amount     bigint NOT NULL,   -- above the tax, so positive (bad_debt_relief_tax_within)
  written_off_amount_lc  bigint NOT NULL,   -- at least the local tax, so not negative
  tax_amount             bigint NOT NULL CONSTRAINT bad_debt_relief_tax_check CHECK (tax_amount > 0),
  tax_amount_lc          bigint NOT NULL CONSTRAINT bad_debt_relief_tax_lc_check CHECK (tax_amount_lc >= 0),
  tax_code               text NOT NULL CONSTRAINT bad_debt_relief_tax_code_check CHECK (tax_code ~ '^[A-Z0-9]{1,4}$'),
  customer_notified_on   date,
  posting_date           date NOT NULL,
  journal_entry_year     smallint NOT NULL,
  journal_entry_id       uuid NOT NULL,     -- ledger context: the relief's entry
  journal_entry_number   text NOT NULL CONSTRAINT bad_debt_relief_entry_number_check CHECK (length(journal_entry_number) BETWEEN 1 AND 40),
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT bad_debt_relief_invoice_fkey FOREIGN KEY (tenant_id, customer_invoice_id) REFERENCES receivables.customer_invoice (tenant_id, id),
  -- A write-off of an invoice is relieved once.
  CONSTRAINT bad_debt_relief_once UNIQUE (tenant_id, customer_invoice_id, write_off_entry_id),
  CONSTRAINT bad_debt_relief_entry_once UNIQUE (tenant_id, journal_entry_id),
  -- The tax recovered is a part of what was written off, and the customer
  -- is notified before the relief, not after it.
  CONSTRAINT bad_debt_relief_tax_within CHECK (tax_amount < written_off_amount AND tax_amount_lc <= written_off_amount_lc),
  CONSTRAINT bad_debt_relief_notified_before CHECK (customer_notified_on IS NULL OR customer_notified_on <= posting_date)
);
CREATE INDEX bad_debt_relief_by_company ON receivables.bad_debt_relief (tenant_id, legal_entity_id, posting_date, id);
CREATE TRIGGER bad_debt_relief_immutable BEFORE UPDATE OR DELETE ON receivables.bad_debt_relief
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.bad_debt_relief'::regclass);
