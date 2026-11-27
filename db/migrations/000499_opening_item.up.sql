-- The open vendor items of a migration's opening entries (ADR-0034 As
-- built, items cleared in a later fiscal year; 03-data-model §3.13). A
-- balances run posts a company's open vendor items as lines of one
-- opening entry; a later fiscal year's run clears an item SAP cleared in
-- that year against the line that holds it, which this table names by
-- the SAP line's key (<BUKRS>/<GJAHR>/<BELNR>/<DOCLN>), since the
-- opening entry's line numbers cannot be told from the later year's
-- extract. journal_entry_id is the ledger context's: no foreign key
-- leaves the schema (02 §2.1). Insert-only: no version column, so the
-- application may only insert (platform.secure_table). Expand only: a new
-- table, which the previous version neither reads nor writes.
CREATE TABLE integration.opening_item (
  tenant_id        uuid NOT NULL,
  system           text NOT NULL CHECK (system ~ '^[A-Z0-9_]{2,20}$'),
  external_key     text NOT NULL CHECK (length(external_key) BETWEEN 1 AND 100),
  journal_entry_id uuid NOT NULL,   -- ledger context: the opening entry
  fiscal_year      smallint NOT NULL,
  line_no          integer NOT NULL CHECK (line_no > 0),
  migration_run_id uuid NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, system, external_key),
  -- A line of an entry holds one item.
  UNIQUE (tenant_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, migration_run_id) REFERENCES integration.migration_run (tenant_id, id)
);
SELECT platform.secure_table('integration.opening_item'::regclass);
