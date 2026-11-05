-- The trigger as 000043 left it.
CREATE OR REPLACE FUNCTION ledger.check_entry_period() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text; d_start date; d_end date;
BEGIN
  SELECT ps.status, fp.start_date, fp.end_date INTO st, d_start, d_end
  FROM ledger.period_status ps
  JOIN ledger.fiscal_period fp ON fp.tenant_id = ps.tenant_id AND fp.calendar_id = ps.calendar_id
       AND fp.fiscal_year = ps.fiscal_year AND fp.period = ps.period
  WHERE ps.tenant_id = NEW.tenant_id AND ps.legal_entity_id = NEW.legal_entity_id AND ps.ledger_id = NEW.ledger_id
    AND ps.fiscal_year = NEW.fiscal_year AND ps.period = NEW.fiscal_period
  FOR SHARE OF ps;
  IF st IS NULL THEN
    RAISE EXCEPTION 'period %/% is not defined for this legal entity and ledger', NEW.fiscal_year, NEW.fiscal_period USING ERRCODE = 'check_violation';
  END IF;
  IF st <> 'open' THEN
    RAISE EXCEPTION 'period %/% is %', NEW.fiscal_year, NEW.fiscal_period, st USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.posting_date NOT BETWEEN d_start AND d_end THEN
    RAISE EXCEPTION 'posting date % is outside period %/% (% to %)', NEW.posting_date, NEW.fiscal_year, NEW.fiscal_period, d_start, d_end
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

ALTER TABLE ledger.parked_entry DROP COLUMN fiscal_period;
