-- The table comes back empty: its codes live in tax_code_definition,
-- tax_component and tax_rate.
CREATE INDEX journal_line_tax_code ON ledger.journal_line (tenant_id, legal_entity_id, tax_code, posting_date) WHERE tax_code IS NOT NULL;
CREATE TABLE ledger.tax_code (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),
  description      text NOT NULL,
  direction        text NOT NULL CHECK (direction IN ('input', 'output')),
  rate_pct         numeric(9,6) NOT NULL CHECK (rate_pct >= 0 AND rate_pct < 100),
  account_id       uuid NOT NULL,
  valid_from       date NOT NULL,
  valid_to         date,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  ended_at         timestamptz,
  ended_by         uuid,
  PRIMARY KEY (tenant_id, legal_entity_id, code, valid_from),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, legal_entity_id WITH =, code WITH =, daterange(valid_from, valid_to) WITH &&)
);
CREATE TRIGGER tax_code_end BEFORE UPDATE ON ledger.tax_code FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
SELECT platform.secure_table('ledger.tax_code'::regclass);
GRANT UPDATE (valid_to, ended_by, ended_at) ON ledger.tax_code TO erp_app;
