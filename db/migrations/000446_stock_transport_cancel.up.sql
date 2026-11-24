-- Cancelling a stock transport order before its goods issue (S2h;
-- ADR-0082 As built S2h, cancellation; review:batch92; 03-data-model
-- §3.35). An order entered by mistake was open for ever, so a planning
-- run counted it as the supplying plant's demand and the receiving
-- plant's receipt for good (ADR-0084 As built, stock transport orders).
-- An open order may now be cancelled: it moves from open to cancelled,
-- names no material document, and changes no more. Expand only (06 §6.4):
-- the status and issue checks widened NOT VALID (validated by 000447), a
-- trigger that refuses everything it refused but the one new step, which
-- the previous version never takes, and a permission's description.

ALTER TABLE procurement.stock_transport_order
  DROP CONSTRAINT stock_transport_order_status_check,
  ADD CONSTRAINT stock_transport_order_status_check CHECK (status IN ('open', 'in_transit', 'received', 'cancelled')) NOT VALID,
  -- Issued once it is in transit or received; an open or cancelled order
  -- names no goods issue.
  DROP CONSTRAINT stock_transport_order_issued,
  ADD CONSTRAINT stock_transport_order_issued CHECK ((status IN ('in_transit', 'received')) = (issue_document_id IS NOT NULL)
    AND (issue_document_id IS NULL) = (issue_document_number IS NULL) AND (issue_document_id IS NULL) = (issue_date IS NULL)) NOT VALID;

-- What an order is and the steps it took stay as they are; its status
-- moves forward one step at a time: open to in transit to received, or
-- open to cancelled.
CREATE OR REPLACE FUNCTION procurement.stock_transport_order_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.fiscal_year, NEW.number, NEW.supplying_plant_id, NEW.receiving_plant_id, NEW.order_date, NEW.created_at, NEW.created_by)
       IS DISTINCT FROM (OLD.legal_entity_id, OLD.fiscal_year, OLD.number, OLD.supplying_plant_id, OLD.receiving_plant_id, OLD.order_date, OLD.created_at,
         OLD.created_by)
     OR (OLD.status, NEW.status) NOT IN (('open', 'in_transit'), ('in_transit', 'received'), ('open', 'cancelled'))
     OR (OLD.issue_document_id IS NOT NULL AND (NEW.issue_document_id, NEW.issue_document_number, NEW.issue_date)
       IS DISTINCT FROM (OLD.issue_document_id, OLD.issue_document_number, OLD.issue_date)) THEN
    RAISE EXCEPTION 'stock_transport_order_fixed: order %: an order stays as created and moves from open to in transit to received, or from open to cancelled',
      OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'stock_transport_order_fixed';
  END IF;
  RETURN NEW;
END $$;

UPDATE ref.permission SET description = 'Create and cancel stock transport orders between plants' WHERE code = 'procurement.stock_transport.write';
