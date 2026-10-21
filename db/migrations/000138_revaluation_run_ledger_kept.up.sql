-- A revaluation run values and posts in one ledger of its legal entity
-- (T52f, ADR-0045): the database holds a run to a ledger the legal entity
-- keeps, as postings are held (journal_entry's ledger check); a run without
-- entries had only the application's check. Expand only: a new trigger on
-- insert, since a run's ledger never changes (revaluation_run_fixed).
CREATE FUNCTION ledger.check_revaluation_run_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT ledger.company_keeps(NEW.legal_entity_id, NEW.ledger_id) THEN
    RAISE EXCEPTION 'revaluation run %: legal entity % does not keep ledger %', NEW.id, NEW.legal_entity_id, NEW.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'revaluation_run_ledger_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_revaluation_run_ledger() OWNER TO erp_owner;
CREATE TRIGGER revaluation_run_ledger_kept BEFORE INSERT ON ledger.revaluation_run
  FOR EACH ROW EXECUTE FUNCTION ledger.check_revaluation_run_ledger();
