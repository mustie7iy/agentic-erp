-- A dunning run confirmed (T76b; ADR-0065 §8; 03-data-model §3.27): the
-- run moves from proposed to confirmed once, and each of its items' level
-- and dunning date (the run's key date) is recorded in
-- collections.dunned_item. From then on an item's level is the highest
-- recorded for it, which the next run reads as its previous level, so the
-- next run proposes the next level. What the run proposed stays as it was
-- (its notices and items are insert-only).
--
-- Expand only (06 §6.4): a status more, the update grant confirming
-- needs and a new table; the previous version starts proposals, which
-- stay valid, and neither reads nor writes the new table.
ALTER TABLE collections.dunning_run
  DROP CONSTRAINT dunning_run_status_check,
  ADD CONSTRAINT dunning_run_status_check CHECK (status IN ('proposed', 'confirmed'));

-- Confirming is the only change: a proposed run moves to confirmed, once,
-- and its company, key date and totals stay (the grant names the columns
-- confirming sets). A confirmed run is not changed again: cancelling one
-- comes later.
CREATE FUNCTION collections.check_dunning_run_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'proposed' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'dunning run %: only a proposed run changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dunning_run_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION collections.check_dunning_run_change() OWNER TO erp_owner;
CREATE TRIGGER dunning_run_confirmed_once BEFORE UPDATE ON collections.dunning_run
  FOR EACH ROW EXECUTE FUNCTION collections.check_dunning_run_change();
GRANT UPDATE (status, version, updated_by) ON collections.dunning_run TO erp_app;

-- How often an item had been dunned when a run proposed it: the count of
-- its rows in dunned_item then. A confirmation compares it with the count
-- as it is and refuses a proposal another confirmed run has overtaken;
-- the level alone would not show it, since an item at the highest level,
-- or short of the next level's days, is dunned again at the level it has.
-- Each confirmed run raises an item one level at most, so an item is at
-- no higher a level than the times it was dunned, and one never dunned
-- is at none. Proposals stored before have the default, 0, as their
-- previous_level is 0.
ALTER TABLE collections.dunning_notice_item
  ADD COLUMN previous_dunnings integer NOT NULL DEFAULT 0,
  ADD CONSTRAINT dunning_notice_item_dunnings_check
    CHECK (previous_dunnings >= previous_level AND (previous_dunnings = 0) = (previous_level = 0));

-- A dunned item: an open item of the ledger, named by its journal line,
-- at the level a confirmed run dunned it at, on the run's key date. It is
-- one of the items the run proposed (the key into dunning_notice_item).
-- An item has a row per confirmed run that took it; its level is the
-- highest among them and its last dunning date the latest. The key leads
-- with the item, which is how the next run, a confirmation and the
-- worklist read it.
CREATE TABLE collections.dunned_item (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,  -- ledger context: the journal line
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL,
  dunning_run_id    uuid NOT NULL,
  level             smallint NOT NULL CONSTRAINT dunned_item_level_check CHECK (level BETWEEN 1 AND 9),
  dunning_date      date NOT NULL,
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no, dunning_run_id),
  CONSTRAINT dunned_item_proposed_fkey FOREIGN KEY (tenant_id, dunning_run_id, fiscal_year, journal_entry_id, line_no)
    REFERENCES collections.dunning_notice_item (tenant_id, dunning_run_id, fiscal_year, journal_entry_id, line_no)
);
-- What was recorded is fixed.
CREATE TRIGGER dunned_item_immutable BEFORE UPDATE OR DELETE ON collections.dunned_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('collections.dunned_item'::regclass);
