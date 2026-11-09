-- 000150's check, without the subdivisions a component lists.
CREATE OR REPLACE FUNCTION ledger.check_line_jurisdiction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE v_country text;
BEGIN
  IF EXISTS (SELECT FROM ledger.journal_entry j WHERE j.tenant_id = NEW.tenant_id AND j.fiscal_year = NEW.fiscal_year
      AND j.id = NEW.journal_entry_id AND j.reversal_of_id IS NOT NULL) THEN
    RETURN NEW;
  END IF;
  SELECT max(c.jurisdiction) FILTER (WHERE c.jurisdiction_on_line) INTO v_country FROM ledger.tax_component c
  WHERE c.tenant_id = NEW.tenant_id AND c.legal_entity_id = NEW.legal_entity_id AND c.code = NEW.tax_code;
  IF (v_country IS NULL) <> (NEW.tax_jurisdiction IS NULL) OR NOT coalesce(NEW.tax_jurisdiction LIKE v_country || '-%', true) THEN
    RAISE EXCEPTION 'line % of entry % names jurisdiction %, which its code % does not levy', NEW.line_no, NEW.journal_entry_id,
      coalesce(NEW.tax_jurisdiction, 'none'), NEW.tax_code
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_tax_jurisdiction_code';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE ledger.tax_component DROP COLUMN subdivisions;
