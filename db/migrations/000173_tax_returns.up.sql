-- The return engine (T61h; ADR-0049 §11): a draft return is computed on
-- demand from the tax items no return has reported yet, of the
-- registration's companies, dated on or before the period's end; nothing
-- is stored until a return is prepared (T61i). The index serves that
-- selection, which tax_item_by_code (code first) does not. Expand only:
-- the previous version neither reads nor needs it.
CREATE INDEX tax_item_unreported ON ledger.tax_item (tenant_id, legal_entity_id, tax_date) WHERE reported_in IS NULL;

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('tax.return.read', 'Compute draft tax returns and read the tax items their boxes sum', false);
