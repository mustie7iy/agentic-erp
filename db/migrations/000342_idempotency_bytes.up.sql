-- A stored answer that is not JSON (T95d; ADR-0008, ADR-0076 §8): an
-- analytics query answered as an Excel workbook is a POST with an
-- Idempotency-Key, and its replay answers the same bytes. JSON answers
-- stay in response_body; a stored answer is one or the other. Expand only
-- (06 §6.4): a nullable column the previous version neither reads nor
-- writes, and a check every row it wrote meets.
ALTER TABLE platform.idempotency_key
  ADD COLUMN response_bytes bytea,
  ADD CONSTRAINT idempotency_key_one_body CHECK (response_body IS NULL OR response_bytes IS NULL);
