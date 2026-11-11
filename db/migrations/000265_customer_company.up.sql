-- A customer's data in a company (T74a; 03-data-model §3.6): the
-- reconciliation account its receivables post to, its payment term and
-- whether postings to it are blocked there. One row per customer and legal
-- entity, as partner.supplier_company (000004) is for a supplier: one
-- business partner plays both roles, with company data per role. The row
-- needs the customer role (the foreign key to partner_role through
-- role_customer) and a payment term of the tenant; the legal entity is the
-- tenancy context's and the account the ledger's, so no foreign key leaves
-- the schema: partner checks both when the data is requested. Expand only:
-- a new table, which the previous version neither reads nor writes.
CREATE TABLE partner.customer_company (
  tenant_id                  uuid NOT NULL,
  partner_id                 uuid NOT NULL,
  legal_entity_id            uuid NOT NULL,      -- tenancy context
  reconciliation_account_id  uuid NOT NULL,      -- ledger context
  payment_term_code          text NOT NULL,
  is_posting_blocked         boolean NOT NULL DEFAULT false,
  role_customer              text NOT NULL DEFAULT 'customer' CHECK (role_customer = 'customer'),  -- lets the FK require the customer role
  created_at                 timestamptz NOT NULL DEFAULT now(),
  created_by                 uuid NOT NULL,
  updated_at                 timestamptz NOT NULL DEFAULT now(),
  updated_by                 uuid NOT NULL,
  version                    integer NOT NULL DEFAULT 1 CHECK (version >= 1),
  PRIMARY KEY (tenant_id, partner_id, legal_entity_id),
  FOREIGN KEY (tenant_id, partner_id, role_customer) REFERENCES partner.partner_role (tenant_id, partner_id, role),
  FOREIGN KEY (tenant_id, payment_term_code) REFERENCES partner.payment_term (tenant_id, code)
);
CREATE TRIGGER customer_company_touch BEFORE UPDATE ON partner.customer_company FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('partner.customer_company'::regclass);
