-- Validates the checks 000465 widened NOT VALID (06 §6.4).
ALTER TABLE procurement.stock_transport_order VALIDATE CONSTRAINT stock_transport_order_status_check;
ALTER TABLE procurement.stock_transport_order VALIDATE CONSTRAINT stock_transport_order_issued;
