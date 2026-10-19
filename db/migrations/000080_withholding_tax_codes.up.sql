-- Withholding tax codes and the suppliers they apply to (T46e;
-- 03-data-model §3.6, §3.7). The codes are the configuration's
-- companies[].withholding_tax_codes, kept per legal entity by the ledger's
-- setup (SAP T059P/T059Z); a supplier's codes per company are partner data
-- (SAP LFBW), changed through partner changes, each a code of its company.
CREATE TABLE ledger.withholding_tax_code (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),
  description      text NOT NULL,
  rate_pct         numeric(6,3) NOT NULL CHECK (rate_pct > 0 AND rate_pct < 100),
  base             text NOT NULL CHECK (base IN ('net', 'gross')),
  account_id       uuid NOT NULL,       -- the withholding tax payable
  posting          text NOT NULL CHECK (posting IN ('invoice', 'payment')),
  is_active        boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, code),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id)
);
CREATE TRIGGER withholding_tax_code_touch BEFORE UPDATE ON ledger.withholding_tax_code FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.withholding_tax_code'::regclass);
-- A code the configuration no longer has is deactivated, never deleted;
-- the partner context refuses that while a supplier has it
-- (partner.WithholdingCodes).

CREATE TABLE partner.supplier_withholding (
  tenant_id        uuid NOT NULL,
  partner_id       uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),  -- a ledger.withholding_tax_code of the legal entity
  is_active        boolean NOT NULL DEFAULT true,  -- false once a partner change took it off the supplier
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, partner_id, legal_entity_id, code),
  FOREIGN KEY (tenant_id, partner_id, legal_entity_id) REFERENCES partner.supplier_company (tenant_id, partner_id, legal_entity_id)
);
-- The configuration check finds the suppliers of a code.
CREATE INDEX supplier_withholding_by_code ON partner.supplier_withholding (tenant_id, legal_entity_id, code) WHERE is_active;
CREATE TRIGGER supplier_withholding_touch BEFORE UPDATE ON partner.supplier_withholding FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('partner.supplier_withholding'::regclass);
