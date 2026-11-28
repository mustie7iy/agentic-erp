-- Period lot sizes (ADR-0084 As built, period lot sizes; 03-data-model
-- §3.37): a material's planning data at a plant may size its proposals
-- by period (SAP MARC-DISLS TB, WB and MB, from memory): `daily`,
-- `weekly` (ISO weeks, Monday to Sunday) or `monthly` (calendar months),
-- a lot per period of the net requirements that fall in it, needed by
-- the period's first requirement date, bounded and rounded as lot for
-- lot is. The fixed lot size and the maximum stock level stay their own
-- procedures' (material_plan_lot_size_fields, unchanged).
--
-- run_line_lot keeps each lot a period-sized line proposed with the day
-- it is needed by, in the order of the proposed requisition's items: a
-- requisition has one need date, so the lots' own days are the run's
-- record. Insert-only, as the run's lines are; only a line sized by a
-- period procedure has lots here, numbered within its lots.
--
-- Expand only (06 §6.4): the checks are widened and a table added, so a
-- server of the release before writes rows these checks accept and never
-- the table. During a rolling upgrade a worker of that release sizes a
-- period plan it meets lot for lot (its sizing knows no other), writing
-- a line naming the period procedure with no lots here and needed by the
-- run's day, and refuses a change of such a plan (422); both end with
-- the upgrade, and the next run plans by period.
ALTER TABLE mrp.material_plan
  DROP CONSTRAINT material_plan_lot_size_check,
  ADD CONSTRAINT material_plan_lot_size_check
    CHECK (lot_size IN ('lot_for_lot', 'fixed', 'replenish_to_maximum', 'daily', 'weekly', 'monthly'));
ALTER TABLE mrp.run_line
  DROP CONSTRAINT run_line_lot_size_check,
  ADD CONSTRAINT run_line_lot_size_check
    CHECK (lot_size IN ('lot_for_lot', 'fixed', 'replenish_to_maximum', 'daily', 'weekly', 'monthly'));

CREATE TABLE mrp.run_line_lot (
  tenant_id    uuid NOT NULL,
  run_id       uuid NOT NULL,
  material_id  uuid NOT NULL,
  lot_no       integer NOT NULL CONSTRAINT run_line_lot_no_check CHECK (lot_no BETWEEN 1 AND 100),
  quantity     numeric(20,6) NOT NULL CONSTRAINT run_line_lot_quantity_check CHECK (quantity > 0),
  needed_by    date NOT NULL,
  PRIMARY KEY (tenant_id, run_id, material_id, lot_no),
  CONSTRAINT run_line_lot_line_fkey FOREIGN KEY (tenant_id, run_id, material_id) REFERENCES mrp.run_line (tenant_id, run_id, material_id)
);

-- A lot is one of a period-sized line's lots: the line names a period
-- procedure and at least lot_no lots. The line is insert-only
-- (run_line_immutable), so what is read here does not move.
CREATE FUNCTION mrp.check_run_line_lot() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
      SELECT FROM mrp.run_line l
      WHERE l.tenant_id = NEW.tenant_id AND l.run_id = NEW.run_id AND l.material_id = NEW.material_id
        AND l.lot_size IN ('daily', 'weekly', 'monthly') AND l.lots >= NEW.lot_no) THEN
    RAISE EXCEPTION 'lot % of material % in planning run % is one of its period-sized line''s lots', NEW.lot_no, NEW.material_id, NEW.run_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_line_lot_of_period_line';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER run_line_lot_of_period_line BEFORE INSERT ON mrp.run_line_lot FOR EACH ROW EXECUTE FUNCTION mrp.check_run_line_lot();
CREATE TRIGGER run_line_lot_immutable BEFORE UPDATE OR DELETE ON mrp.run_line_lot FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('mrp.run_line_lot'::regclass);
