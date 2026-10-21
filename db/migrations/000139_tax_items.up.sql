-- Tax items (T61c, ADR-0049 §4-§5): with every entry that carries a tax
-- code, in the same transaction, one item per tax code and component (and
-- tax line account): the tax date, the rate applied, the base and the tax
-- in transaction and local currency and the recoverable part. Returns read
-- items, never journal lines. Insert-only like the journal (ADR-0006),
-- but for reported_in, which the return that reports an item sets once
-- (T61g). Expand and migrate (06-engineering-process §6.4): the items of
-- the entries posted before are written here from their lines, and the
-- check that a rate is not re-rated reads items from now on.

-- The tax date decides the rate and the return period; the posting date
-- keeps deciding the fiscal period. Entries posted before have none: their
-- codes were valid on their posting date, which their items carry.
ALTER TABLE ledger.journal_entry ADD COLUMN tax_date date;
ALTER TABLE ledger.parked_entry ADD COLUMN tax_date date;

CREATE TABLE ledger.tax_item (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  item_no           integer NOT NULL CHECK (item_no > 0),
  legal_entity_id   uuid NOT NULL,                 -- the entry's
  code              text NOT NULL,
  component         text NOT NULL,
  jurisdiction      text,
  direction         text NOT NULL CHECK (direction IN ('input', 'output')),
  kind              text NOT NULL CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import')),
  account_key       text NOT NULL CHECK (account_key IN ('input', 'output', 'non_deductible', 'self_output', 'self_input')),
  account_id        uuid,                          -- the tax line's account; none for a non-deductible part (T61d)
  tax_date          date NOT NULL,
  tax_rate_id       uuid NOT NULL,                 -- the rate applied
  rate_pct          numeric(9,6) NOT NULL,
  recoverable_pct   numeric(6,3) NOT NULL CHECK (recoverable_pct >= 0 AND recoverable_pct <= 100),
  base_tc           bigint NOT NULL,
  tax_tc            bigint NOT NULL,
  recoverable_tc    bigint NOT NULL,
  base_lc           bigint NOT NULL,
  tax_lc            bigint NOT NULL,
  recoverable_lc    bigint NOT NULL,
  reported_in       uuid,                          -- the return that reported the item (T61g), set once
  created_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, item_no),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  FOREIGN KEY (tenant_id, legal_entity_id, code, component) REFERENCES ledger.tax_component (tenant_id, legal_entity_id, code, component),
  FOREIGN KEY (tenant_id, tax_rate_id) REFERENCES ledger.tax_rate (tenant_id, id),
  -- Output tax is owed, not recovered; input tax is recovered in part,
  -- of the tax's sign and at most it.
  CONSTRAINT tax_item_recoverable CHECK (CASE WHEN account_key IN ('output', 'self_output') THEN recoverable_tc = 0 AND recoverable_lc = 0
    ELSE recoverable_tc * sign(tax_tc) BETWEEN 0 AND abs(tax_tc) AND recoverable_lc * sign(tax_lc) BETWEEN 0 AND abs(tax_lc)
      AND (tax_tc <> 0 OR recoverable_tc = 0) AND (tax_lc <> 0 OR recoverable_lc = 0) END)
);
-- What a return selects (T61g) and the rate check below.
CREATE INDEX tax_item_by_code ON ledger.tax_item (tenant_id, legal_entity_id, code, tax_date);
SELECT platform.secure_table('ledger.tax_item'::regclass);
GRANT UPDATE (reported_in) ON ledger.tax_item TO erp_app; -- no version column, so secure_table grants none

-- An item never changes but for reported_in, set once; none is deleted.
CREATE FUNCTION ledger.check_tax_item_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.reported_in IS NOT NULL OR NEW.reported_in IS NULL
     OR to_jsonb(NEW) - 'reported_in' IS DISTINCT FROM to_jsonb(OLD) - 'reported_in' THEN
    RAISE EXCEPTION 'tax item % of entry % is fixed but for the return that reports it, set once', OLD.item_no, OLD.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_tax_item_fixed() OWNER TO erp_owner;
