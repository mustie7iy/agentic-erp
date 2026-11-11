-- The run a consolidation run's translation reserve opens from (ADR-0059
-- §11, As built: other comprehensive income; the accounting specialist's
-- B3, po:2026-11-09; IAS 21.39(c), 21.41, 21.52(b)): the group's final run
-- of the previous fiscal year's last period, recorded when the run starts
-- running, so that the fiscal year's translation difference its
-- statements show as other comprehensive income (each company's reserve
-- less its opening) never changes with a later finalization of that
-- period. Absent when the group had no such run: the reserve opens at
-- zero (product owner 2026-11-05, question 4: consolidation starts at
-- go-live with the reserve at zero). No foreign key, as superseded_by has
-- none: a run is never deleted, and an import restores runs in any order.
-- Expand only: a nullable column the previous version neither reads nor
-- writes; a run it started keeps none.
ALTER TABLE consolidation.run ADD COLUMN opening_run_id uuid;

-- The opening is named once, as the run starts running, and is then the
-- group's final run of the previous fiscal year's period 12 (a run's
-- periods are the regular ones, 1 to 12: run_regular_period). An import
-- (the tenant app.importing names, ADR-0029, 000230) inserts a run as
-- exported.
CREATE FUNCTION consolidation.check_run_opening() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.opening_run_id IS NOT NULL AND NOT platform.importing(NEW.tenant_id) THEN
      RAISE EXCEPTION 'a consolidation run names its opening run when it starts running, not when it is queued'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'run_opening_fixed';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.opening_run_id IS NOT DISTINCT FROM OLD.opening_run_id THEN
    RETURN NEW;
  END IF;
  IF OLD.opening_run_id IS NOT NULL OR NEW.opening_run_id IS NULL OR OLD.status <> 'queued' OR NEW.status <> 'running' THEN
    RAISE EXCEPTION 'consolidation run % names its opening run once, when it starts running', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_opening_fixed';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM consolidation.run o WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.opening_run_id
      AND o.group_code = NEW.group_code AND o.fiscal_year = NEW.fiscal_year - 1 AND o.fiscal_period = 12 AND o.status = 'final') THEN
    RAISE EXCEPTION 'the opening run of consolidation run % is the final run of group % for %/12; run % is not', OLD.id, NEW.group_code,
      NEW.fiscal_year - 1, NEW.opening_run_id USING ERRCODE = 'check_violation', CONSTRAINT = 'run_opening_final';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION consolidation.check_run_opening() OWNER TO erp_owner;
CREATE TRIGGER run_opening BEFORE INSERT OR UPDATE ON consolidation.run FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_opening();
