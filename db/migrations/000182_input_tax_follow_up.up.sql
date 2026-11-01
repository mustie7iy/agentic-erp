-- Input tax follow-up of T61d (ADR-0049 §4, §6). Expand only: columns the
-- previous release neither reads nor writes (its statements name their
-- columns), so this runs ahead of the code.
--
-- A posted invoice's documentary-requirement warnings, recorded when it
-- posts: what its document may lack for its input tax to be recovered, by
-- the company's tiers (payables.DocumentaryWarning). Warnings, never
-- refusals; an invoice posted before has none.
ALTER TABLE payables.supplier_invoice ADD COLUMN documentary_warnings jsonb NOT NULL DEFAULT '[]'
  CONSTRAINT supplier_invoice_documentary_warnings_array CHECK (jsonb_typeof(documentary_warnings) = 'array');

-- A down payment request left out of a run with tax_code_invalid names its
-- tax code and the rates: the rate of its due date, at which its tax was
-- calculated, and the run date's, at which it would be paid; the run
-- date's is absent when the code is not valid on the run date at all.
-- Exclusions recorded before name none.
ALTER TABLE payments.request_exclusion
  ADD COLUMN tax_code text,
  ADD COLUMN request_tax_rate numeric,
  ADD COLUMN run_tax_rate numeric,
  ADD CONSTRAINT request_exclusion_tax_named CHECK (
    (tax_code IS NULL OR reason_code = 'tax_code_invalid')
    AND (tax_code IS NOT NULL OR (request_tax_rate IS NULL AND run_tax_rate IS NULL))
    AND coalesce(request_tax_rate >= 0 AND request_tax_rate < 100, true)
    AND coalesce(run_tax_rate >= 0 AND run_tax_rate < 100, true));
