-- Payment methods, the house banks ranked to pay with them, and the methods
-- a supplier may be paid by (T45b; ADR-0038; 03-data-model §3.6, §3.10). The
-- methods and rankings are the configuration's companies[].payment_methods
-- (SAP T042Z, T042E/T042I), kept per legal entity by payments.PaymentMethods;
-- a supplier's methods are its company data (SAP LFB1-ZWELS), in order,
-- changed through partner changes, each a method of its company.
CREATE TABLE payments.payment_method (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]$'),
  description      text NOT NULL,
  is_active        boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, code)
);
CREATE TRIGGER payment_method_touch BEFORE UPDATE ON payments.payment_method FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('payments.payment_method'::regclass);

-- The house banks, by code and first preferred, that pay with a method in
-- a currency. The codes are the legal entity's payments.house_bank codes,
-- which are never deleted; the configuration ranks only banks it lists.
CREATE TABLE payments.bank_ranking (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  method           text NOT NULL,
  currency         char(3) NOT NULL REFERENCES ref.currency,
  house_banks      text[] NOT NULL CHECK (cardinality(house_banks) > 0),
  is_active        boolean NOT NULL DEFAULT true,  -- false once the configuration no longer ranks banks for the currency
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, method, currency),
  FOREIGN KEY (tenant_id, legal_entity_id, method) REFERENCES payments.payment_method (tenant_id, legal_entity_id, code)
);
CREATE TRIGGER bank_ranking_touch BEFORE UPDATE ON payments.bank_ranking FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('payments.bank_ranking'::regclass);

-- The supplier's methods in its order of preference: up to ten codes. The
-- default needs no rewrite; the check is added NOT VALID and validated by
-- 000091 (06 §6.4).
ALTER TABLE partner.supplier_company ADD COLUMN payment_methods text[] NOT NULL DEFAULT '{}';
ALTER TABLE partner.supplier_company ADD CONSTRAINT supplier_company_payment_methods
  CHECK (cardinality(payment_methods) = 0 OR array_to_string(payment_methods, ',', '?') ~ '^[A-Z0-9](,[A-Z0-9]){0,9}$') NOT VALID;
