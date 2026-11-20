-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000416's state by adding the checks back NOT VALID.
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused')) NOT VALID,
  DROP CONSTRAINT einvoice_read_lengths,
  ADD CONSTRAINT einvoice_read_lengths CHECK (length(validated_against) BETWEEN 1 AND 100 AND length(problem_code) BETWEEN 1 AND 100
    AND length(problem_detail) BETWEEN 1 AND 2000 AND length(document_number) BETWEEN 1 AND 200 AND length(specification) BETWEEN 1 AND 200
    AND document_kind IN ('invoice', 'credit_note')) NOT VALID,
  DROP CONSTRAINT einvoice_processed,
  ADD CONSTRAINT einvoice_processed CHECK ((processed_at IS NULL) = (validated_against IS NULL)
    AND (processed_at IS NULL) = (processed_fetch_operation_id IS NULL) AND (processed_at IS NULL OR processed_at >= received_at) AND (status = 'received' OR processed_at IS NOT NULL)) NOT VALID,
  DROP CONSTRAINT einvoice_problem,
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL)
    AND (problem_code IS NOT NULL) = (status = 'refused')) NOT VALID,
  DROP CONSTRAINT einvoice_read,
  ADD CONSTRAINT einvoice_read CHECK ((currency IS NULL) = (gross_amount IS NULL)
    AND (processed_at IS NOT NULL OR (document_number IS NULL AND document_kind IS NULL AND issue_date IS NULL AND currency IS NULL
      AND specification IS NULL))) NOT VALID;
