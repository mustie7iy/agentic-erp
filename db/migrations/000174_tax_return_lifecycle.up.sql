-- The return lifecycle (T61i; ADR-0049 §11-§13; 03-data-model §3.20). A
-- return of a registration's period is prepared from the tax items no
-- return has reported: a snapshot of its boxes, the keys of the items it
-- claimed and their hash, and the country pack version that defined it;
-- the items' reported_in names it, set in the same transaction under a
-- lock on the registration, so an item is reported once. It is approved by
-- a person other than its preparer (a tax_return approval request,
-- workflow context), recorded as filed by a person with the authority's
-- confirmation (P3), or withdrawn or rejected before filing, which
-- releases its items. A filed return is amended by a further return of
-- kind amendment for the same period, which claims the items dated in the
-- period that arrived after it. registration_id is this schema's; the
-- items are the ledger's, by key, with no foreign key across the schema.
-- Expand only: new tables, a trigger the previous version never fires
-- (it never sets reported_in), and a ledger check made to allow a release,
-- which the previous version never asks for.
SELECT platform.create_event_tables('tax');
SELECT platform.secure_table('tax.outbox'::regclass);
SELECT platform.secure_table('tax.inbox'::regclass);
GRANT UPDATE (published_at) ON tax.outbox TO erp_app;
GRANT USAGE ON SCHEMA tax TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON tax.outbox TO erp_relay;

CREATE TABLE tax.tax_return (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  registration_id      uuid NOT NULL,
  period_start         date NOT NULL,
  period_end           date NOT NULL CHECK (period_end >= period_start),
  kind                 text NOT NULL CHECK (kind IN ('original', 'amendment')),
  amends_id            uuid,              -- the filed return of the period an amendment corrects
  status               text NOT NULL DEFAULT 'prepared' CHECK (status IN ('prepared', 'approved', 'filed', 'withdrawn', 'rejected')),
  -- The definition the boxes follow: the pack version the registrant's
  -- company had adopted (an amendment keeps the amended return's).
  return_type          text NOT NULL,
  return_name          text NOT NULL,
  pack_country         char(2) NOT NULL,
  pack_version         text NOT NULL,
  currency             char(3) NOT NULL REFERENCES ref.currency,   -- the registrant's local currency
  item_count           bigint NOT NULL CHECK (item_count >= 0),
  items_hash           text NOT NULL CHECK (items_hash ~ '^[0-9a-f]{64}$'),
  prior_item_count     bigint NOT NULL CHECK (prior_item_count BETWEEN 0 AND item_count),
  prior_tax            bigint NOT NULL,    -- minor units of currency, signed like the journal
  prior_threshold      bigint,             -- the pack's threshold; null when it sets none
  above_threshold      boolean NOT NULL,
  approval_request_id  uuid NOT NULL,      -- workflow context
  approved_by          uuid,
  approved_at          timestamptz,
  confirmation_number  text CHECK (btrim(confirmation_number) <> '' AND length(confirmation_number) <= 100),
  filed_on             date,
  filed_by             uuid,
  filed_at             timestamptz,
  decision_comment     text CHECK (length(decision_comment) <= 2000),
  decided_by           uuid,               -- who withdrew or rejected it
  decided_at           timestamptz,
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,      -- the preparer
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, registration_id) REFERENCES tax.registration (tenant_id, id),
  FOREIGN KEY (tenant_id, amends_id) REFERENCES tax.tax_return (tenant_id, id),
  CONSTRAINT tax_return_amends CHECK ((kind = 'amendment') = (amends_id IS NOT NULL)),
  CONSTRAINT tax_return_approved CHECK ((approved_by IS NULL) = (approved_at IS NULL)
    AND (status NOT IN ('approved', 'filed') OR approved_by IS NOT NULL)),
  -- Four eyes: the approver is never the preparer (ADR-0049 §11).
  CONSTRAINT tax_return_four_eyes CHECK (approved_by <> created_by),
  CONSTRAINT tax_return_filed CHECK ((status = 'filed') = (confirmation_number IS NOT NULL AND filed_on IS NOT NULL AND filed_by IS NOT NULL
    AND filed_at IS NOT NULL) AND filed_on > period_end),
  CONSTRAINT tax_return_ended CHECK ((status IN ('withdrawn', 'rejected')) = (decided_by IS NOT NULL AND decided_at IS NOT NULL))
);
-- One return of a period in progress, and one original that stands.
CREATE UNIQUE INDEX tax_return_in_progress ON tax.tax_return (tenant_id, registration_id, period_start) WHERE status IN ('prepared', 'approved');
CREATE UNIQUE INDEX tax_return_one_original ON tax.tax_return (tenant_id, registration_id, period_start)
  WHERE kind = 'original' AND status IN ('prepared', 'approved', 'filed');
