-- Outgoing e-invoices (E2; ADR-0061 §8, §9, As built E2; 03-data-model
-- §3.25): a posted customer invoice or customer credit memo of a UAE
-- company rendered as a PINT AE e-invoice, once per customer document.
-- A render that passes every mandatory-field rule is valid, its XML kept
-- write-once in object storage under its SHA-256 (ADR-0016), final; one
-- that fails is invalid, its problems listed and no XML kept, and is
-- rendered again once the master data is corrected. customer_invoice_id
-- and customer_id are the receivables and partner contexts', the
-- source document the sales context's, legal_entity_id the tenancy
-- context's: no foreign key leaves the schema (02 §2.1). Expand only: a
-- new table and two permissions, which the previous version neither
-- reads nor writes.
CREATE TABLE einvoicing.outgoing_einvoice (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  legal_entity_id          uuid NOT NULL,        -- tenancy context: the seller
  customer_invoice_id      uuid NOT NULL,        -- receivables context
  customer_invoice_number  text NOT NULL CHECK (length(customer_invoice_number) BETWEEN 1 AND 40),
  source_document_id       uuid,                 -- sales context: the billing document it posted from
  source_document_number   text CHECK (length(source_document_number) BETWEEN 1 AND 40),
  customer_id              uuid NOT NULL,        -- partner context: the buyer
  kind                     text NOT NULL CHECK (kind IN ('invoice', 'credit_note')),
  type_code                text NOT NULL,
  issue_date               date NOT NULL,
  currency                 char(3) NOT NULL REFERENCES ref.currency,
  net_amount               bigint NOT NULL,
  tax_amount               bigint NOT NULL,
  gross_amount             bigint NOT NULL,
  reference_invoice_number text CHECK (length(reference_invoice_number) BETWEEN 1 AND 40),
  credit_reason_code       text CHECK (credit_reason_code ~ '^[A-Za-z0-9._-]{1,20}$'),
  credit_reason_text       text CHECK (length(credit_reason_text) BETWEEN 1 AND 500),
  specification            text NOT NULL CHECK (length(specification) BETWEEN 1 AND 200),
  status                   text NOT NULL CHECK (status IN ('valid', 'invalid')),
  problems                 jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(problems) = 'array'),
  sha256                   bytea CHECK (length(sha256) = 32),
  size                     bigint CHECK (size > 0),
  content_ref              text CHECK (length(content_ref) BETWEEN 1 AND 300),  -- object key: tenants/<tenant>/documents/<sha256>
  rendered_at              timestamptz NOT NULL DEFAULT now(),
  version                  integer NOT NULL DEFAULT 1,
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  updated_at               timestamptz NOT NULL DEFAULT now(),
  updated_by               uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- One e-invoice per customer document: rendering again upserts on it.
  CONSTRAINT outgoing_einvoice_once UNIQUE (tenant_id, customer_invoice_id),
  -- A tax invoice is 380, a tax credit note 381 (PINT AE IBT-003).
  CONSTRAINT outgoing_einvoice_type CHECK ((kind = 'invoice' AND type_code = '380') OR (kind = 'credit_note' AND type_code = '381')),
  -- A credit note's reason (BTAE-03), code and text together, and an
  -- invoice's none.
  CONSTRAINT outgoing_einvoice_reason CHECK ((credit_reason_code IS NULL) = (credit_reason_text IS NULL)
    AND (kind = 'credit_note' OR credit_reason_code IS NULL)),
  CONSTRAINT outgoing_einvoice_source CHECK ((source_document_id IS NULL) = (source_document_number IS NULL)),
  -- Valid: its XML kept and no problem; invalid: problems and no XML.
  CONSTRAINT outgoing_einvoice_content CHECK (
    (status = 'valid' AND sha256 IS NOT NULL AND size IS NOT NULL AND content_ref IS NOT NULL AND problems = '[]'::jsonb)
    OR (status = 'invalid' AND sha256 IS NULL AND size IS NULL AND content_ref IS NULL AND problems <> '[]'::jsonb))
);
CREATE TRIGGER outgoing_einvoice_touch BEFORE UPDATE ON einvoicing.outgoing_einvoice FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list, newest rendered first.
CREATE INDEX outgoing_einvoice_by_rendered ON einvoicing.outgoing_einvoice (tenant_id, rendered_at, id);
SELECT platform.secure_table('einvoicing.outgoing_einvoice'::regclass);

-- A valid e-invoice is the legal document and never changes; an invalid
-- one is rendered again, to invalid or valid, and keeps what names its
-- customer document.
CREATE FUNCTION einvoicing.check_outgoing_einvoice() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'valid' THEN
    RAISE EXCEPTION 'outgoing e-invoice % is valid and final', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_einvoice_final';
  END IF;
  IF (NEW.tenant_id, NEW.id, NEW.legal_entity_id, NEW.customer_invoice_id, NEW.customer_invoice_number, NEW.customer_id, NEW.kind,
      NEW.created_at, NEW.created_by) IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.legal_entity_id, OLD.customer_invoice_id,
      OLD.customer_invoice_number, OLD.customer_id, OLD.kind, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'outgoing e-invoice % keeps its customer document', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_einvoice_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION einvoicing.check_outgoing_einvoice() OWNER TO erp_owner;
CREATE TRIGGER outgoing_einvoice_transition BEFORE UPDATE ON einvoicing.outgoing_einvoice FOR EACH ROW
  EXECUTE FUNCTION einvoicing.check_outgoing_einvoice();

-- Rendering and reading outgoing e-invoices (ADR-0061 §11, As built E2).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('einvoicing.outgoing.read', 'View the e-invoices rendered from the customer invoices of the companies one may act in', false),
  ('einvoicing.outgoing.render', 'Render posted customer invoices and credit memos as e-invoices', false);
