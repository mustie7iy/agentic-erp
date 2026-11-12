-- Supplier quotations (T82b; ADR-0069 §7, plan-breadth-week2 §T82b): what
-- an invited supplier quoted for a published RFQ, as the buyer recorded
-- it. A quotation belongs to an RFQ and to one of the suppliers it invites
-- (the foreign key to rfq_supplier), one per supplier, with the supplier's
-- reference and the last day it is valid; its items each price one item
-- of the RFQ, in the RFQ's currency, per price_unit of the RFQ item's
-- unit. A quotation may leave RFQ items out. Its total is not stored: it
-- is the sum of its items' quantity × unit price ÷ price unit, read with
-- the RFQ's quantities, which never change. Expand only: new tables, which
-- the previous version neither reads nor writes (06 §6.4).
CREATE TABLE procurement.supplier_quotation (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  rfq_id              uuid NOT NULL,
  supplier_id         uuid NOT NULL,       -- partner context, through rfq_supplier
  supplier_reference  text NOT NULL CONSTRAINT supplier_quotation_reference_check
                        CHECK (btrim(supplier_reference) <> '' AND length(supplier_reference) <= 40),
  valid_until         date NOT NULL,
  status              text NOT NULL DEFAULT 'recorded' CONSTRAINT supplier_quotation_status_check CHECK (status IN ('recorded')),
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- A supplier has one quotation per RFQ.
  CONSTRAINT supplier_quotation_once UNIQUE (tenant_id, rfq_id, supplier_id),
  -- What an item's key names: its quotation within its RFQ.
  CONSTRAINT supplier_quotation_in_rfq UNIQUE (tenant_id, rfq_id, id),
  -- Only a supplier the RFQ invites quotes for it.
  CONSTRAINT supplier_quotation_invited FOREIGN KEY (tenant_id, rfq_id, supplier_id)
    REFERENCES procurement.rfq_supplier (tenant_id, rfq_id, supplier_id)
);
CREATE TRIGGER supplier_quotation_touch BEFORE UPDATE ON procurement.supplier_quotation FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: an RFQ's quotations in the order they were recorded.
CREATE INDEX supplier_quotation_by_rfq ON procurement.supplier_quotation (tenant_id, rfq_id, created_at, id);
SELECT platform.secure_table('procurement.supplier_quotation'::regclass);
-- Recording is the only write: nothing changes a quotation yet, and
-- nothing is deleted (T82d grants what an award sets).
REVOKE UPDATE ON procurement.supplier_quotation FROM erp_app;

-- Only a published RFQ takes quotations: the RFQ is read FOR SHARE, so a
-- quotation is not recorded beside a change of the RFQ in progress.
CREATE FUNCTION procurement.check_rfq_is_published() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  current text;
BEGIN
  SELECT status INTO current FROM procurement.rfq WHERE tenant_id = NEW.tenant_id AND id = NEW.rfq_id FOR SHARE;
  IF current IS DISTINCT FROM 'published' THEN
    RAISE EXCEPTION 'RFQ % is not published: it takes no quotation', NEW.rfq_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'supplier_quotation_of_published_rfq';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_rfq_is_published() OWNER TO erp_owner;
CREATE TRIGGER supplier_quotation_of_published BEFORE INSERT ON procurement.supplier_quotation
  FOR EACH ROW EXECUTE FUNCTION procurement.check_rfq_is_published();

-- A quoted price: one item of the quotation's RFQ (the foreign key to
-- rfq_item), once per quotation, at a positive price in minor units of
-- the RFQ's currency per price_unit of the RFQ item's unit.
CREATE TABLE procurement.supplier_quotation_item (
  tenant_id               uuid NOT NULL,
  supplier_quotation_id   uuid NOT NULL,
  rfq_id                  uuid NOT NULL,
  rfq_item_no             integer NOT NULL,
  unit_price              bigint NOT NULL CONSTRAINT supplier_quotation_item_unit_price_check CHECK (unit_price > 0),
  price_unit              integer NOT NULL DEFAULT 1 CONSTRAINT supplier_quotation_item_price_unit_check CHECK (price_unit > 0),
  PRIMARY KEY (tenant_id, supplier_quotation_id, rfq_item_no),
  CONSTRAINT supplier_quotation_item_quotation_fkey FOREIGN KEY (tenant_id, rfq_id, supplier_quotation_id)
    REFERENCES procurement.supplier_quotation (tenant_id, rfq_id, id),
  CONSTRAINT supplier_quotation_item_rfq_item_fkey FOREIGN KEY (tenant_id, rfq_id, rfq_item_no)
    REFERENCES procurement.rfq_item (tenant_id, rfq_id, item_no)
);
-- What was quoted is fixed.
CREATE TRIGGER supplier_quotation_item_immutable BEFORE UPDATE OR DELETE ON procurement.supplier_quotation_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('procurement.supplier_quotation_item'::regclass);
