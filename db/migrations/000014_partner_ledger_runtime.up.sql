-- Runtime support for the partner and ledger contexts (T3).

-- Journal entries draw their numbers from ranges named after the document
-- type (journal_sa, journal_kr, ...), and document types may contain digits.
ALTER TABLE config.number_range DROP CONSTRAINT number_range_document_type_check;
ALTER TABLE config.number_range ADD CONSTRAINT number_range_document_type_check CHECK (document_type ~ '^[a-z][a-z0-9_]*$');

-- The configuration document keeps one chart of accounts per company, and
-- the chart is named after the company code.
ALTER TABLE ledger.chart_of_accounts DROP CONSTRAINT chart_of_accounts_code_check;
ALTER TABLE ledger.chart_of_accounts ADD CONSTRAINT chart_of_accounts_code_check CHECK (code ~ '^[A-Z0-9]{1,10}$');

-- The API addresses journal entries by id alone; the primary key leads with
-- the fiscal year (the partition key).
CREATE INDEX journal_entry_by_id ON ledger.journal_entry (tenant_id, id);

-- Entries waiting for the period-balance projector (ADR-0006). Posting
-- inserts one row next to the entry's JournalEntryPosted event; the
-- projector adds the entry to ledger.period_balance and sets projected_at
-- in one transaction, so every entry is counted exactly once. Balance reads
-- add the entries still pending, so they are exact without waiting.
CREATE TABLE ledger.balance_projection (
  tenant_id         uuid NOT NULL,
  event_id          uuid NOT NULL,             -- the JournalEntryPosted event in ledger.outbox
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  enqueued_at       timestamptz NOT NULL DEFAULT now(),
  projected_at      timestamptz,
  PRIMARY KEY (tenant_id, event_id),
  UNIQUE (tenant_id, fiscal_year, journal_entry_id),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id)
);
CREATE INDEX balance_projection_queue ON ledger.balance_projection (enqueued_at) WHERE projected_at IS NULL;
CREATE INDEX balance_projection_pending ON ledger.balance_projection (tenant_id, fiscal_year) WHERE projected_at IS NULL;
SELECT platform.secure_table('ledger.balance_projection');

-- The trial balance reads one legal entity, ledger, year and period; the
-- primary key leads with the account.
CREATE INDEX period_balance_by_period ON ledger.period_balance (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, account_id);

-- The projector works for every tenant, so like the outbox relay it
-- bypasses row-level security; it can read the journal and maintain
-- balances, nothing else.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_projector') THEN CREATE ROLE erp_projector NOLOGIN BYPASSRLS; END IF;
END $$;
GRANT USAGE ON SCHEMA ledger TO erp_projector;
GRANT SELECT ON ledger.journal_entry, ledger.journal_line TO erp_projector;
GRANT SELECT, UPDATE (projected_at) ON ledger.balance_projection TO erp_projector;
GRANT SELECT, INSERT, UPDATE ON ledger.period_balance TO erp_projector;

-- A new address replaces the current one of the same purpose by ending it,
-- the way bank accounts are replaced.
ALTER TABLE partner.address ADD COLUMN ended_at timestamptz, ADD COLUMN ended_by uuid;
CREATE TRIGGER address_end BEFORE UPDATE ON partner.address FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
GRANT UPDATE (valid_to, ended_by, ended_at) ON partner.address TO erp_app;

-- Tax identifiers are replaced in place (one per scheme), so they carry the
-- standard change columns.
ALTER TABLE partner.tax_identifier ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN updated_by uuid, ADD COLUMN version integer NOT NULL DEFAULT 1;
UPDATE partner.tax_identifier SET updated_by = created_by;
ALTER TABLE partner.tax_identifier ALTER COLUMN updated_by SET NOT NULL;
CREATE TRIGGER tax_identifier_touch BEFORE UPDATE ON partner.tax_identifier FOR EACH ROW EXECUTE FUNCTION platform.touch();
GRANT UPDATE ON partner.tax_identifier TO erp_app;
