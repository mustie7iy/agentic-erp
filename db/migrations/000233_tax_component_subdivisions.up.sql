-- The emirate guard (ADR-0049 §7 As built, review:T61k): a component that
-- levies its lines' subdivision may list the subdivisions it accepts (the
-- UAE pack's seven emirates), and a line naming another is refused, so a
-- mistyped emirate cannot leave a sale's output VAT off the VAT 201's
-- boxes 1a-1g. Null accepts any subdivision of the component's country,
-- as every existing row does, so the check holds on them; the list is
-- only on a component levying its lines' subdivision, each element a
-- subdivision of its country (jurisdiction is then two letters, 000150,
-- so it is safe inside the pattern).
ALTER TABLE ledger.tax_component ADD COLUMN subdivisions text[];
ALTER TABLE ledger.tax_component ADD CONSTRAINT tax_component_subdivisions
  CHECK (subdivisions IS NULL OR (jurisdiction_on_line AND cardinality(subdivisions) > 0
    AND array_to_string(subdivisions, ',', '?') ~ ('^' || jurisdiction || '-[A-Z0-9]{1,3}(,' || jurisdiction || '-[A-Z0-9]{1,3})*$')));
GRANT UPDATE (subdivisions) ON ledger.tax_component TO erp_app;

-- 000150's check, and then one of the subdivisions the component lists,
-- where it lists them. A reversal still repeats the original's.
CREATE OR REPLACE FUNCTION ledger.check_line_jurisdiction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE v_country text; v_subdivisions text[];
BEGIN
  IF EXISTS (SELECT FROM ledger.journal_entry j WHERE j.tenant_id = NEW.tenant_id AND j.fiscal_year = NEW.fiscal_year
      AND j.id = NEW.journal_entry_id AND j.reversal_of_id IS NOT NULL) THEN
    RETURN NEW;
  END IF;
  -- The first component levying the line's subdivision, as the ledger
  -- reads it (in component order).
  SELECT c.jurisdiction, c.subdivisions INTO v_country, v_subdivisions FROM ledger.tax_component c
  WHERE c.tenant_id = NEW.tenant_id AND c.legal_entity_id = NEW.legal_entity_id AND c.code = NEW.tax_code AND c.jurisdiction_on_line
  ORDER BY c.component LIMIT 1;
  IF (v_country IS NULL) <> (NEW.tax_jurisdiction IS NULL) OR NOT coalesce(NEW.tax_jurisdiction LIKE v_country || '-%', true)
      OR NOT coalesce(NEW.tax_jurisdiction = ANY(v_subdivisions), true) THEN
    RAISE EXCEPTION 'line % of entry % names jurisdiction %, which its code % does not levy', NEW.line_no, NEW.journal_entry_id,
      coalesce(NEW.tax_jurisdiction, 'none'), NEW.tax_code
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_tax_jurisdiction_code';
  END IF;
  RETURN NEW;
END $$;
