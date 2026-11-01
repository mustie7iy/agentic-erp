-- Recurring and template entries (ADR-0056, product owner 2026-11-01). A
-- template is a saved parked-entry body: header and lines as a parked
-- entry has them, without its dates. A person parks an entry from it once
-- (entry-templates/{id}:park), or gives it a schedule and the ERP parks
-- one entry per due date for the template's owner, who posts it as any
-- parked entry (T35): never posted automatically.
CREATE TABLE ledger.entry_template (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  legal_entity_id    uuid NOT NULL,                 -- tenancy context
  name               text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
  ledger_id          uuid NOT NULL,                 -- as on a parked entry: the ledger named, the group's representative or the leading one
  ledger_specific    boolean NOT NULL DEFAULT false,
  ledger_group_id    uuid,
  document_type      text NOT NULL CHECK (document_type ~ '^[A-Z0-9]{2}$'),
  currency           char(3) NOT NULL REFERENCES ref.currency,
  reference          text,
  header_text        text,
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
  created_by_kind    text NOT NULL CHECK (created_by_kind IN ('human', 'agent', 'service')),
  owner_id           uuid,                          -- identity context: the one person who changes or retires it, and owns what it parks
  -- The schedule, all or nothing but its end: every 1, 3 or 12 months on
  -- day_of_month (the month's last day when shorter), from starts_on,
  -- until ends_on or for occurrences dates, or without end.
  frequency          text CHECK (frequency IN ('monthly', 'quarterly', 'yearly')),
  day_of_month       smallint CHECK (day_of_month BETWEEN 1 AND 31),
  starts_on          date,
  ends_on            date,
  occurrences        integer CHECK (occurrences > 0),
  -- Kept by the ERP: the schedule's next date after the last one it
  -- parked or refused (null without a schedule or once it has ended), the
  -- last date it parked, and the last date it could not park, with why.
  next_due_on        date,
  last_occurrence_on date,
  refused_on         date,
  refusal_code       text,
  refusal_detail     text,
  version            integer NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT entry_template_name_once UNIQUE (tenant_id, legal_entity_id, name),
  FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id),
  FOREIGN KEY (tenant_id, ledger_group_id) REFERENCES ledger.ledger_group (tenant_id, id),
  CONSTRAINT entry_template_ledger_or_group CHECK (NOT (ledger_specific AND ledger_group_id IS NOT NULL)),
  CONSTRAINT entry_template_person_owns CHECK (created_by_kind <> 'human' OR owner_id IS NOT DISTINCT FROM created_by),
  CONSTRAINT entry_template_schedule CHECK ((frequency IS NULL) = (day_of_month IS NULL) AND (frequency IS NULL) = (starts_on IS NULL)
      AND (frequency IS NOT NULL OR (ends_on IS NULL AND occurrences IS NULL AND next_due_on IS NULL))
      AND (ends_on IS NULL OR occurrences IS NULL) AND (ends_on IS NULL OR ends_on >= starts_on)
      AND (next_due_on IS NULL OR next_due_on >= starts_on)),
  CONSTRAINT entry_template_refusal CHECK ((refused_on IS NULL) = (refusal_code IS NULL) AND (refused_on IS NULL) = (refusal_detail IS NULL))
);
-- The dispatcher's question across tenants: the active templates a person
-- owns that are due.
CREATE INDEX entry_template_due ON ledger.entry_template (next_due_on) WHERE status = 'active' AND owner_id IS NOT NULL;
CREATE TRIGGER entry_template_touch BEFORE UPDATE ON ledger.entry_template FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Who made a template, and when, its company and the ledgers it posts to
-- never change; its owner never changes once a person owns it; a retired
-- template changes no more.
CREATE FUNCTION ledger.check_entry_template_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'retired' THEN
    RAISE EXCEPTION 'entry template % is retired', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF (NEW.id, NEW.legal_entity_id, NEW.created_by_kind, NEW.created_by, NEW.created_at, NEW.ledger_specific, NEW.ledger_group_id)
     IS DISTINCT FROM (OLD.id, OLD.legal_entity_id, OLD.created_by_kind, OLD.created_by, OLD.created_at, OLD.ledger_specific, OLD.ledger_group_id) THEN
    RAISE EXCEPTION 'who made entry template %, when, its company and its ledgers do not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.owner_id IS NOT NULL AND NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
    RAISE EXCEPTION 'the owner of entry template % does not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_entry_template_change() OWNER TO erp_owner;
CREATE TRIGGER entry_template_change BEFORE UPDATE ON ledger.entry_template
  FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_template_change();

