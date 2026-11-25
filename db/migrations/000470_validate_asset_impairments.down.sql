-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000469's state by adding them back NOT VALID.
ALTER TABLE assets.movement
  DROP CONSTRAINT movement_kind_check,
  ADD CONSTRAINT movement_kind_check
    CHECK (kind IN ('acquisition', 'depreciation', 'retirement', 'transfer_out', 'transfer_in', 'impairment', 'impairment_reversal')) NOT VALID,
  DROP CONSTRAINT movement_document_kind_check,
  ADD CONSTRAINT movement_document_kind_check
    CHECK (document_kind IN ('acquisition', 'retirement', 'transfer', 'depreciation_run', 'impairment')) NOT VALID,
  DROP CONSTRAINT movement_impairment,
  ADD CONSTRAINT movement_impairment CHECK (kind NOT IN ('impairment', 'impairment_reversal')
    OR (cost = 0 AND cost_gc = 0 AND residual = 0 AND proceeds = 0 AND share IS NULL AND document_kind = 'impairment'
      AND (reverses_id IS NOT NULL OR (kind = 'impairment') = (accumulated < 0)) AND accumulated <> 0)) NOT VALID,
  DROP CONSTRAINT movement_impairment_document,
  ADD CONSTRAINT movement_impairment_document CHECK (document_kind <> 'impairment' OR kind IN ('impairment', 'impairment_reversal')) NOT VALID;
ALTER TABLE assets.document_entry
  DROP CONSTRAINT document_entry_document_kind_check,
  ADD CONSTRAINT document_entry_document_kind_check CHECK (document_kind IN ('acquisition', 'retirement', 'transfer', 'impairment')) NOT VALID;
