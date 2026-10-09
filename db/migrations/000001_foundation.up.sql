-- Foundation: roles, schemas, shared functions, reference data, platform tables.
-- Conventions: docs/design/03-data-model.md §3.1.

CREATE EXTENSION IF NOT EXISTS btree_gist;   -- EXCLUDE constraints on effective dates

-- Roles. erp_owner owns all objects and runs migrations; erp_app is the
-- application (subject to row-level security, no DDL); erp_readonly is for
-- reporting replicas.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_owner') THEN CREATE ROLE erp_owner NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_app') THEN CREATE ROLE erp_app NOLOGIN NOBYPASSRLS; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_readonly') THEN CREATE ROLE erp_readonly NOLOGIN NOBYPASSRLS; END IF;
END $$;

CREATE SCHEMA ref;
CREATE SCHEMA platform;
CREATE SCHEMA tenancy;
CREATE SCHEMA identity;
CREATE SCHEMA config;
CREATE SCHEMA partner;
CREATE SCHEMA ledger;
CREATE SCHEMA procurement;
CREATE SCHEMA payables;
CREATE SCHEMA payments;
CREATE SCHEMA workflow;
CREATE SCHEMA audit;
CREATE SCHEMA integration;
CREATE SCHEMA agents;
CREATE SCHEMA analytics;

-- ---------------------------------------------------------------------------
-- Shared functions
-- ---------------------------------------------------------------------------

-- The tenant of the current transaction, set by the application with
-- SET LOCAL app.tenant_id = '...'. NULL when unset, which matches no rows.
CREATE FUNCTION platform.current_tenant() RETURNS uuid
LANGUAGE sql STABLE AS $$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;

-- Mutable tables: maintain updated_at and enforce optimistic locking
-- (the application must increment version by exactly one; ADR-0008).
CREATE FUNCTION platform.touch() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF NEW.version IS DISTINCT FROM OLD.version + 1 THEN
      RAISE EXCEPTION 'optimistic lock: %.% version must go from % to %', TG_TABLE_SCHEMA, TG_TABLE_NAME, OLD.version, OLD.version + 1
        USING ERRCODE = 'serialization_failure';
    END IF;
    NEW.updated_at := now();
  END IF;
  RETURN NEW;
END $$;

-- Insert-only tables: refuse UPDATE and DELETE for every role (defense in
-- depth on top of grants; ADR-0006, ADR-0011).
CREATE FUNCTION platform.forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%.% is insert-only; % is not allowed', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'insufficient_privilege';
END $$;

