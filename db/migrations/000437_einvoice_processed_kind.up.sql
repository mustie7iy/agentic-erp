-- review:batch87 (ADR-0061 As built T65d, T65e): the fetch's entering
-- branches on what a processed document is (a tax invoice or a tax credit
-- note), so a processed document that was not refused states its kind;
-- einvoice_read keeps every read field empty before processing and nothing
-- required the kind after. Added NOT VALID and validated by 000438 (06 §6.4).
ALTER TABLE einvoicing.einvoice
  ADD CONSTRAINT einvoice_read_processed CHECK (processed_at IS NULL OR status = 'refused' OR document_kind IS NOT NULL) NOT VALID;
