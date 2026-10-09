-- Business partners (03-data-model §3.6)

CREATE TABLE partner.payment_term (
  tenant_id     uuid NOT NULL,
  code          text NOT NULL CHECK (code ~ '^[A-Z0-9_/]{1,10}$'),
  description   text NOT NULL,
  net_days      smallint NOT NULL CHECK (net_days BETWEEN 0 AND 365),
  discount_days smallint CHECK (discount_days BETWEEN 0 AND 365),
  discount_pct  numeric(9,6) CHECK (discount_pct > 0 AND discount_pct < 100),
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, code),
  CHECK ((discount_days IS NULL) = (discount_pct IS NULL)),
  CHECK (discount_days IS NULL OR discount_days < net_days)
);
CREATE TRIGGER payment_term_touch BEFORE UPDATE ON partner.payment_term FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE partner.business_partner (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  number        text NOT NULL CHECK (number ~ '^[A-Z0-9-]{1,20}$'),
  kind          text NOT NULL CHECK (kind IN ('organization', 'person')),
  name          text NOT NULL CHECK (length(trim(name)) > 0),
  name_norm     text GENERATED ALWAYS AS (upper(regexp_replace(name, '\s+', ' ', 'g'))) STORED,
  legal_name    text,
  country_code  char(2) NOT NULL REFERENCES ref.country,
  language      text CHECK (language ~ '^[a-z]{2}$'),
  status        text NOT NULL CHECK (status IN ('pending', 'active', 'blocked', 'inactive')),
  extensions    jsonb NOT NULL DEFAULT '{}',
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, number)
);
CREATE INDEX business_partner_name ON partner.business_partner (tenant_id, name_norm);
CREATE TRIGGER business_partner_touch BEFORE UPDATE ON partner.business_partner FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE partner.partner_role (
  tenant_id   uuid NOT NULL,
  partner_id  uuid NOT NULL,
  role        text NOT NULL CHECK (role IN ('supplier', 'customer')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, partner_id, role),
  FOREIGN KEY (tenant_id, partner_id) REFERENCES partner.business_partner (tenant_id, id)
);

CREATE TABLE partner.supplier_company (
  tenant_id                  uuid NOT NULL,
  partner_id                 uuid NOT NULL,
  legal_entity_id            uuid NOT NULL,      -- tenancy context
  reconciliation_account_id  uuid NOT NULL,      -- ledger context
  payment_term_code          text NOT NULL,
  is_posting_blocked         boolean NOT NULL DEFAULT false,
  is_payment_blocked         boolean NOT NULL DEFAULT false,
  role_supplier              text NOT NULL DEFAULT 'supplier' CHECK (role_supplier = 'supplier'),  -- lets the FK require the supplier role
  created_at                 timestamptz NOT NULL DEFAULT now(),
  created_by                 uuid NOT NULL,
  updated_at                 timestamptz NOT NULL DEFAULT now(),
  updated_by                 uuid NOT NULL,
  version                    integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, partner_id, legal_entity_id),
  FOREIGN KEY (tenant_id, partner_id, role_supplier) REFERENCES partner.partner_role (tenant_id, partner_id, role),
  FOREIGN KEY (tenant_id, payment_term_code) REFERENCES partner.payment_term (tenant_id, code)
);
CREATE TRIGGER supplier_company_touch BEFORE UPDATE ON partner.supplier_company FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE partner.address (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  partner_id    uuid NOT NULL,
  purpose       text NOT NULL CHECK (purpose IN ('legal', 'remit_to', 'order_from', 'ship_from')),
  line1         text NOT NULL,
  line2         text,
  city          text NOT NULL,
  postal_code   text,
  region        text,
  country_code  char(2) NOT NULL REFERENCES ref.country,
  valid_from    date NOT NULL,
  valid_to      date,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, partner_id) REFERENCES partner.business_partner (tenant_id, id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, partner_id WITH =, purpose WITH =, daterange(valid_from, valid_to) WITH &&)
);

