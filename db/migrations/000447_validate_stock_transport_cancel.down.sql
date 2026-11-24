-- A validated check cannot be marked unvalidated again: the down restores
-- 000446's state by adding both checks back NOT VALID.
ALTER TABLE procurement.stock_transport_order
  DROP CONSTRAINT stock_transport_order_status_check,
  ADD CONSTRAINT stock_transport_order_status_check CHECK (status IN ('open', 'in_transit', 'received', 'cancelled')) NOT VALID,
  DROP CONSTRAINT stock_transport_order_issued,
  ADD CONSTRAINT stock_transport_order_issued CHECK ((status IN ('in_transit', 'received')) = (issue_document_id IS NOT NULL)
    AND (issue_document_id IS NULL) = (issue_document_number IS NULL) AND (issue_document_id IS NULL) = (issue_date IS NULL)) NOT VALID;
