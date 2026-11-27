-- Impairments, and the movements and entries they wrote, which the
-- previous version cannot hold, are refused rather than dropped (06 §6.4:
-- a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM assets.impairment) OR EXISTS (SELECT FROM assets.movement WHERE document_kind = 'impairment') THEN
    RAISE EXCEPTION 'asset impairments exist; the previous version cannot hold them'
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
END $$;
DROP TABLE assets.impairment;
ALTER TABLE assets.year_total DROP COLUMN impairment_losses, DROP COLUMN impairment_reversals;
ALTER TABLE assets.class_year_total DROP COLUMN impairment_losses, DROP COLUMN impairment_reversals;
ALTER TABLE assets.document_entry
  DROP CONSTRAINT document_entry_document_kind_check,
  ADD CONSTRAINT document_entry_document_kind_check CHECK (document_kind IN ('acquisition', 'retirement', 'transfer'));
ALTER TABLE assets.movement
  DROP CONSTRAINT movement_impairment_document,
  DROP CONSTRAINT movement_impairment,
  DROP CONSTRAINT movement_document_kind_check,
  ADD CONSTRAINT movement_document_kind_check CHECK (document_kind IN ('acquisition', 'retirement', 'transfer', 'depreciation_run')),
  DROP CONSTRAINT movement_kind_check,
  ADD CONSTRAINT movement_kind_check CHECK (kind IN ('acquisition', 'depreciation', 'retirement', 'transfer_out', 'transfer_in'));
