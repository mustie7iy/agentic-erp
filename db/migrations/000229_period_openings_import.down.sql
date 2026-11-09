-- 000227's trigger function: every insert starts at 0 (1 when open).
CREATE OR REPLACE FUNCTION ledger.count_period_openings() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.opened_count <> 0 THEN
      RAISE EXCEPTION 'period %/% starts with an opened count of 0, not %', NEW.fiscal_year, NEW.period, NEW.opened_count
        USING ERRCODE = 'check_violation', CONSTRAINT = 'period_status_opened_count_set';
    END IF;
    -- A period inserted open (test fixtures do) has been opened once.
    NEW.opened_count := (NEW.status = 'open')::int;
    RETURN NEW;
  END IF;
  IF NEW.opened_count IS DISTINCT FROM OLD.opened_count THEN
    RAISE EXCEPTION 'period %/%: the opened count is kept by the database, not set', OLD.fiscal_year, OLD.period
      USING ERRCODE = 'check_violation', CONSTRAINT = 'period_status_opened_count_set';
  END IF;
  IF NEW.status = 'open' AND OLD.status <> 'open' THEN
    NEW.opened_count := OLD.opened_count + 1;
  END IF;
  RETURN NEW;
END $$;
