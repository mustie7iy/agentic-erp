-- migrate:no-transaction
-- Finding the live run that pays a request (payments.Reservations.RequestRunOf,
-- T46c), built concurrently (06 §6.4).
CREATE INDEX CONCURRENTLY payment_by_request ON payments.payment (tenant_id, down_payment_request_id) WHERE kind = 'down_payment';
