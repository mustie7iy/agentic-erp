-- Postings into the special periods 13-16 (ADR-0060 §3, T64m1): manual
-- and parked entries and their reversals, dated in the year's last
-- regular period, whose dates the special periods share.
--
-- A parked entry may name the special period it is to be posted into; it
-- keeps it and is held to the rules again when it is posted. Null: the
-- regular period of its posting date, as before. Expand only (06 §6.4): a
-- nullable column without a default rewrites nothing, and its check is
-- added NOT VALID, reading none of the rows under the lock; 000226
-- validates it. The previous release never writes the column.
ALTER TABLE ledger.parked_entry
  ADD COLUMN fiscal_period smallint,
  ADD CONSTRAINT parked_entry_fiscal_period_special CHECK (fiscal_period BETWEEN 13 AND 16) NOT VALID;

-- The posting-period trigger as 000043 left it, and a special period takes
-- only a manual entry (source ledger: posted directly, or as a parked
-- entry) or a reversal: every other posting goes to the regular period of
-- its date. The date rule needs nothing new: a special period's dates are
-- its year's last regular period's, so an entry dated elsewhere is outside
-- them. The ledger checks all of it first and answers with a problem
-- (ledger.special_period_date); these are its backstop, so a caller that
-- reaches them gets a 500. The special flag is read in the same row.
CREATE OR REPLACE FUNCTION ledger.check_entry_period() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text; d_start date; d_end date; special boolean;
BEGIN
  SELECT ps.status, fp.start_date, fp.end_date, fp.is_special INTO st, d_start, d_end, special
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
  IF special AND NEW.source_context <> 'ledger' AND NEW.reversal_of_id IS NULL THEN
    RAISE EXCEPTION 'special period %/% takes manual entries and reversals only, not a % posting', NEW.fiscal_year, NEW.fiscal_period, NEW.source_context
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