-- Lines as a parked entry's (000052, 000147, 000150, 000159): minor units
-- of the template's currency, debit positive; replaced without deletes
-- (is_removed) while the template is active.
CREATE TABLE ledger.entry_template_line (
  tenant_id         uuid NOT NULL,
  template_id       uuid NOT NULL,
  line_no           integer NOT NULL CHECK (line_no > 0),
  account_id        uuid NOT NULL,
  amount            bigint NOT NULL CHECK (amount <> 0),
  partner_id        uuid,                             -- partner context
  tax_code          text,
  quantity          numeric(20,6),
  uom_code          text REFERENCES ref.unit_of_measure,
  line_text         text,
  tax_base          bigint,
  tax_jurisdiction  text,
  cost_center_id    uuid,
  profit_center_id  uuid,
  segment_id        uuid,
  is_removed        boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, template_id, line_no),
  FOREIGN KEY (tenant_id, template_id) REFERENCES ledger.entry_template (tenant_id, id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES ledger.gl_account (tenant_id, id),
  FOREIGN KEY (tenant_id, cost_center_id) REFERENCES ledger.cost_center (tenant_id, id),
  FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id),
  FOREIGN KEY (tenant_id, segment_id) REFERENCES ledger.segment (tenant_id, id),
  CHECK ((quantity IS NULL) = (uom_code IS NULL)),
  CONSTRAINT entry_template_line_tax_base CHECK (tax_base IS NULL OR tax_code IS NOT NULL),
  CONSTRAINT entry_template_line_tax_jurisdiction
    CHECK (tax_jurisdiction IS NULL OR (tax_code IS NOT NULL AND tax_jurisdiction ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'))
);
CREATE TRIGGER entry_template_line_editable BEFORE INSERT OR UPDATE ON ledger.entry_template_line
  FOR EACH ROW EXECUTE FUNCTION platform.check_editable_parent('ledger.entry_template', 'template_id', 'active');

SELECT platform.secure_table('ledger.entry_template'::regclass);
SELECT platform.secure_table('ledger.entry_template_line'::regclass);
GRANT UPDATE ON ledger.entry_template_line TO erp_app; -- no version column, so secure_table grants none

-- The dispatcher finds the due templates across tenants (ADR-0056).
GRANT SELECT (tenant_id, id, status, owner_id, next_due_on) ON ledger.entry_template TO erp_worker;

-- A parked entry made from a template names it, and one the ERP parked
-- for a due date names that date too: one entry per template and date,
-- however often its workflow runs. The ERP's own entries are a system
-- principal's ('system'), always with the template's owner as owner.
-- Expand only (06 §6.4): nullable columns without a default, the checks
-- and key added NOT VALID; 000185 validates them.
ALTER TABLE ledger.parked_entry
  ADD COLUMN template_id uuid,
  ADD COLUMN occurrence_date date,
  DROP CONSTRAINT parked_entry_created_by_kind_check,
  ADD CONSTRAINT parked_entry_created_by_kind CHECK (created_by_kind IN ('human', 'agent', 'service', 'system')) NOT VALID,
  ADD CONSTRAINT parked_entry_template_fkey FOREIGN KEY (tenant_id, template_id) REFERENCES ledger.entry_template (tenant_id, id) NOT VALID,
  ADD CONSTRAINT parked_entry_occurrence CHECK ((occurrence_date IS NULL OR template_id IS NOT NULL)
      AND (created_by_kind = 'system') = (occurrence_date IS NOT NULL)
      AND (occurrence_date IS NULL OR owner_id IS NOT NULL)) NOT VALID;
CREATE UNIQUE INDEX parked_entry_occurrence_once ON ledger.parked_entry (tenant_id, template_id, occurrence_date) WHERE occurrence_date IS NOT NULL;
CREATE INDEX parked_entry_by_template ON ledger.parked_entry (tenant_id, template_id, created_at) WHERE template_id IS NOT NULL;
-- A person's parked entries, for the owner's list (owned_by=me).
CREATE INDEX parked_entry_by_owner ON ledger.parked_entry (tenant_id, owner_id, status, created_at) WHERE owner_id IS NOT NULL;

-- The template an entry was parked from, and its date, never change.
CREATE FUNCTION ledger.keep_parked_entry_template() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.template_id, NEW.occurrence_date) IS DISTINCT FROM (OLD.template_id, OLD.occurrence_date) THEN
    RAISE EXCEPTION 'the template parked entry % was parked from does not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.keep_parked_entry_template() OWNER TO erp_owner;
CREATE TRIGGER parked_entry_keep_template BEFORE UPDATE ON ledger.parked_entry
  FOR EACH ROW EXECUTE FUNCTION ledger.keep_parked_entry_template();
