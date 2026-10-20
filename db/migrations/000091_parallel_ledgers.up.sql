-- Parallel ledgers (T52a, ADR-0045; 03-data-model §3.7): the ledgers a
-- company keeps (SAP's ledger assignment to company codes), exactly one of
-- them the leading ledger 0L, and ledger groups (FAGL ledger groups) with a
-- representative ledger. All of it is configuration, applied by
-- ledger.Setup from the approved document; nothing here is backfilled: a
-- company is assigned its ledgers by the next activation (expand only,
-- 06 §6.4).

-- The leading ledger is 0L and 0L is the leading ledger. Existing rows are
-- all 0L and leading (ledger.Setup created them so); the check is added NOT
-- VALID and validated by the contract migration of T52e, new rows are
-- checked from here on. A ledger's name may change through configuration;
-- its code, principle and role never do (only name and version are granted).
ALTER TABLE ledger.ledger ADD CONSTRAINT ledger_leading_is_0l CHECK (is_leading = (code = '0L')) NOT VALID;
ALTER TABLE ledger.ledger ADD COLUMN version integer NOT NULL DEFAULT 1;
ALTER TABLE ledger.ledger ADD CONSTRAINT ledger_role_key UNIQUE (tenant_id, id, is_leading);
GRANT UPDATE (name, version) ON ledger.ledger TO erp_app;

-- The ledgers of a company (legal entity, tenancy context: no foreign key
-- across contexts). is_leading repeats the ledger's, kept equal by the
-- foreign key, so the database can hold a company to one leading ledger.
-- An assignment is never taken back: the ledger's postings and periods
-- stay the company's.
CREATE TABLE ledger.company_ledger (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  ledger_id        uuid NOT NULL,
  is_leading       boolean NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, ledger_id),
  CONSTRAINT company_ledger_ledger_fkey FOREIGN KEY (tenant_id, ledger_id, is_leading) REFERENCES ledger.ledger (tenant_id, id, is_leading)
);
CREATE UNIQUE INDEX company_ledger_one_leading ON ledger.company_ledger (tenant_id, legal_entity_id) WHERE is_leading;
CREATE INDEX company_ledger_ledger ON ledger.company_ledger (tenant_id, ledger_id);
CREATE TRIGGER company_ledger_immutable BEFORE UPDATE OR DELETE ON ledger.company_ledger FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- At commit, a company with ledgers has its leading one: the index above
-- allows at most one, this at least one.
CREATE FUNCTION ledger.check_company_has_leading() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT FROM ledger.company_ledger c WHERE c.tenant_id = NEW.tenant_id AND c.legal_entity_id = NEW.legal_entity_id AND c.is_leading) THEN
    RAISE EXCEPTION 'legal entity % has ledgers but not the leading ledger', NEW.legal_entity_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'company_ledger_leading_required';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER company_ledger_leading_required AFTER INSERT ON ledger.company_ledger
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_company_has_leading();

-- A ledger group names the ledgers a posting goes to; its representative
-- ledger decides the posting period of the group's postings (T52b). Only
-- the name changes; members are never taken out and the representative
-- never changes, so a posting's group keeps meaning what it meant.
CREATE TABLE ledger.ledger_group (
  tenant_id                 uuid NOT NULL,
  id                        uuid NOT NULL,
  code                      text NOT NULL CONSTRAINT ledger_group_code_check CHECK (code ~ '^[0-9A-Z]{1,4}$'),
  name                      text NOT NULL CONSTRAINT ledger_group_name_check CHECK (length(trim(name)) BETWEEN 1 AND 200),
  representative_ledger_id  uuid NOT NULL,
  created_at                timestamptz NOT NULL DEFAULT now(),
  created_by                uuid NOT NULL,
  updated_at                timestamptz NOT NULL DEFAULT now(),
  updated_by                uuid NOT NULL,
  version                   integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT ledger_group_code_key UNIQUE (tenant_id, code)
);
CREATE TRIGGER ledger_group_touch BEFORE UPDATE ON ledger.ledger_group FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE FUNCTION ledger.check_ledger_group_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.tenant_id, NEW.id, NEW.code, NEW.representative_ledger_id, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.code, OLD.representative_ledger_id, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'only the name of a ledger group changes' USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_group_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER ledger_group_fixed BEFORE UPDATE ON ledger.ledger_group FOR EACH ROW EXECUTE FUNCTION ledger.check_ledger_group_update();

CREATE TABLE ledger.ledger_group_member (
  tenant_id   uuid NOT NULL,
  group_id    uuid NOT NULL,
  ledger_id   uuid NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, group_id, ledger_id),
  CONSTRAINT ledger_group_member_group_fkey FOREIGN KEY (tenant_id, group_id) REFERENCES ledger.ledger_group (tenant_id, id),
  CONSTRAINT ledger_group_member_ledger_fkey FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id)
);
CREATE INDEX ledger_group_member_ledger ON ledger.ledger_group_member (tenant_id, ledger_id);
CREATE TRIGGER ledger_group_member_immutable BEFORE UPDATE OR DELETE ON ledger.ledger_group_member FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- The representative is a member, checked at commit since the group and
-- its members are inserted together.
ALTER TABLE ledger.ledger_group ADD CONSTRAINT ledger_group_representative_member FOREIGN KEY (tenant_id, id, representative_ledger_id)
  REFERENCES ledger.ledger_group_member (tenant_id, group_id, ledger_id) DEFERRABLE INITIALLY DEFERRED;

-- A group with the leading ledger has it as its representative (SAP's
-- rule): the leading ledger's periods decide its postings.
CREATE FUNCTION ledger.check_group_representative() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM ledger.ledger_group g
      JOIN ledger.ledger r ON r.tenant_id = g.tenant_id AND r.id = g.representative_ledger_id
      JOIN ledger.ledger_group_member m ON m.tenant_id = g.tenant_id AND m.group_id = g.id
      JOIN ledger.ledger l ON l.tenant_id = m.tenant_id AND l.id = m.ledger_id
      WHERE g.tenant_id = NEW.tenant_id AND g.id = NEW.group_id AND l.is_leading AND NOT r.is_leading) THEN
    RAISE EXCEPTION 'ledger group % has the leading ledger, which must be its representative', NEW.group_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_group_leading_represents';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER ledger_group_leading_represents AFTER INSERT ON ledger.ledger_group_member
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_group_representative();

SELECT platform.secure_table('ledger.company_ledger'::regclass);
SELECT platform.secure_table('ledger.ledger_group'::regclass);
SELECT platform.secure_table('ledger.ledger_group_member'::regclass);
