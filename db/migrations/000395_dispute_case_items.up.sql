-- A dispute case over several invoices (T86c; ADR-0065 As built;
-- 03-data-model §3.27): a case names one or more open items of one
-- customer, each with its own disputed amount. The items are the case's
-- items, numbered from 1; the case's own journal line is its first item's
-- and its amounts are its items' totals, which a deferred check holds at
-- commit. An item is in one open case at a time: the unique index moves
-- to the items, each of which carries whether its case is open (kept by
-- the case's change, never by the application). The items' journal lines
-- are the ledger's: no foreign key leaves the schema (02 §2.1).
--
-- Expand, then migrate (06 §6.4): a new table the previous version
-- neither reads nor writes, and the existing cases' items written from
-- their own line. A case the previous version would open during the
-- rollout, without items, is refused at commit by the deferred check
-- rather than left out of the dunning holds that now read the items.
-- dispute_case_one_open on the case's line stays: the items' index
-- covers it, and dropping it is a later contract step.

-- An item of a case: its journal line, what the case read of it (the
-- document number and reference), the disputed amount in the case's
-- currency, more than zero, and its share of the item's amount in the
-- company's as booked. id is the item's own, which a document made from it
-- names, and account_id the item's account, which a write-off credits
-- (T86d). open is whether its case is open.
CREATE TABLE collections.dispute_case_item (
  tenant_id           uuid NOT NULL,
  dispute_case_id     uuid NOT NULL,
  item_no             smallint NOT NULL CONSTRAINT dispute_case_item_no_check CHECK (item_no BETWEEN 1 AND 50),
  id                  uuid NOT NULL,
  fiscal_year         smallint NOT NULL, -- ledger context: the journal line
  journal_entry_id    uuid NOT NULL,
  line_no             integer NOT NULL CONSTRAINT dispute_case_item_line_no_check CHECK (line_no > 0),
  account_id          uuid NOT NULL,     -- ledger context: the line's account
  document_number     text NOT NULL CONSTRAINT dispute_case_item_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  reference           text,
  disputed_amount     bigint NOT NULL CONSTRAINT dispute_case_item_amount_check CHECK (disputed_amount > 0),
  disputed_amount_lc  bigint NOT NULL CONSTRAINT dispute_case_item_amount_lc_check CHECK (disputed_amount_lc >= 0),
  open                boolean NOT NULL DEFAULT true,
  PRIMARY KEY (tenant_id, dispute_case_id, item_no),
  CONSTRAINT dispute_case_item_id_key UNIQUE (tenant_id, id),
  -- An item is named once in a case.
  CONSTRAINT dispute_case_item_once UNIQUE (tenant_id, dispute_case_id, fiscal_year, journal_entry_id, line_no),
  CONSTRAINT dispute_case_item_case_fkey FOREIGN KEY (tenant_id, dispute_case_id) REFERENCES collections.dispute_case (tenant_id, id)
);
-- An item is in one open case at a time, whether the second case comes
-- after the first or races it.
CREATE UNIQUE INDEX dispute_case_item_one_open ON collections.dispute_case_item (tenant_id, fiscal_year, journal_entry_id, line_no) WHERE open;
SELECT platform.secure_table('collections.dispute_case_item'::regclass);
-- The application inserts items with their case; it changes and deletes
-- none (the grant), and no role changes what an item names or reopens it
-- (the trigger): only its case's close marks it no longer open.
REVOKE UPDATE, DELETE ON collections.dispute_case_item FROM erp_app;
CREATE FUNCTION collections.check_dispute_case_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR (to_jsonb(NEW) - 'open') <> (to_jsonb(OLD) - 'open') OR (NEW.open AND NOT OLD.open) THEN
    RAISE EXCEPTION 'dispute case item %/%: an item stays as its case named it, and is closed with its case', OLD.dispute_case_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_item_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION collections.check_dispute_case_item_change() OWNER TO erp_owner;
CREATE TRIGGER dispute_case_item_fixed BEFORE UPDATE OR DELETE ON collections.dispute_case_item
  FOR EACH ROW EXECUTE FUNCTION collections.check_dispute_case_item_change();

-- A case closed closes its items: they are no longer in an open case.
CREATE FUNCTION collections.close_dispute_case_items() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE collections.dispute_case_item SET open = false WHERE tenant_id = NEW.tenant_id AND dispute_case_id = NEW.id AND open;
  RETURN NULL;
END $$;
ALTER FUNCTION collections.close_dispute_case_items() OWNER TO erp_owner;
CREATE TRIGGER dispute_case_items_closed AFTER UPDATE OF status ON collections.dispute_case
  FOR EACH ROW WHEN (OLD.status = 'open' AND NEW.status <> 'open') EXECUTE FUNCTION collections.close_dispute_case_items();

-- At commit, a case has items: its first is its own journal line, their
-- disputed amounts add up to its own in both currencies, and each is open
-- exactly while the case is. Checked once per case written, whether its
-- row or an item of it, as the role that wrote it, which reads the
-- tenant's cases (the application) or every case (a migration).
CREATE FUNCTION collections.check_dispute_case_items() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  c collections.dispute_case;
  case_id uuid;
  n integer;
  total numeric;
  total_lc numeric;
  first_line boolean;
  mismatched integer;
BEGIN
  IF TG_TABLE_NAME = 'dispute_case' THEN
    case_id := NEW.id;
  ELSE
    case_id := NEW.dispute_case_id;
  END IF;
  SELECT * INTO c FROM collections.dispute_case WHERE tenant_id = NEW.tenant_id AND id = case_id;
  SELECT count(*), sum(i.disputed_amount), sum(i.disputed_amount_lc),
         bool_or(i.item_no = 1 AND i.fiscal_year = c.fiscal_year AND i.journal_entry_id = c.journal_entry_id AND i.line_no = c.line_no),
         count(*) FILTER (WHERE i.open <> (c.status = 'open'))
    INTO n, total, total_lc, first_line, mismatched
    FROM collections.dispute_case_item i WHERE i.tenant_id = c.tenant_id AND i.dispute_case_id = c.id;
  IF n = 0 OR total <> c.disputed_amount OR total_lc <> c.disputed_amount_lc OR NOT first_line OR mismatched > 0 THEN
    RAISE EXCEPTION 'dispute case %: its items are its amounts, its first is its own line, and they are open while it is', c.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_items';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION collections.check_dispute_case_items() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER dispute_case_items AFTER INSERT OR UPDATE ON collections.dispute_case
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION collections.check_dispute_case_items();
CREATE CONSTRAINT TRIGGER dispute_case_items AFTER INSERT OR UPDATE ON collections.dispute_case_item
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION collections.check_dispute_case_items();

-- The existing cases' items: each case's own line, its only item, on the
-- line's account.
INSERT INTO collections.dispute_case_item (tenant_id, dispute_case_id, item_no, id, fiscal_year, journal_entry_id, line_no, account_id,
    document_number, reference, disputed_amount, disputed_amount_lc, open)
  SELECT d.tenant_id, d.id, 1, gen_random_uuid(), d.fiscal_year, d.journal_entry_id, d.line_no, l.account_id, d.document_number, d.reference,
    d.disputed_amount, d.disputed_amount_lc, d.status = 'open'
  FROM collections.dispute_case d
  JOIN ledger.journal_line l ON l.tenant_id = d.tenant_id AND l.fiscal_year = d.fiscal_year AND l.journal_entry_id = d.journal_entry_id
    AND l.line_no = d.line_no
  ORDER BY d.tenant_id, d.id;
