-- Intercompany matching and eliminations (T68f; ADR-0059 §5, §8;
-- 03-data-model §3.24): a run's intercompany differences, per
-- elimination and pair of its companies, and per company the rows on an
-- elimination's accounts naming a company outside the run or none; and
-- its elimination entries, each line on a group account, which sum to
-- zero per entry. Written while the run runs and never changed or removed
-- after: accepting a difference is T68g's, which adds its columns and its
-- one transition. The trading partners and companies are tenancy's legal
-- entities, the accounts the ledger's: no foreign key leaves the schema
-- (02 §2.1). Expand only: new tables, which the previous version neither
-- reads nor writes.

-- A run's intercompany differences (§8). A pair (matched,
-- within_tolerance, open) is two companies of the run, the company's code
-- before the partner's, with both sides' amounts on the elimination's
-- accounts, their sum and the elimination's tolerance; outside_group is a
-- company's rows naming a trading partner that is not a company of the
-- run, no_trading_partner its rows naming none, each with its amount
-- alone. line_no follows the elimination's code, the company's code and
-- the partner's code (none first), so two runs of the same figures number
-- and list them alike (§5). Amounts are the group currency's minor units,
-- debit positive.
CREATE TABLE consolidation.run_difference (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  run_id            uuid NOT NULL,
  line_no           integer NOT NULL CHECK (line_no >= 1),
  elimination_code  text NOT NULL CHECK (elimination_code ~ '^[A-Z0-9_]{1,30}$'),
  legal_entity_id   uuid NOT NULL,
  partner_id        uuid,                 -- tenancy context
  status            text NOT NULL CHECK (status IN ('matched', 'within_tolerance', 'open', 'outside_group', 'no_trading_partner')),
  amount            bigint NOT NULL,
  partner_amount    bigint,
  difference        bigint,
  tolerance         bigint CHECK (tolerance >= 0),
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, run_id, line_no),
  UNIQUE NULLS NOT DISTINCT (tenant_id, run_id, elimination_code, legal_entity_id, partner_id),
  FOREIGN KEY (tenant_id, run_id, legal_entity_id) REFERENCES consolidation.run_member (tenant_id, run_id, legal_entity_id),
  CONSTRAINT run_difference_partner CHECK (partner_id IS DISTINCT FROM legal_entity_id AND (status = 'no_trading_partner') = (partner_id IS NULL)),
  -- A pair has both sides, their sum and the tolerance; the others the
  -- company's amount alone.
  CONSTRAINT run_difference_pair CHECK (CASE WHEN status IN ('matched', 'within_tolerance', 'open')
    THEN partner_amount IS NOT NULL AND tolerance IS NOT NULL AND difference IS NOT DISTINCT FROM amount + partner_amount
    ELSE partner_amount IS NULL AND difference IS NULL AND tolerance IS NULL END),
  -- The status is the difference's against the tolerance.
  CONSTRAINT run_difference_status CHECK (CASE status WHEN 'matched' THEN difference = 0
    WHEN 'within_tolerance' THEN difference <> 0 AND abs(difference) <= tolerance WHEN 'open' THEN abs(difference) > tolerance ELSE true END)
);
-- The differences list's filter by status.
CREATE INDEX run_difference_by_status ON consolidation.run_difference (tenant_id, run_id, status);
SELECT platform.secure_table('consolidation.run_difference'::regclass);