CREATE TRIGGER tax_item_fixed BEFORE UPDATE OR DELETE ON ledger.tax_item FOR EACH ROW EXECUTE FUNCTION ledger.check_tax_item_fixed();

-- Writes the items of entry e from its lines, at tax date day: one per
-- component with a rate on that day of each tax code its lines carry (and
-- per tax line account), the tax the lines on the component's account with
-- the code, the base the lines with the code on no account of the code's
-- components (once per component: on its first account's item). The rate is the component's on that day; the recoverable
-- part the tax times the component's share, rounded half away from zero.
-- A non-deductible component has no tax line of its own and is not
-- written (T61d). former: a component's former accounts are its tax lines'
-- too (the migration of entries posted before, T61a); the application
-- posts on the current ones. Returns the number of items written.
CREATE FUNCTION ledger.write_tax_items(t uuid, y smallint, e uuid, day date, former boolean) RETURNS integer
LANGUAGE sql AS $$
  WITH lines AS (
    SELECT l.legal_entity_id, l.account_id, l.tax_code, l.amount_tc, l.amount_lc FROM ledger.journal_line l
    WHERE l.tenant_id = t AND l.fiscal_year = y AND l.journal_entry_id = e AND l.tax_code IS NOT NULL
  ), codes AS (
    SELECT DISTINCT legal_entity_id, tax_code FROM lines
  ), accounts AS (  -- every account a component of a code posts its tax to
    SELECT c.code, a FROM ledger.tax_component c JOIN codes ON codes.legal_entity_id = c.legal_entity_id AND codes.tax_code = c.code
      CROSS JOIN LATERAL unnest(CASE WHEN former THEN c.former_account_ids || c.account_id ELSE ARRAY[c.account_id] END) a
    WHERE c.tenant_id = t AND a IS NOT NULL
  ), base AS (
    SELECT l.tax_code, sum(l.amount_tc)::bigint tc, sum(l.amount_lc)::bigint lc FROM lines l
    WHERE NOT EXISTS (SELECT FROM accounts x WHERE x.code = l.tax_code AND x.a = l.account_id) GROUP BY 1
  ), comps AS (
    SELECT c.legal_entity_id, c.code, c.component, c.jurisdiction, c.account_key, c.recoverable_pct, c.account_id, c.former_account_ids,
      d.direction, d.kind, r.id rate_id, r.rate_pct
    FROM ledger.tax_component c JOIN codes ON codes.legal_entity_id = c.legal_entity_id AND codes.tax_code = c.code
    JOIN ledger.tax_code_definition d ON d.tenant_id = c.tenant_id AND d.legal_entity_id = c.legal_entity_id AND d.code = c.code
    JOIN ledger.tax_rate r ON r.tenant_id = c.tenant_id AND r.legal_entity_id = c.legal_entity_id AND r.code = c.code AND r.component = c.component
      AND r.withdrawn_at IS NULL AND r.valid_from <= day AND (r.valid_to IS NULL OR r.valid_to > day)
    WHERE c.tenant_id = t AND c.account_id IS NOT NULL
  ), taxed AS (  -- each component's tax per account it has lines on, or its account with none
    SELECT c.*, coalesce(s.account_id, c.account_id) tax_account, coalesce(s.tc, 0) tax_tc, coalesce(s.lc, 0) tax_lc FROM comps c
    LEFT JOIN LATERAL (SELECT l.account_id, sum(l.amount_tc)::bigint tc, sum(l.amount_lc)::bigint lc FROM lines l
      WHERE l.tax_code = c.code AND (l.account_id = c.account_id OR (former AND l.account_id = ANY(c.former_account_ids))) GROUP BY 1) s ON true
  ), ins AS (
    INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, jurisdiction, direction, kind,
      account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
    SELECT t, y, e, row_number() OVER (ORDER BY x.code, x.component, x.tax_account), x.legal_entity_id, x.code, x.component, x.jurisdiction,
      x.direction, x.kind, x.account_key, x.tax_account, day, x.rate_id, x.rate_pct, x.recoverable_pct,
      -- The code's base once per component, on its first account's item.
      CASE WHEN row_number() OVER (PARTITION BY x.code, x.component ORDER BY x.tax_account) = 1 THEN coalesce(b.tc, 0) ELSE 0 END, x.tax_tc,
      CASE WHEN x.account_key IN ('output', 'self_output') THEN 0 ELSE round(x.tax_tc * x.recoverable_pct / 100)::bigint END,
      CASE WHEN row_number() OVER (PARTITION BY x.code, x.component ORDER BY x.tax_account) = 1 THEN coalesce(b.lc, 0) ELSE 0 END, x.tax_lc,
      CASE WHEN x.account_key IN ('output', 'self_output') THEN 0 ELSE round(x.tax_lc * x.recoverable_pct / 100)::bigint END
    FROM taxed x LEFT JOIN base b ON b.tax_code = x.code
    RETURNING 1
  )
  SELECT count(*)::integer FROM ins
$$;
ALTER FUNCTION ledger.write_tax_items(uuid, smallint, uuid, date, boolean) OWNER TO erp_owner;

-- The items of the entries posted before T61c, from their lines at their
-- posting date, the date their codes were validated on; former accounts
-- count, since a component's account may have changed since (T61a).
SET LOCAL row_security = off;
SELECT ledger.write_tax_items(e.tenant_id, e.fiscal_year, e.id, e.posting_date, true)
FROM ledger.journal_entry e
WHERE EXISTS (SELECT FROM ledger.journal_line l WHERE l.tenant_id = e.tenant_id AND l.fiscal_year = e.fiscal_year AND l.journal_entry_id = e.id
  AND l.tax_code IS NOT NULL)
  -- Tax items are the leading ledger's (ADR-0045): a parallel ledger's
  -- copy, or entry, has none.
  AND EXISTS (SELECT FROM ledger.ledger g WHERE g.tenant_id = e.tenant_id AND g.id = e.ledger_id AND g.is_leading);
RESET row_security;

-- An entry of the leading ledger and its items agree when it commits (a
-- parallel ledger's has none): each tax code its lines carry that posts to
-- an account has an item, the items are of its legal entity, and per
-- account and code the lines on an item's account or on a component's
-- account add up to the items' tax, in transaction and local currency.
-- Checked when the entry or an item is written, deferred like the balance.
CREATE FUNCTION ledger.check_tax_items(t uuid, y smallint, e uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  v_le uuid;
  v_leading boolean;
  v_code text;
  v_account uuid;
BEGIN
  SELECT j.legal_entity_id, g.is_leading INTO v_le, v_leading FROM ledger.journal_entry j
    JOIN ledger.ledger g ON g.tenant_id = j.tenant_id AND g.id = j.ledger_id WHERE j.tenant_id = t AND j.fiscal_year = y AND j.id = e;
  -- Tax items are the leading ledger's (ADR-0045, ADR-0049 §5): returns are
  -- statutory and read it; a parallel ledger's copy carries the tax lines
  -- and no items, and an entry of a parallel ledger has none either.
  IF NOT v_leading THEN
    IF EXISTS (SELECT FROM ledger.tax_item i WHERE i.tenant_id = t AND i.fiscal_year = y AND i.journal_entry_id = e) THEN
      RAISE EXCEPTION 'journal entry % is not the leading ledger''s: it has no tax items', e
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_leading';
    END IF;
    RETURN;
  END IF;
  -- A code whose components post to no account (all non-deductible) has
  -- no tax line and no item until T61d.
  SELECT l.tax_code INTO v_code FROM ledger.journal_line l WHERE l.tenant_id = t AND l.fiscal_year = y AND l.journal_entry_id = e
    AND l.tax_code IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.tax_item i WHERE i.tenant_id = t AND i.fiscal_year = y
      AND i.journal_entry_id = e AND i.code = l.tax_code)
    AND EXISTS (SELECT FROM ledger.tax_component c WHERE c.tenant_id = t AND c.legal_entity_id = v_le AND c.code = l.tax_code AND c.account_id IS NOT NULL)
    LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'journal entry % carries tax code % without its tax item', e, v_code USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_missing';
  END IF;
  IF EXISTS (SELECT FROM ledger.tax_item i WHERE i.tenant_id = t AND i.fiscal_year = y AND i.journal_entry_id = e AND i.legal_entity_id <> v_le) THEN
    RAISE EXCEPTION 'journal entry % has tax items of another legal entity', e USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_entity';
  END IF;
  WITH l AS (
    SELECT l.account_id, l.tax_code code, sum(l.amount_tc) tc, sum(l.amount_lc) lc FROM ledger.journal_line l
    WHERE l.tenant_id = t AND l.fiscal_year = y AND l.journal_entry_id = e AND l.tax_code IS NOT NULL GROUP BY 1, 2
  ), i AS (
    SELECT i.account_id, i.code, sum(i.tax_tc) tc, sum(i.tax_lc) lc FROM ledger.tax_item i
    WHERE i.tenant_id = t AND i.fiscal_year = y AND i.journal_entry_id = e AND i.account_id IS NOT NULL GROUP BY 1, 2
  ), k AS (
    SELECT account_id, code FROM i
    UNION SELECT c.account_id, c.code FROM ledger.tax_component c WHERE c.tenant_id = t AND c.legal_entity_id = v_le AND c.account_id IS NOT NULL
      AND c.code IN (SELECT l.code FROM l)
  )
  SELECT k.account_id, k.code INTO v_account, v_code FROM k
  LEFT JOIN l ON l.account_id = k.account_id AND l.code = k.code LEFT JOIN i ON i.account_id = k.account_id AND i.code = k.code
  WHERE (coalesce(l.tc, 0), coalesce(l.lc, 0)) IS DISTINCT FROM (coalesce(i.tc, 0), coalesce(i.lc, 0)) LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'journal entry %: the tax lines on account % with tax code % differ from its tax items', e, v_account, v_code
      USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_item_lines';
  END IF;
END $$;
ALTER FUNCTION ledger.check_tax_items(uuid, smallint, uuid) OWNER TO erp_owner;

CREATE FUNCTION ledger.check_entry_tax_items() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- The argument names the table: a partition's trigger names the
  -- partition, not ledger.journal_entry.
  IF TG_ARGV[0] = 'entry' THEN
    -- An entry without a tax date was posted by an instance of the previous
    -- release during the rolling upgrade, which writes no items: it is not
    -- held to them, and a later release writes them (06 §6.4).
    IF NEW.tax_date IS NOT NULL THEN
      PERFORM ledger.check_tax_items(NEW.tenant_id, NEW.fiscal_year, NEW.id);
    END IF;
  ELSE
    PERFORM ledger.check_tax_items(NEW.tenant_id, NEW.fiscal_year, NEW.journal_entry_id);
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION ledger.check_entry_tax_items() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER journal_entry_tax_items AFTER INSERT ON ledger.journal_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_tax_items('entry');
-- An item written for an entry committed before is checked as well.
CREATE CONSTRAINT TRIGGER tax_item_checked AFTER INSERT ON ledger.tax_item
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_tax_items('item');

-- A rate is used once an item of its code has a tax date within its dates
-- (T61a's check, ledger.check_tax_rate, reads this): the items name the
-- tax date each posting applied its rate on, where journal lines gave only
-- the posting date.
CREATE OR REPLACE FUNCTION ledger.tax_code_posted(t uuid, le uuid, c text, lo date, hi date) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT lo < coalesce(hi, 'infinity'::date) AND EXISTS (SELECT FROM ledger.tax_item i WHERE i.tenant_id = t AND i.legal_entity_id = le
    AND i.code = c AND i.tax_date >= lo AND (hi IS NULL OR i.tax_date < hi))
$$;
