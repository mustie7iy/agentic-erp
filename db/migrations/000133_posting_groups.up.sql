-- Postings copied to the ledgers of a company (T52b, ADR-0045; 03-data-model
-- §3.7). A posting is written once per ledger it goes to, as linked entries:
-- every copy names the first, the representative ledger's, as its posting
-- group (posting_group_id, the first's own id on the first) and carries
-- its document number; ledger_group_id is the ledger group the posting
-- named, if any. Expand only (06 §6.4): nullable columns, nothing
-- backfilled; entries posted before have no posting group and stand alone.
-- No foreign keys on the journal: PostgreSQL 17 cannot add one NOT VALID to
-- a partitioned table, so it would scan the journal under lock; the insert
-- trigger below holds the same rules for new entries.
ALTER TABLE ledger.journal_entry ADD COLUMN posting_group_id uuid, ADD COLUMN ledger_group_id uuid;

-- The ledgers a company keeps (ledger.company_ledger), or the leading
-- ledger alone for a company not yet assigned any: none are backfilled,
-- the next activation assigns them. Legal entity and ledger ids are unique
-- across tenants, so the answer is right under any role; row-level
-- security scopes the reads to the tenant.
CREATE FUNCTION ledger.company_keeps(entity uuid, book uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT EXISTS (SELECT FROM ledger.company_ledger c WHERE c.legal_entity_id = entity AND c.ledger_id = book)
    OR (NOT EXISTS (SELECT FROM ledger.company_ledger c WHERE c.legal_entity_id = entity)
      AND EXISTS (SELECT FROM ledger.ledger l WHERE l.id = book AND l.is_leading))
$$;

-- An entry goes only to a ledger its company keeps; to a member of the
-- ledger group it names; and, as a copy, with the header of its posting
-- group's first entry, in another ledger.
CREATE FUNCTION ledger.check_entry_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT ledger.company_keeps(NEW.legal_entity_id, NEW.ledger_id) THEN
    RAISE EXCEPTION 'legal entity % does not keep ledger %', NEW.legal_entity_id, NEW.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_entry_company_ledger';
  END IF;
  IF NEW.ledger_group_id IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.ledger_group_member m
      WHERE m.tenant_id = NEW.tenant_id AND m.group_id = NEW.ledger_group_id AND m.ledger_id = NEW.ledger_id) THEN
    RAISE EXCEPTION 'ledger % is not a member of ledger group %', NEW.ledger_id, NEW.ledger_group_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_entry_ledger_group_member';
  END IF;
  IF NEW.posting_group_id IS DISTINCT FROM NEW.id AND NEW.posting_group_id IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.journal_entry g
      WHERE g.tenant_id = NEW.tenant_id AND g.fiscal_year = NEW.fiscal_year AND g.id = NEW.posting_group_id AND g.posting_group_id = g.id
        AND g.ledger_id <> NEW.ledger_id
        AND (g.legal_entity_id, g.fiscal_period, g.document_type, g.document_number, g.document_date, g.posting_date, g.currency_tc, g.source_context)
          = (NEW.legal_entity_id, NEW.fiscal_period, NEW.document_type, NEW.document_number, NEW.document_date, NEW.posting_date, NEW.currency_tc, NEW.source_context)
        AND (g.ledger_group_id, g.source_document_id) IS NOT DISTINCT FROM (NEW.ledger_group_id, NEW.source_document_id)) THEN
    RAISE EXCEPTION 'journal entry % is no copy of the first entry % of its posting group', NEW.id, NEW.posting_group_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_entry_posting_group';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER journal_entry_ledger BEFORE INSERT ON ledger.journal_entry FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_ledger();

-- A parked entry posts as a posting does: to the company's ledgers, to
-- the ledger group it names (ledger_id is then its representative), or to
-- ledger_id alone when ledger_specific. Parked entries of before posted to
-- the ledger named or the leading one; with one ledger per company, as
-- every company had, all its ledgers is the same.
ALTER TABLE ledger.parked_entry ADD COLUMN ledger_group_id uuid, ADD COLUMN ledger_specific boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT parked_entry_ledger_group_fkey FOREIGN KEY (tenant_id, ledger_group_id) REFERENCES ledger.ledger_group (tenant_id, id),
  ADD CONSTRAINT parked_entry_ledger_or_group CHECK (NOT (ledger_specific AND ledger_group_id IS NOT NULL));

CREATE FUNCTION ledger.check_parked_entry_ledgers() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.ledger_group_id, NEW.ledger_specific) IS DISTINCT FROM (OLD.ledger_group_id, OLD.ledger_specific) THEN
    RAISE EXCEPTION 'the ledgers parked entry % posts to do not change', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'parked_entry_ledgers_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER parked_entry_ledgers_fixed BEFORE UPDATE ON ledger.parked_entry FOR EACH ROW EXECUTE FUNCTION ledger.check_parked_entry_ledgers();