-- Bank accounts are effective-dated and encrypted (field-level encryption by
-- the application; the database never sees plaintext). A change is a new row;
-- valid_from drives the payment hold after bank changes.
CREATE TABLE partner.bank_account (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  partner_id           uuid NOT NULL,
  purpose              text NOT NULL DEFAULT 'payments' CHECK (purpose IN ('payments', 'collections')),
  account_cipher       bytea NOT NULL,
  account_fingerprint  text NOT NULL CHECK (account_fingerprint ~ '^[0-9a-f]{64}$'),   -- HMAC-SHA256
  account_last4        text NOT NULL CHECK (account_last4 ~ '^[A-Z0-9]{1,4}$'),
  account_scheme       text NOT NULL CHECK (account_scheme IN ('iban', 'domestic')),
  bic                  text CHECK (bic ~ '^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$'),
  bank_country_code    char(2) NOT NULL REFERENCES ref.country,
  holder_name          text NOT NULL,
  valid_from           date NOT NULL,
  valid_to             date,
  partner_change_id    uuid NOT NULL,          -- the approved change that introduced it
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  ended_at             timestamptz,            -- set when an approved change ends this account
  ended_by             uuid,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, partner_id) REFERENCES partner.business_partner (tenant_id, id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, partner_id WITH =, purpose WITH =, daterange(valid_from, valid_to) WITH &&)
);
-- Same account on two partners is a classic fraud signal; this index makes it fast to find.
CREATE TRIGGER bank_account_end BEFORE UPDATE ON partner.bank_account FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
CREATE INDEX bank_account_fingerprint ON partner.bank_account (tenant_id, account_fingerprint);

CREATE TABLE partner.tax_identifier (
  tenant_id          uuid NOT NULL,
  partner_id         uuid NOT NULL,
  scheme             text NOT NULL CHECK (scheme ~ '^[A-Z]{2}_[A-Z0-9_]+$'),   -- e.g. EU_VAT, US_EIN
  value_cipher       bytea NOT NULL,
  value_fingerprint  text NOT NULL CHECK (value_fingerprint ~ '^[0-9a-f]{64}$'),
  value_last4        text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, partner_id, scheme),
  FOREIGN KEY (tenant_id, partner_id) REFERENCES partner.business_partner (tenant_id, id)
);
CREATE INDEX tax_identifier_fingerprint ON partner.tax_identifier (tenant_id, scheme, value_fingerprint);

-- Governed changes (SAP MDG): applied only after approval.
CREATE TABLE partner.partner_change (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  number               text NOT NULL,
  kind                 text NOT NULL CHECK (kind IN ('create', 'update', 'bank_change', 'block', 'unblock')),
  partner_id           uuid NOT NULL,
  payload              jsonb NOT NULL,          -- sensitive fields arrive encrypted
  reason               text NOT NULL,
  warnings             jsonb NOT NULL DEFAULT '[]',
  status               text NOT NULL CHECK (status IN ('pending', 'applied', 'rejected', 'cancelled')),
  workflow_request_id  uuid,                    -- workflow context
  requested_by         uuid NOT NULL,
  applied_at           timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, number),
  -- A 'create' change refers to the partner row it created in status 'pending'.
  FOREIGN KEY (tenant_id, partner_id) REFERENCES partner.business_partner (tenant_id, id),
  CHECK ((status = 'applied') = (applied_at IS NOT NULL))
);
-- At most one pending change of each kind per partner.
CREATE UNIQUE INDEX partner_change_one_pending ON partner.partner_change (tenant_id, partner_id, kind) WHERE status = 'pending';
CREATE TRIGGER partner_change_touch BEFORE UPDATE ON partner.partner_change FOR EACH ROW EXECUTE FUNCTION platform.touch();

SELECT platform.create_event_tables('partner');
