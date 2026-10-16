-- Manual journal entries with approval (T35, 03-data-model §3.7). A manual
-- entry is parked before it is posted: :post posts it at once within the
-- company's manual_entry_approval_limit and otherwise opens a manual_entry
-- approval request (workflow context), posting it when that is approved.
-- The journal stays insert-only (ADR-0006): a parked entry is a working
-- document with its own lifecycle, and the journal entry it becomes names
-- it as its source document. That the request was approved is checked by
-- the ledger's approvals callback, which posts under the guard
-- approval_request_id = the approved request: the rules stay within the
-- ledger's schema (02-architecture, boundary rule 1). Agents may park; only
-- a person owns, posts or withdraws an entry, so the requester an approval
-- excludes is the one person who wrote it.
CREATE TABLE ledger.parked_entry (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,                 -- tenancy context
  ledger_id            uuid NOT NULL,
  document_type        text NOT NULL CHECK (document_type ~ '^[A-Z0-9]{2}$'),
  document_date        date NOT NULL,
  posting_date         date NOT NULL,
  currency             char(3) NOT NULL REFERENCES ref.currency,
  reference            text,
  header_text          text,
  status               text NOT NULL DEFAULT 'parked'
                       CHECK (status IN ('parked', 'pending_approval', 'posted', 'rejected', 'cancelled')),
  created_by_kind      text NOT NULL CHECK (created_by_kind IN ('human', 'agent', 'service')),
  owner_id             uuid,                          -- identity context: the one person who changes, posts or withdraws it
  approval_request_id  uuid,                          -- workflow context
  journal_fiscal_year  smallint,
  journal_entry_id     uuid,
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id),
  FOREIGN KEY (tenant_id, journal_fiscal_year, journal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  CHECK ((journal_entry_id IS NULL) = (journal_fiscal_year IS NULL)),
  CONSTRAINT parked_entry_journal_when_posted CHECK ((status = 'posted') = (journal_entry_id IS NOT NULL)),
  -- Posted on approval it keeps its request; posted directly it has none.
  CONSTRAINT parked_entry_request_when_decided CHECK (CASE status
      WHEN 'pending_approval' THEN approval_request_id IS NOT NULL
      WHEN 'rejected' THEN approval_request_id IS NOT NULL
      WHEN 'posted' THEN true
      ELSE approval_request_id IS NULL END),
  CONSTRAINT parked_entry_person_owns CHECK ((created_by_kind <> 'human' OR owner_id = created_by)
      AND (status IN ('parked', 'cancelled') OR owner_id IS NOT NULL))
);
CREATE INDEX parked_entry_by_status ON ledger.parked_entry (tenant_id, legal_entity_id, status, created_at);
CREATE TRIGGER parked_entry_touch BEFORE UPDATE ON ledger.parked_entry FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Lines in minor units of the entry's currency, debit positive. Edits
-- replace them without deletes (is_removed, as in 000015), and only while
-- the entry is parked.
CREATE TABLE ledger.parked_entry_line (
  tenant_id        uuid NOT NULL,
  parked_entry_id  uuid NOT NULL,
  line_no          integer NOT NULL CHECK (line_no > 0),
  account_id       uuid NOT NULL,
  amount           bigint NOT NULL CHECK (amount <> 0),
  partner_id       uuid,                              -- partner context
  tax_code         text,
  quantity         numeric(20,6),
  uom_code         text REFERENCES ref.unit_of_measure,
  line_text        text,
  is_removed       boolean NOT NULL DEFAULT false,
  PRIMARY KEY (tenant_id, parked_entry_id, line_no),
  FOREIGN KEY (tenant_id, parked_entry_id) REFERENCES ledger.parked_entry (tenant_id, id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES ledger.gl_account (tenant_id, id),
  CHECK ((quantity IS NULL) = (uom_code IS NULL))
);
CREATE TRIGGER parked_entry_line_editable BEFORE INSERT OR UPDATE ON ledger.parked_entry_line
  FOR EACH ROW EXECUTE FUNCTION platform.check_editable_parent('ledger.parked_entry', 'parked_entry_id', 'parked');

-- An entry is parked first and then moves only along its lifecycle; what is
-- approved is what is posted, an entry is decided only on the request it
-- names, its owner never changes once a person owns it, and it is posted as
-- the journal entry posted for it (not that entry's reversal, which names
-- the same source document).
CREATE FUNCTION ledger.check_parked_entry_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'parked' THEN
      RAISE EXCEPTION 'an entry is parked first, not %', NEW.status USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) NOT IN (('parked', 'parked'), ('parked', 'pending_approval'), ('parked', 'posted'), ('parked', 'cancelled'),
      ('pending_approval', 'posted'), ('pending_approval', 'rejected'), ('pending_approval', 'parked')) THEN
    RAISE EXCEPTION 'parked entry % cannot go from % to %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'check_violation';
  END IF;
  IF (OLD.status, NEW.status) <> ('parked', 'parked') AND
     (NEW.legal_entity_id, NEW.ledger_id, NEW.document_type, NEW.document_date, NEW.posting_date, NEW.currency, NEW.reference, NEW.header_text)
     IS DISTINCT FROM
     (OLD.legal_entity_id, OLD.ledger_id, OLD.document_type, OLD.document_date, OLD.posting_date, OLD.currency, OLD.reference, OLD.header_text) THEN
    RAISE EXCEPTION 'parked entry % changes only while it is parked', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.status = 'pending_approval' AND NEW.status <> 'parked' AND NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id THEN
    RAISE EXCEPTION 'parked entry % is decided only on the request it names', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.owner_id IS NOT NULL AND NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
    RAISE EXCEPTION 'the owner of parked entry % does not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.journal_entry_id IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.journal_entry j
      WHERE j.tenant_id = NEW.tenant_id AND j.fiscal_year = NEW.journal_fiscal_year AND j.id = NEW.journal_entry_id
        AND j.source_context = 'ledger' AND j.source_document_id = NEW.id AND j.reversal_of_id IS NULL) THEN
    RAISE EXCEPTION 'journal entry % was not posted for parked entry %', NEW.journal_entry_id, OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_parked_entry_change() OWNER TO erp_owner;
CREATE TRIGGER parked_entry_change BEFORE INSERT OR UPDATE ON ledger.parked_entry
  FOR EACH ROW EXECUTE FUNCTION ledger.check_parked_entry_change();

SELECT platform.secure_table('ledger.parked_entry'::regclass);
SELECT platform.secure_table('ledger.parked_entry_line'::regclass);
GRANT UPDATE ON ledger.parked_entry_line TO erp_app; -- no version column, so secure_table grants none

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.entry.park', 'Park manual journal entries for a person to post', false),
  ('ledger.entry.approve', 'Approve manual journal entries above the approval limit', true);
