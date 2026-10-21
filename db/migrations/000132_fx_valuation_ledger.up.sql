-- Foreign-currency revaluation per ledger (T52d, ADR-0045, ADR-0042): a
-- parallel ledger may value at a rate type of its own, its valuation area's
-- (SAP FAGL_FCV valuation areas per accounting principle); without one it
-- values at its legal entity's (ledger.fx_valuation), as the leading ledger
-- always does. The valuation accounts stay the legal entity's: each run
-- posts in its own ledger alone, so the ledgers keep their amounts apart on
-- the same accounts. Expand only: a new table.
CREATE TABLE ledger.fx_valuation_ledger (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,                  -- tenancy context
  ledger_id        uuid NOT NULL,
  -- Always false: repeats the ledger's, so the foreign key holds the
  -- setting to a parallel ledger.
  is_leading       boolean NOT NULL DEFAULT false CHECK (NOT is_leading),
  rate_type        text NOT NULL CHECK (rate_type IN ('average', 'bank_buying', 'bank_selling', 'planning')),
  is_active        boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it: the legal entity's again
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, ledger_id),
  -- A ledger the legal entity keeps, and not the leading one.
  FOREIGN KEY (tenant_id, legal_entity_id, ledger_id) REFERENCES ledger.company_ledger (tenant_id, legal_entity_id, ledger_id),
  FOREIGN KEY (tenant_id, ledger_id, is_leading) REFERENCES ledger.ledger (tenant_id, id, is_leading)
);
CREATE TRIGGER fx_valuation_ledger_touch BEFORE UPDATE ON ledger.fx_valuation_ledger FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.fx_valuation_ledger'::regclass);
