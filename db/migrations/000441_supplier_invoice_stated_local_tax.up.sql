-- E-invoices not in AED entered with their VAT in AED as stated (ADR-0061
-- §5 C5, As built batch 88; split:T65d, po:2026-11-09, po:2026-11-21): a
-- supplier invoice entered from a received tax invoice or credit note in
-- another currency keeps the VAT it states in the tax accounting currency
-- (the sum of its lines' BTAE-08), which its posting takes as the tax's
-- local amount, rather than its tax converted at the tax date's rate. The
-- amount with its currency, or neither; never negative (the kind gives the
-- direction).
ALTER TABLE payables.supplier_invoice
  ADD COLUMN stated_local_tax bigint,
  ADD COLUMN stated_local_currency char(3) REFERENCES ref.currency,
  ADD CONSTRAINT supplier_invoice_stated_local_tax CHECK ((stated_local_tax IS NULL) = (stated_local_currency IS NULL)
    AND (stated_local_tax IS NULL OR stated_local_tax >= 0)) NOT VALID;
