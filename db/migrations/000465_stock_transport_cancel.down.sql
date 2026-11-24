-- Cancelled stock transport orders the previous version cannot hold are
-- refused rather than changed (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.stock_transport_order WHERE status = 'cancelled') THEN
    RAISE EXCEPTION 'cancelled stock transport orders exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
UPDATE ref.permission SET description = 'Create stock transport orders between plants' WHERE code = 'procurement.stock_transport.write';
-- stock_transport_order_fixed as 000418 left it.
CREATE OR REPLACE FUNCTION procurement.stock_transport_order_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.fiscal_year, NEW.number, NEW.supplying_plant_id, NEW.receiving_plant_id, NEW.order_date, NEW.created_at, NEW.created_by)
       IS DISTINCT FROM (OLD.legal_entity_id, OLD.fiscal_year, OLD.number, OLD.supplying_plant_id, OLD.receiving_plant_id, OLD.order_date, OLD.created_at,
         OLD.created_by)
     OR (OLD.status, NEW.status) NOT IN (('open', 'in_transit'), ('in_transit', 'received'))
     OR (OLD.issue_document_id IS NOT NULL AND (NEW.issue_document_id, NEW.issue_document_number, NEW.issue_date)
       IS DISTINCT FROM (OLD.issue_document_id, OLD.issue_document_number, OLD.issue_date)) THEN
    RAISE EXCEPTION 'stock_transport_order_fixed: order %: an order stays as created and moves from open to in transit to received', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stock_transport_order_fixed';
  END IF;
  RETURN NEW;
END $$;
-- The checks go back to 000418's state, validated.
ALTER TABLE procurement.stock_transport_order
  DROP CONSTRAINT stock_transport_order_status_check,
  ADD CONSTRAINT stock_transport_order_status_check CHECK (status IN ('open', 'in_transit', 'received')),
  DROP CONSTRAINT stock_transport_order_issued,
  ADD CONSTRAINT stock_transport_order_issued CHECK ((status <> 'open') = (issue_document_id IS NOT NULL)
    AND (issue_document_id IS NULL) = (issue_document_number IS NULL) AND (issue_document_id IS NULL) = (issue_date IS NULL));
