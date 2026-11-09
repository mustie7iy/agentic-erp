-- An import restores a period's opened count as exported (ADR-0029,
-- ADR-0040; ADR-0060 §6; found by batch58's ci-local): 000227's trigger
-- starts every inserted period at 0 (1 when open) and refuses a count
-- given, so a tenant's export, whose periods carry their counts, could
-- not be imported, and a count reset on import would let a carry-forward
-- run read before a later opening look current again. The setting
-- app.importing names the tenant being imported for the transaction, as
-- app.erasing names the one being erased (000023); only the importer sets
-- it (erp_app could, but row-level security confines it to the tenant's
-- own rows), and an insert in that tenant then keeps the count it carries.
-- An export written before 000227 carries none: its NULL is filled as
-- 000227 filled existing and inserted rows. Every other insert, and every
-- update, is held as before.
CREATE OR REPLACE FUNCTION ledger.count_period_openings() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF coalesce(current_setting('app.importing', true), '') = NEW.tenant_id::text THEN
      IF NEW.opened_count IS NULL THEN
        NEW.opened_count := (NEW.status = 'open')::int;
      END IF;
      RETURN NEW;
    END IF;
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