CREATE INDEX tax_return_by_registration ON tax.tax_return (tenant_id, registration_id, period_start, id);
CREATE TRIGGER tax_return_touch BEFORE UPDATE ON tax.tax_return FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A return starts prepared (an amendment of its period's filed return)
-- and moves prepared -> approved -> filed, or to withdrawn or rejected
-- before filing. Each move sets its own fields and nothing else: the
-- snapshot never changes, and a filed, withdrawn or rejected return is
-- final.
CREATE FUNCTION tax.check_tax_return() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  allowed text[];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'prepared' THEN
      RAISE EXCEPTION 'a tax return starts prepared, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_return_transition';
    END IF;
    IF NEW.amends_id IS NOT NULL AND NOT EXISTS (SELECT FROM tax.tax_return a WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.amends_id
        AND a.status = 'filed' AND a.registration_id = NEW.registration_id AND a.period_start = NEW.period_start) THEN
      RAISE EXCEPTION 'an amendment corrects a filed return of its registration and period'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_return_amends_filed';
    END IF;
    RETURN NEW;
  END IF;
  allowed := CASE
    WHEN OLD.status = 'prepared' AND NEW.status = 'approved' THEN ARRAY['approved_by', 'approved_at']
    WHEN OLD.status = 'approved' AND NEW.status = 'filed' THEN ARRAY['confirmation_number', 'filed_on', 'filed_by', 'filed_at']
    WHEN OLD.status = 'prepared' AND NEW.status IN ('withdrawn', 'rejected') OR OLD.status = 'approved' AND NEW.status = 'withdrawn'
      THEN ARRAY['decision_comment', 'decided_by', 'decided_at'] END;
  IF allowed IS NULL THEN
    RAISE EXCEPTION 'tax return % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_return_transition';
  END IF;
  allowed := allowed || ARRAY['status', 'version', 'updated_at', 'updated_by'];
  IF to_jsonb(NEW) - allowed IS DISTINCT FROM to_jsonb(OLD) - allowed THEN
    RAISE EXCEPTION 'tax return % changes only what its move from % to % sets', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_return_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION tax.check_tax_return() OWNER TO erp_owner;
CREATE TRIGGER tax_return_transition BEFORE INSERT OR UPDATE ON tax.tax_return FOR EACH ROW EXECUTE FUNCTION tax.check_tax_return();
SELECT platform.secure_table('tax.tax_return'::regclass);

-- The snapshot's boxes, in the definition's order: what the return's
-- items sum to (amount, and its prior-period part) and, for an amendment,
-- the period's figure it corrects to (corrected_amount: the amended
-- return's plus this one's; an original's is its amount). Minor units of
-- the return's currency.
CREATE TABLE tax.tax_return_box (
  tenant_id            uuid NOT NULL,
  tax_return_id        uuid NOT NULL,
  box_no               smallint NOT NULL CHECK (box_no > 0),
  code                 text NOT NULL CHECK (code ~ '^[a-z0-9][a-z0-9_]{0,19}$'),
  label                text NOT NULL,
  kind                 text NOT NULL CHECK (kind IN ('selection', 'expression')),
  amount               bigint NOT NULL,
  prior_period_amount  bigint NOT NULL,
  corrected_amount     bigint NOT NULL,
  item_count           bigint CHECK (item_count >= 0),
  PRIMARY KEY (tenant_id, tax_return_id, box_no),
  CONSTRAINT tax_return_box_code_once UNIQUE (tenant_id, tax_return_id, code),
  FOREIGN KEY (tenant_id, tax_return_id) REFERENCES tax.tax_return (tenant_id, id),
  CONSTRAINT tax_return_box_items CHECK ((kind = 'selection') = (item_count IS NOT NULL))
);

-- The tax items the return claimed (ledger context, by key): their
-- reported_in names the return while it stands.
CREATE TABLE tax.tax_return_item (
  tenant_id         uuid NOT NULL,
  tax_return_id     uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  item_no           integer NOT NULL,
  PRIMARY KEY (tenant_id, tax_return_id, fiscal_year, journal_entry_id, item_no),
  FOREIGN KEY (tenant_id, tax_return_id) REFERENCES tax.tax_return (tenant_id, id)
);

-- Boxes and items are written with the return, while it is prepared, and
-- never change: a filed snapshot is immutable (ADR-0049 §12).
CREATE FUNCTION tax.check_snapshot_part() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT FROM tax.tax_return r WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.tax_return_id AND r.status = 'prepared') THEN
    RAISE EXCEPTION 'the snapshot of tax return % is written while it is prepared', NEW.tax_return_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_return_snapshot';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION tax.check_snapshot_part() OWNER TO erp_owner;
