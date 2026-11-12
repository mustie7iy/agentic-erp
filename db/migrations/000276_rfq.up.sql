-- Requests for quotation (T82a; ADR-0069 §1-§3, plan-breadth-week2 §T82a):
-- a buyer asks the suppliers invited for prices of items the company
-- wants to buy. An RFQ belongs to a company, is numbered from its `rfq`
-- range, names the currency quotations are given in and the date they are
-- due, and is a draft until it is published, once; nothing else changes
-- it. Its items name what is bought as a contract's do (description and
-- material_key, each key once: an award makes them a contract's items,
-- T82d) with the quantity asked for, and carry no price. legal_entity_id
-- is the tenancy context's, supplier_id the partner context's and
-- account_id the ledger's: no foreign key leaves the schema (02 §2.1).
-- Expand only: new tables and permissions, which the previous version
-- neither reads nor writes (06 §6.4).
CREATE TABLE procurement.rfq (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  document_number  text NOT NULL,
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  currency         char(3) NOT NULL REFERENCES ref.currency,
  due_date         date NOT NULL,
  status           text NOT NULL DEFAULT 'draft' CONSTRAINT rfq_status_check CHECK (status IN ('draft', 'published')),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT rfq_number_once UNIQUE (tenant_id, legal_entity_id, document_number)
);
CREATE TRIGGER rfq_touch BEFORE UPDATE ON procurement.rfq FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: the tenant's RFQs by due date, in the order its pages are read.
CREATE INDEX rfq_by_due_date ON procurement.rfq (tenant_id, due_date, id);
SELECT platform.secure_table('procurement.rfq'::regclass);

-- An RFQ starts as a draft, keeps its company, number, currency and due
-- date, and stays published: the suppliers invited were told what is
-- asked and by when.
CREATE FUNCTION procurement.check_rfq() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'draft' THEN
      RAISE EXCEPTION 'RFQ % starts as a draft', NEW.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_starts_draft';
    END IF;
    RETURN NEW;
  END IF;
  IF (NEW.legal_entity_id, NEW.document_number, NEW.currency, NEW.due_date)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.document_number, OLD.currency, OLD.due_date) THEN
    RAISE EXCEPTION 'RFQ % keeps its company, number, currency and due date', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_identity_kept';
  END IF;
  IF OLD.status = 'published' AND NEW.status <> 'published' THEN
    RAISE EXCEPTION 'RFQ % stays published', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_published';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_rfq() OWNER TO erp_owner;
CREATE TRIGGER rfq_lifecycle BEFORE INSERT OR UPDATE ON procurement.rfq FOR EACH ROW EXECUTE FUNCTION procurement.check_rfq();

-- What an RFQ asks for and whom it asks is written with it, while it is a
-- draft, and is fixed: the RFQ is read FOR SHARE, so a row is not added
-- beside a publication in progress.
CREATE FUNCTION procurement.check_rfq_is_draft() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  current text;
BEGIN
  SELECT status INTO current FROM procurement.rfq WHERE tenant_id = NEW.tenant_id AND id = NEW.rfq_id FOR SHARE;
  IF current IS DISTINCT FROM 'draft' THEN
    RAISE EXCEPTION 'RFQ % is not a draft: its items and suppliers are fixed', NEW.rfq_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_fixed_once_published';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_rfq_is_draft() OWNER TO erp_owner;

-- An item: what is asked for, without a price. quantity is as a
-- requisition's (numeric(20,6), more than zero).
CREATE TABLE procurement.rfq_item (
  tenant_id     uuid NOT NULL,
  rfq_id        uuid NOT NULL,
  item_no       integer NOT NULL CONSTRAINT rfq_item_item_no_check CHECK (item_no > 0),
  material_key  text NOT NULL CONSTRAINT rfq_item_material_key_check CHECK (length(material_key) BETWEEN 1 AND 40),
  description   text NOT NULL CONSTRAINT rfq_item_description_check CHECK (btrim(description) <> '' AND length(description) <= 200),
  quantity      numeric(20,6) NOT NULL CONSTRAINT rfq_item_quantity_check CHECK (quantity > 0),
  uom_code      text NOT NULL REFERENCES ref.unit_of_measure,
  account_id    uuid NOT NULL,          -- ledger context
  PRIMARY KEY (tenant_id, rfq_id, item_no),
  CONSTRAINT rfq_item_material_key UNIQUE (tenant_id, rfq_id, material_key),
  FOREIGN KEY (tenant_id, rfq_id) REFERENCES procurement.rfq (tenant_id, id)
);
CREATE TRIGGER rfq_item_immutable BEFORE UPDATE OR DELETE ON procurement.rfq_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER rfq_item_of_draft BEFORE INSERT ON procurement.rfq_item FOR EACH ROW EXECUTE FUNCTION procurement.check_rfq_is_draft();
SELECT platform.secure_table('procurement.rfq_item'::regclass);

-- A supplier invited, once per RFQ; invite_no is its place in the
-- invitation, the order the RFQ and its events list the suppliers in.
CREATE TABLE procurement.rfq_supplier (
  tenant_id    uuid NOT NULL,
  rfq_id       uuid NOT NULL,
  supplier_id  uuid NOT NULL,           -- partner context
  invite_no    integer NOT NULL CONSTRAINT rfq_supplier_invite_no_check CHECK (invite_no > 0),
  CONSTRAINT rfq_supplier_once PRIMARY KEY (tenant_id, rfq_id, supplier_id),
  CONSTRAINT rfq_supplier_invite_no UNIQUE (tenant_id, rfq_id, invite_no),
  FOREIGN KEY (tenant_id, rfq_id) REFERENCES procurement.rfq (tenant_id, id)
);
CREATE TRIGGER rfq_supplier_immutable BEFORE UPDATE OR DELETE ON procurement.rfq_supplier
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER rfq_supplier_of_draft BEFORE INSERT ON procurement.rfq_supplier FOR EACH ROW EXECUTE FUNCTION procurement.check_rfq_is_draft();
-- The list's filter: the RFQs a supplier is invited to.
CREATE INDEX rfq_supplier_by_supplier ON procurement.rfq_supplier (tenant_id, supplier_id, rfq_id);
SELECT platform.secure_table('procurement.rfq_supplier'::regclass);

-- Reading RFQs, and creating and publishing them (ADR-0069 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('procurement.rfq.read', 'View requests for quotation of the companies one may act in', false),
  ('procurement.rfq.manage', 'Create and publish requests for quotation', false);