-- A run's elimination entries (§8, §9): intercompany entries per
-- elimination and pair (T68f), the investment and non-controlling
-- interest entries per member (T68h). entry_no follows the kind, then the
-- elimination's code and the pair's two company codes (§5).
CREATE TABLE consolidation.run_entry (
  tenant_id         uuid NOT NULL,
  run_id            uuid NOT NULL,
  entry_no          integer NOT NULL CHECK (entry_no >= 1),
  kind              text NOT NULL CHECK (kind IN ('intercompany', 'investment', 'nci')),
  legal_entity_id   uuid NOT NULL,
  partner_id        uuid,
  elimination_code  text CHECK (elimination_code ~ '^[A-Z0-9_]{1,30}$'),
  PRIMARY KEY (tenant_id, run_id, entry_no),
  FOREIGN KEY (tenant_id, run_id, legal_entity_id) REFERENCES consolidation.run_member (tenant_id, run_id, legal_entity_id),
  FOREIGN KEY (tenant_id, run_id, partner_id) REFERENCES consolidation.run_member (tenant_id, run_id, legal_entity_id),
  -- An intercompany entry is of an elimination and a pair of the run's
  -- companies; the others of a member alone.
  CONSTRAINT run_entry_pair CHECK ((kind = 'intercompany') = (partner_id IS NOT NULL) AND (kind = 'intercompany') = (elimination_code IS NOT NULL)
    AND partner_id IS DISTINCT FROM legal_entity_id)
);
-- One intercompany entry per elimination and pair.
CREATE UNIQUE INDEX run_entry_one_per_pair ON consolidation.run_entry (tenant_id, run_id, elimination_code, legal_entity_id, partner_id)
  WHERE kind = 'intercompany';
SELECT platform.secure_table('consolidation.run_entry'::regclass);

-- An entry's lines (§8): each on a group account, with the company, the
-- company account and the trading partner of the translated row it
-- reverses, or none of them (an intercompany difference's line, which is
-- the pair's); never zero. line_no follows the group account's number,
-- the company's code (none first) and the reversed row's line, which
-- follows the company account's number and the trading partner's code
-- (§5).
CREATE TABLE consolidation.run_entry_line (
  tenant_id           uuid NOT NULL,
  run_id              uuid NOT NULL,
  entry_no            integer NOT NULL,
  line_no             integer NOT NULL CHECK (line_no >= 1),
  group_account       text NOT NULL CHECK (group_account ~ '^[0-9A-Z]{1,10}$'),
  legal_entity_id     uuid,
  account_id          uuid,                -- ledger context
  trading_partner_id  uuid,                -- tenancy context
  amount_gc           bigint NOT NULL CHECK (amount_gc <> 0),
  PRIMARY KEY (tenant_id, run_id, entry_no, line_no),
  FOREIGN KEY (tenant_id, run_id, entry_no) REFERENCES consolidation.run_entry (tenant_id, run_id, entry_no),
  FOREIGN KEY (tenant_id, run_id, legal_entity_id) REFERENCES consolidation.run_member (tenant_id, run_id, legal_entity_id),
  CONSTRAINT run_entry_line_company CHECK ((account_id IS NULL OR legal_entity_id IS NOT NULL) AND (trading_partner_id IS NULL OR legal_entity_id IS NOT NULL)
    AND (trading_partner_id IS NULL OR trading_partner_id <> legal_entity_id))
);
-- The statements' eliminations per group account.
CREATE INDEX run_entry_line_by_group_account ON consolidation.run_entry_line (tenant_id, run_id, group_account);
SELECT platform.secure_table('consolidation.run_entry_line'::regclass);

CREATE TRIGGER run_difference_while_running BEFORE INSERT ON consolidation.run_difference
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_rows();
CREATE TRIGGER run_entry_while_running BEFORE INSERT ON consolidation.run_entry
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_rows();
CREATE TRIGGER run_entry_line_while_running BEFORE INSERT ON consolidation.run_entry_line
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_rows();

-- At commit, every entry has at least two lines and they sum to zero, as
-- a journal entry does (ledger.check_entry_balanced).
CREATE FUNCTION consolidation.check_entry_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  n integer;
  total bigint;
BEGIN
  SELECT count(*), coalesce(sum(amount_gc), 0) INTO n, total FROM consolidation.run_entry_line
  WHERE tenant_id = NEW.tenant_id AND run_id = NEW.run_id AND entry_no = NEW.entry_no;
  IF n < 2 OR total <> 0 THEN
    RAISE EXCEPTION 'entry % of consolidation run % has % line(s) summing to %; at least two summing to zero are required', NEW.entry_no, NEW.run_id, n, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_entry_balanced';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION consolidation.check_entry_balanced() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER run_entry_balanced AFTER INSERT ON consolidation.run_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION consolidation.check_entry_balanced();