CREATE TRIGGER tax_return_box_snapshot BEFORE INSERT ON tax.tax_return_box FOR EACH ROW EXECUTE FUNCTION tax.check_snapshot_part();
CREATE TRIGGER tax_return_item_snapshot BEFORE INSERT ON tax.tax_return_item FOR EACH ROW EXECUTE FUNCTION tax.check_snapshot_part();
CREATE TRIGGER tax_return_box_insert_only BEFORE UPDATE OR DELETE ON tax.tax_return_box FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER tax_return_item_insert_only BEFORE UPDATE OR DELETE ON tax.tax_return_item FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('tax.tax_return_box'::regclass);
SELECT platform.secure_table('tax.tax_return_item'::regclass);

-- An item's reported_in is set from null to a return, or released from a
-- return back to null, never moved from one return to another; nothing
-- else of it changes and none is deleted (000144, made to allow the
-- release a withdrawn return makes).
CREATE OR REPLACE FUNCTION ledger.check_tax_item_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR (OLD.reported_in IS NULL) = (NEW.reported_in IS NULL)
     OR to_jsonb(NEW) - 'reported_in' IS DISTINCT FROM to_jsonb(OLD) - 'reported_in' THEN
    RAISE EXCEPTION 'tax item % of entry % is fixed but for the return that reports it, set or released', OLD.item_no, OLD.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_fixed';
  END IF;
  RETURN NEW;
END $$;

-- At commit: an item reported in a return is in its snapshot, of a
-- company its registration covers, dated by its period's end, and the
-- return stands; an item released was reported in a return withdrawn or
-- rejected. The tax filing context's rule on the ledger's items.
CREATE FUNCTION tax.check_item_reported() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.reported_in IS NOT NULL THEN
    IF NOT EXISTS (SELECT FROM tax.tax_return r JOIN tax.registration g ON g.tenant_id = r.tenant_id AND g.id = r.registration_id
        JOIN tax.tax_return_item ri ON ri.tenant_id = r.tenant_id AND ri.tax_return_id = r.id
        WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.reported_in AND r.status IN ('prepared', 'approved', 'filed') AND NEW.tax_date <= r.period_end
          AND (NEW.legal_entity_id = g.legal_entity_id OR NEW.legal_entity_id = ANY (g.group_member_ids))
          AND ri.fiscal_year = NEW.fiscal_year AND ri.journal_entry_id = NEW.journal_entry_id AND ri.item_no = NEW.item_no) THEN
      RAISE EXCEPTION 'tax item % of entry % is reported in a return that does not stand or does not claim it', NEW.item_no, NEW.journal_entry_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_reported';
    END IF;
  ELSIF NOT EXISTS (SELECT FROM tax.tax_return r WHERE r.tenant_id = OLD.tenant_id AND r.id = OLD.reported_in AND r.status IN ('withdrawn', 'rejected')) THEN
    RAISE EXCEPTION 'tax item % of entry % is released by a return that stands', OLD.item_no, OLD.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_reported';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION tax.check_item_reported() OWNER TO erp_owner;
-- What a withdrawal releases and the check above reads: a return's items.
CREATE INDEX tax_item_reported_in ON ledger.tax_item (tenant_id, reported_in) WHERE reported_in IS NOT NULL;
CREATE CONSTRAINT TRIGGER tax_item_reported AFTER UPDATE OF reported_in ON ledger.tax_item DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION tax.check_item_reported();

-- Preparing and withdrawing are a tax accountant's, recording a return
-- filed a person's who filed it; approving is an approval permission,
-- which agents cannot hold. Every transition is human-only (§13).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('tax.return.prepare', 'Prepare, amend and withdraw tax returns, claiming their tax items', false),
  ('tax.return.approve', 'Approve tax returns another person prepared', true),
  ('tax.return.file', 'Record a tax return as filed with the authority''s confirmation, locking its fiscal periods', false);