-- Normalizes external references for duplicate detection (spaces, dashes,
-- case and punctuation ignored).
CREATE FUNCTION platform.normalize_ref(t text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$ SELECT upper(regexp_replace(coalesce(t, ''), '[^A-Za-z0-9]', '', 'g')) $$;

-- ---------------------------------------------------------------------------
-- Reference data (global, read-only to the application)
-- ---------------------------------------------------------------------------

CREATE TABLE ref.currency (
  code      char(3) PRIMARY KEY CHECK (code ~ '^[A-Z]{3}$'),
  name      text NOT NULL,
  exponent  smallint NOT NULL CHECK (exponent BETWEEN 0 AND 4)
);

CREATE TABLE ref.country (
  code         char(2) PRIMARY KEY CHECK (code ~ '^[A-Z]{2}$'),
  name         text NOT NULL,
  iban_length  smallint CHECK (iban_length BETWEEN 15 AND 34)
);

CREATE TABLE ref.unit_of_measure (
  code       text PRIMARY KEY CHECK (code ~ '^[A-Z0-9]{1,6}$'),
  name       text NOT NULL,
  dimension  text NOT NULL CHECK (dimension IN ('count', 'mass', 'length', 'area', 'volume', 'time', 'energy'))
);

CREATE TABLE ref.permission (
  code         text PRIMARY KEY CHECK (code ~ '^[a-z_]+(\.[a-z_]+)+$'),
  description  text NOT NULL,
  is_approval  boolean NOT NULL DEFAULT false   -- agents may never hold these
);

INSERT INTO ref.currency (code, name, exponent) VALUES
  ('USD', 'US dollar', 2), ('EUR', 'Euro', 2), ('GBP', 'Pound sterling', 2), ('CHF', 'Swiss franc', 2),
  ('JPY', 'Yen', 0), ('CNY', 'Yuan renminbi', 2), ('INR', 'Indian rupee', 2), ('CAD', 'Canadian dollar', 2),
  ('AUD', 'Australian dollar', 2), ('SGD', 'Singapore dollar', 2), ('AED', 'UAE dirham', 2), ('SAR', 'Saudi riyal', 2),
  ('KWD', 'Kuwaiti dinar', 3), ('BHD', 'Bahraini dinar', 3), ('OMR', 'Rial Omani', 3), ('SEK', 'Swedish krona', 2),
  ('NOK', 'Norwegian krone', 2), ('DKK', 'Danish krone', 2), ('PLN', 'Zloty', 2), ('BRL', 'Brazilian real', 2),
  ('MXN', 'Mexican peso', 2), ('ZAR', 'Rand', 2), ('KRW', 'Won', 0), ('HKD', 'Hong Kong dollar', 2);

INSERT INTO ref.country (code, name, iban_length) VALUES
  ('US', 'United States', NULL), ('DE', 'Germany', 22), ('GB', 'United Kingdom', 22), ('FR', 'France', 27),
  ('NL', 'Netherlands', 18), ('CH', 'Switzerland', 21), ('IE', 'Ireland', 22), ('ES', 'Spain', 24), ('IT', 'Italy', 27),
  ('JP', 'Japan', NULL), ('CN', 'China', NULL), ('IN', 'India', NULL), ('CA', 'Canada', NULL), ('AU', 'Australia', NULL),
  ('SG', 'Singapore', NULL), ('AE', 'United Arab Emirates', 23), ('SA', 'Saudi Arabia', 24), ('KW', 'Kuwait', 30),
  ('SE', 'Sweden', 24), ('NO', 'Norway', 15), ('DK', 'Denmark', 18), ('PL', 'Poland', 28), ('BR', 'Brazil', 29),
  ('MX', 'Mexico', NULL), ('ZA', 'South Africa', NULL);

INSERT INTO ref.unit_of_measure (code, name, dimension) VALUES
  ('EA', 'Each', 'count'), ('PC', 'Piece', 'count'), ('BOX', 'Box', 'count'), ('KG', 'Kilogram', 'mass'),
  ('G', 'Gram', 'mass'), ('T', 'Tonne', 'mass'), ('M', 'Metre', 'length'), ('M2', 'Square metre', 'area'),
  ('L', 'Litre', 'volume'), ('M3', 'Cubic metre', 'volume'), ('H', 'Hour', 'time'), ('DAY', 'Day', 'time'),
  ('KWH', 'Kilowatt hour', 'energy');

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.entry.post', 'Post manual journal entries', false),
  ('ledger.period.manage', 'Open and close posting periods', false),
  ('payables.invoice.capture', 'Capture supplier invoices', false),
  ('payables.invoice.post', 'Post supplier invoices', false),
  ('payables.invoice.review', 'Decide invoice exceptions', false),
  ('payables.invoice.hold', 'Put invoices on hold', false),
  ('payables.invoice.approve', 'Approve supplier invoice exceptions', true),
  ('procurement.requisition.create', 'Create purchase requisitions', false),
  ('procurement.requisition.approve', 'Approve purchase requisitions', true),
  ('procurement.order.create', 'Create purchase orders', false),
  ('procurement.contract.manage', 'Manage contracts', false),
  ('procurement.receipt.post', 'Post goods receipts', false),
  ('partner.supplier.create', 'Request new suppliers', false),
  ('partner.supplier.change', 'Request supplier changes and blocks', false),
  ('partner.bank.change', 'Request bank detail changes', false),
  ('partner.change.approve', 'Approve business partner changes', true),
  ('payments.run.propose', 'Propose payment runs', false),
  ('payments.run.release', 'Release payment runs', true),
  ('config.change.propose', 'Propose configuration changes', false),
  ('config.change.approve', 'Approve configuration changes', true),
  ('config.change.activate', 'Activate configuration changes', true),
  ('integration.migration.run', 'Run migrations and rehearsals', false),
  ('analytics.query.run', 'Run analytics queries', false),
  ('identity.access.manage', 'Manage roles and access', true),
  -- Further permissions declared by api/openapi.yaml (x-permission), checked by api/spec_test.go.
  ('tenancy.org.read', 'View legal entities and org units', false),
  ('tenancy.org.manage', 'Create, change, activate and block legal entities and org units', false),
  ('identity.access.read', 'View principals, roles, role assignments and SoD rules', false),
  ('config.change.read', 'View configuration change requests', false),
  ('config.version.read', 'View configuration versions and number ranges', false),
  ('partner.supplier.read', 'View business partners, bank accounts (masked) and partner changes', false),
  ('ledger.account.read', 'View general ledger accounts', false),
  ('ledger.period.read', 'View fiscal periods', false),
  ('ledger.entry.read', 'View journal entries, open items, clearings and balances', false),
  ('ledger.clearing.post', 'Clear open items', false),
  ('ledger.rate.read', 'View exchange rates', false),
  ('ledger.rate.maintain', 'Record exchange rates', false),
  ('procurement.requisition.read', 'View purchase requisitions', false),
  ('procurement.order.read', 'View purchase orders', false),
  ('procurement.order.release', 'Release purchase orders to suppliers', false),
  ('procurement.contract.read', 'View contracts', false),
  ('procurement.receipt.read', 'View goods receipts', false),
  ('payables.invoice.read', 'View supplier invoices and match results', false),
  ('payments.run.read', 'View payment runs, payments and bank files', false),
  ('payments.file.transmit', 'Transmit bank files to banks', false),
  ('workflow.request.read', 'View approval requests', false),
  ('workflow.request.decide', 'Approve or reject approval requests', true),
  ('workflow.request.cancel', 'Withdraw own approval requests', false),
  ('audit.event.read', 'View the audit trail', false),
  ('audit.chain.verify', 'Verify the audit hash chain', false),
  ('integration.connector.read', 'View connectors', false),
  ('integration.connector.manage', 'Create and change connectors', false),
  ('integration.migration.read', 'View migration runs', false),
  ('platform.operation.read', 'View long-running operations', false),
  ('platform.operation.cancel', 'Cancel long-running operations', false);

-- ---------------------------------------------------------------------------
-- Platform tables (per cell)
-- ---------------------------------------------------------------------------

-- Stored responses for idempotent requests (ADR-0008). Expired rows are
-- removed by a cleanup job.
CREATE TABLE platform.idempotency_key (
  tenant_id      uuid NOT NULL,
  key            text NOT NULL CHECK (length(key) BETWEEN 8 AND 255),
  principal_id   uuid NOT NULL,
  request_hash   bytea NOT NULL,
  status         text NOT NULL CHECK (status IN ('in_progress', 'completed')),
  response_code  smallint,
  response_body  jsonb,
  created_at     timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);
CREATE INDEX idempotency_key_expiry ON platform.idempotency_key (expires_at);

-- Long-running operations (ADR-0012).
CREATE TABLE platform.operation (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  kind          text NOT NULL,
  status        text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
  progress_done   bigint NOT NULL DEFAULT 0 CHECK (progress_done >= 0),
  progress_total  bigint CHECK (progress_total IS NULL OR progress_done <= progress_total),   -- null while unknown
  resource      text,                                -- API path of the resource the operation creates or changes
  request       jsonb NOT NULL DEFAULT '{}',
  result        jsonb,
  error         jsonb,
  workflow_ref  text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX operation_by_status ON platform.operation (tenant_id, status, created_at);
CREATE TRIGGER operation_touch BEFORE UPDATE ON platform.operation FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Creates the outbox (domain events awaiting publication) and inbox
-- (events already processed by this context's consumers) in a context schema
-- (ADR-0007). Called by each context's migration.
CREATE FUNCTION platform.create_event_tables(ctx text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE format($f$
    CREATE TABLE %1$I.outbox (
      tenant_id       uuid NOT NULL,
      id              uuid NOT NULL,
      aggregate_type  text NOT NULL,
      aggregate_id    uuid NOT NULL,
      -- CloudEvents type, for example com.example.erp.payables.supplier_invoice_posted.v1 (04 §4.12).
      event_type      text NOT NULL CHECK (event_type ~ '^com\.example\.erp\.%2$s\.[a-z_]+\.v[0-9]+$'),
      payload         jsonb NOT NULL,
      actor_id        uuid NOT NULL,
      trace_id        text,
      occurred_at     timestamptz NOT NULL DEFAULT now(),
      published_at    timestamptz,
      PRIMARY KEY (tenant_id, id)
    );
    CREATE INDEX outbox_unpublished ON %1$I.outbox (occurred_at) WHERE published_at IS NULL;
    CREATE TABLE %1$I.inbox (
      tenant_id     uuid NOT NULL,
      consumer      text NOT NULL,
      event_id      uuid NOT NULL,
      processed_at  timestamptz NOT NULL DEFAULT now(),
      PRIMARY KEY (tenant_id, consumer, event_id)
    );
  $f$, ctx, CASE ctx WHEN 'config' THEN 'configuration' ELSE ctx END);
END $$;

-- Effective-dated assignments (roles, company access, bank accounts, ...) are
-- never deleted or rewritten. The only change allowed is ending them: setting
-- valid_to (or bringing it forward), never into the past, recording who ended
-- it. The application role gets UPDATE on exactly these columns (000011).
CREATE FUNCTION platform.end_validity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.valid_to IS NULL OR (OLD.valid_to IS NOT NULL AND NEW.valid_to >= OLD.valid_to) THEN
    RAISE EXCEPTION '%: validity can only be ended or shortened', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.valid_to < current_date THEN
    RAISE EXCEPTION '%: validity cannot be ended in the past', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.ended_by IS NULL THEN
    RAISE EXCEPTION '%: ended_by is required', TG_TABLE_NAME USING ERRCODE = 'not_null_violation';
  END IF;
  NEW.ended_at := now();
  RETURN NEW;
END $$;

-- Long-running operations publish their completion (ADR-0012).
SELECT platform.create_event_tables('platform');
