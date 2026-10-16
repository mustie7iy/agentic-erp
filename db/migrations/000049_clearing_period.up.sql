-- A clearing's entry is one of the clearing's legal entity, and the
-- clearing is dated in an open fiscal period of that legal entity and the
-- entry's ledger (T32, 03-data-model §3.10), as an entry's posting date is.
-- A clearing that posts its entry was held to this by the entry's own
-- trigger, but one a migration records against an entry already posted
-- (Postings.Clear) posts nothing, so the rule is on the clearing itself.
-- The period's status row is held FOR SHARE until the transaction ends, as
-- check_entry_period does: a close or lock takes it FOR UPDATE and waits.
-- The ledger takes the same lock first in the application, after the
-- entry buckets, so this adds no lock order.
CREATE FUNCTION ledger.check_clearing_period() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE entity uuid; book uuid; st text; y smallint; n smallint;
BEGIN
  SELECT e.legal_entity_id, e.ledger_id INTO entity, book FROM ledger.journal_entry e
  WHERE e.tenant_id = NEW.tenant_id AND e.fiscal_year = NEW.clearing_entry_year AND e.id = NEW.clearing_entry_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'clearing entry %/% does not exist', NEW.clearing_entry_year, NEW.clearing_entry_id USING ERRCODE = 'foreign_key_violation';
  END IF;
  IF entity <> NEW.legal_entity_id THEN
    RAISE EXCEPTION 'clearing entry % belongs to another legal entity', NEW.clearing_entry_id USING ERRCODE = 'check_violation';
  END IF;
  SELECT ps.status, ps.fiscal_year, ps.period INTO st, y, n
  FROM ledger.period_status ps
  JOIN ledger.fiscal_period fp ON fp.tenant_id = ps.tenant_id AND fp.calendar_id = ps.calendar_id
       AND fp.fiscal_year = ps.fiscal_year AND fp.period = ps.period
  WHERE ps.tenant_id = NEW.tenant_id AND ps.legal_entity_id = entity AND ps.ledger_id = book
    AND NOT fp.is_special AND NEW.clearing_date BETWEEN fp.start_date AND fp.end_date
  FOR SHARE OF ps;
  IF st IS NULL THEN
    RAISE EXCEPTION 'no fiscal period of the legal entity and ledger covers clearing date %', NEW.clearing_date USING ERRCODE = 'check_violation';
  END IF;
  IF st <> 'open' THEN
    RAISE EXCEPTION 'period %/% of clearing date % is %', y, n, NEW.clearing_date, st USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER clearing_period BEFORE INSERT ON ledger.clearing FOR EACH ROW EXECUTE FUNCTION ledger.check_clearing_period();
